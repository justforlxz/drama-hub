package vfs

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/store"
)

// testSecret 是 fixture 的 stream_secret，满足 store 的 ≥32 字节校验。
const testSecret = "0123456789abcdef0123456789abcdef"

// stubID 是 IDFunc 替身：把入参编码进结果，证明 id 来自注入缝（HMAC 属 stream）。
func stubID(secret, uid string) string { return "id(" + secret + ":" + uid + ")" }

// 固定 fixture 的剧名。titleB 带 Windows 非法字符 ':'，用于验证 sanitize 贯穿全树。
const (
	titleA = "测试剧"
	titleB = "另一部:剧"
	// genreA 是剧 A 的分类；剧 B 不声明分类，落兜底目录「其他」。
	genreA = "爱情"
)

// shardOf 独立实现目录分片号 fnv32a(provider_id) % N（不复用 store.ShardOf，
// 避免「用被测代码验证被测代码」）。
func shardOf(providerID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(providerID))
	return fmt.Sprintf("%02x", h.Sum32()%store.ShardCount)
}

// dramaDirPath 独立拼出剧目录的 vfs 路径：短剧/<分类>/<分片>/<剧目录>。
func dramaDirPath(genre, providerID, dir string) string {
	g := genre
	if g == "" {
		g = otherGenre
	}
	return Root + "/" + g + "/" + shardOf(providerID) + "/" + dir
}

// fixture 是一套两剧三集的数据：剧 A（有分类）有「有标签」与「无标签」两集，
// 剧 B（无分类）无集。
type fixture struct {
	tree   *Tree
	db     *store.DB
	epA1   store.Episode
	epA2   store.Episode
	dirA   string // 剧 A 的 vfs 目录路径
	dirB   string // 剧 B 的 vfs 目录路径
	shardA string // 剧 A 所在分片目录名
}

// seed 用真实临时库（WAL / FK / busy_timeout 由 store.Open 施加）建 fixture。
func seed(t *testing.T, strmPrefix string) fixture {
	t.Helper()
	return seedTree(t, strmPrefix, true)
}

// seedNoSecret 同 seed，但库中从未写入 stream_secret：验证缺密钥路径时
// 无需在测试里执行裸 SQL（SQL 只允许出现在 internal/store）。
func seedNoSecret(t *testing.T) fixture {
	t.Helper()
	return seedTree(t, "", false)
}

// seedTree 建 fixture；withSecret=false 时不写 stream_secret。
func seedTree(t *testing.T, strmPrefix string, withSecret bool) fixture {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "vfs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)

	aID, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "web", ProviderID: "a1", Title: titleA, Year: 2024,
		Plot: "第一行 & <第二行>", EpisodeTotal: 2, Genre: genreA,
	})
	if err != nil {
		t.Fatalf("UpsertDrama A: %v", err)
	}
	if _, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "app", ProviderID: "b1", Title: titleB, Year: 0,
	}); err != nil {
		t.Fatalf("UpsertDrama B: %v", err)
	}

	ep1 := store.Episode{DramaID: aID, EpNo: 1, Title: "开局", DurationSec: 125, Source: "web", Vid: "v1"}
	ep2 := store.Episode{DramaID: aID, EpNo: 2, Title: "", DurationSec: 59, Source: "web", Vid: "v2"}
	ep1.ID, err = st.UpsertEpisode(ctx, ep1)
	if err != nil {
		t.Fatalf("UpsertEpisode 1: %v", err)
	}
	ep2.ID, err = st.UpsertEpisode(ctx, ep2)
	if err != nil {
		t.Fatalf("UpsertEpisode 2: %v", err)
	}
	// 派生表是 vfs 的唯一数据源（硬约束 6）：seed 后必须重建一次，等价于
	// 「同步完成」那个生产调用点。
	if _, err := st.RebuildEmbyItems(ctx, aID); err != nil {
		t.Fatalf("RebuildEmbyItems A: %v", err)
	}

	if withSecret {
		if err := settings.Set(ctx, store.KeyStreamSecret, testSecret); err != nil {
			t.Fatalf("Set stream_secret: %v", err)
		}
	}
	if strmPrefix != "" {
		if err := settings.Set(ctx, store.KeyStrmPrefix, strmPrefix); err != nil {
			t.Fatalf("Set strm_prefix: %v", err)
		}
	}

	return fixture{
		tree:   NewTree(st, settings, stubID, slog.New(slog.DiscardHandler)),
		db:     db,
		epA1:   ep1,
		epA2:   ep2,
		dirA:   dramaDirPath(genreA, "a1", titleA+" (2024)"),
		dirB:   dramaDirPath("", "b1", "另一部_剧 (0)"),
		shardA: shardOf("a1"),
	}
}

// store 返回 fixture 库上的 Store（测试里追加数据用）。
func (f fixture) store() *store.Store { return store.NewStore(f.db.DB) }

// addDrama 往库里加一部剧，返回主键。
func (f fixture) addDrama(t *testing.T, d store.Drama) int64 {
	t.Helper()
	id, err := f.store().UpsertDrama(context.Background(), d)
	if err != nil {
		t.Fatalf("UpsertDrama %s/%s: %v", d.Provider, d.ProviderID, err)
	}
	return id
}

// walk 递归列举整棵树（只用于断言「一部剧只出现一次」「没有 全部/」这类全局性质）。
func (f fixture) walk(t *testing.T) []Node {
	t.Helper()
	ctx := context.Background()
	var out []Node
	queue := []string{""}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		nodes, err := f.tree.List(ctx, dir)
		if err != nil {
			t.Fatalf("List(%q): %v", dir, err)
		}
		for _, n := range nodes {
			out = append(out, n)
			if n.IsDir {
				queue = append(queue, n.Path)
			}
		}
		if len(out) > 100000 {
			t.Fatalf("遍历节点数爆炸（>100000）：树里可能有环")
		}
	}
	return out
}

// AC：根目录只有分类目录（没有 全部/），且分类按字节序。
func TestListRoot_OnlyGenres(t *testing.T) {
	f := seed(t, "")
	nodes, err := f.tree.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List(根): %v", err)
	}
	want := []Node{
		{Path: Root + "/" + otherGenre, IsDir: true},
		{Path: Root + "/" + genreA, IsDir: true},
	}
	if len(nodes) != len(want) {
		t.Fatalf("根目录子项 = %v, 期望 %v", nodes, want)
	}
	for i := range want {
		if nodes[i] != want[i] {
			t.Errorf("节点[%d] = %+v, 期望 %+v", i, nodes[i], want[i])
		}
	}
	for _, n := range nodes {
		if strings.Contains(n.Path, "全部") {
			t.Errorf("树里不该有 全部/ 节点：%s", n.Path)
		}
	}
}

// AC：分类目录只有分片目录；"短剧" 与 "" 等价（都是根）。
func TestListGenre_OnlyShards(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()

	byRoot, err := f.tree.List(ctx, Root)
	if err != nil {
		t.Fatalf("List(短剧): %v", err)
	}
	byEmpty, err := f.tree.List(ctx, "")
	if err != nil {
		t.Fatalf("List(\"\"): %v", err)
	}
	if len(byRoot) != len(byEmpty) {
		t.Fatalf("List(短剧) 与 List(\"\") 不一致：%v vs %v", byRoot, byEmpty)
	}

	// 分类目录的子项是分片的全集 00–7f（docs/DIRECTORY-CONTRACT.md 允许「直接回
	// 00–7f」）：分片集合是代码常量，查库只会把一个 O(分类规模) 的 DISTINCT 扫描
	// 挂到每次列举上。空分片是空目录，列出它不发生查询。
	nodes, err := f.tree.List(ctx, Root+"/"+genreA)
	if err != nil {
		t.Fatalf("List(分类): %v", err)
	}
	if len(nodes) != store.ShardCount {
		t.Fatalf("分类目录子项 = %d, 期望 %d（00–%02x）", len(nodes), store.ShardCount, store.ShardCount-1)
	}
	for i, n := range nodes {
		if want := Root + "/" + genreA + "/" + fmt.Sprintf("%02x", i); n.Path != want || !n.IsDir {
			t.Fatalf("分片目录[%d] = %+v, 期望 %s（目录）", i, n, want)
		}
	}
	if nodes[0].Path != Root+"/"+genreA+"/00" || nodes[store.ShardCount-1].Path != Root+"/"+genreA+"/7f" {
		t.Errorf("分片目录首尾 = %q … %q, 期望 …/00 … …/7f", nodes[0].Path, nodes[store.ShardCount-1].Path)
	}

	// 空分片目录存在但没有子项（不是 ErrNotFound）。
	empty := Root + "/" + genreA + "/" + differentShard(f.shardA)
	if kids, err := f.tree.List(ctx, empty); err != nil || len(kids) != 0 {
		t.Errorf("List(空分片 %s) = %v, %v, 期望空列表、无错误", empty, kids, err)
	}
}

// AC：分片目录只有剧目录（按目录名排序）。
func TestListShard_OnlyDramaDirs(t *testing.T) {
	f := seed(t, "")
	nodes, err := f.tree.List(context.Background(), Root+"/"+genreA+"/"+f.shardA)
	if err != nil {
		t.Fatalf("List(分片): %v", err)
	}
	if len(nodes) != 1 || nodes[0].Path != f.dirA || !nodes[0].IsDir {
		t.Fatalf("分片目录子项 = %v, 期望只有 %s", nodes, f.dirA)
	}
}

// AC：剧目录只有 tvshow.nfo 与 Season 01；季目录只有该剧的文件对。
func TestListDramaAndSeason(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()

	nodes, err := f.tree.List(ctx, f.dirA)
	if err != nil {
		t.Fatalf("List(剧目录): %v", err)
	}
	want := []Node{
		{Path: f.dirA + "/tvshow.nfo"},
		{Path: f.dirA + "/" + seasonDir, IsDir: true},
	}
	if len(nodes) != len(want) {
		t.Fatalf("剧目录子项 = %v, 期望 %v", nodes, want)
	}
	for i := range want {
		if nodes[i] != want[i] {
			t.Errorf("节点[%d] = %+v, 期望 %+v", i, nodes[i], want[i])
		}
	}

	season := f.dirA + "/" + seasonDir
	nodes, err = f.tree.List(ctx, season)
	if err != nil {
		t.Fatalf("List(季目录): %v", err)
	}
	want = []Node{
		{Path: season + "/" + titleA + " - S01E01 - 开局.strm"},
		{Path: season + "/" + titleA + " - S01E01 - 开局.nfo"},
		{Path: season + "/" + titleA + " - S01E02 - 第2集.strm"},
		{Path: season + "/" + titleA + " - S01E02 - 第2集.nfo"},
	}
	if len(nodes) != len(want) {
		t.Fatalf("季目录子项 = %v, 期望 %v", nodes, want)
	}
	for i := range want {
		if nodes[i] != want[i] {
			t.Errorf("节点[%d] = %+v, 期望 %+v", i, nodes[i], want[i])
		}
	}
}

// AC：不存在的目录一律 ErrNotFound（表驱动）。
func TestListNotFound(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	paths := []string{
		"其他/某种剧 (2024)",             // 不在 短剧/ 下
		Root + "/不存在",               // 分类不存在
		Root + "/" + genreA + "/ff", // 分片越界（N=128，最大 7f）
		Root + "/" + genreA + "/zz", // 分片不是十六进制
		Root + "/" + genreA + "/" + f.shardA + "/不存在 (2024)",
		Root + "/" + genreA + "/" + f.shardA + "/Season 01", // 段数不足
		f.dirA + "/tvshow.nfo",                              // 文件当目录
		f.dirA + "/Season 02",                               // 季目录名错
		f.dirA + "/Season 01/x",                             // 季目录下的未知文件
		Root + "/" + genreA + "/" + f.shardA + "/" + titleA + " (2024)/" + seasonDir + "/x",
		"/" + Root + "/" + Root,
	}
	for _, p := range paths {
		if nodes, err := f.tree.List(ctx, p); !errors.Is(err, ErrNotFound) {
			t.Errorf("List(%q) = %v, %v, 期望 ErrNotFound", p, nodes, err)
		}
	}
}

// AC：一部剧只出现一次；树里没有 全部/ 节点。
func TestTreeEachDramaOnce_NoAllNode(t *testing.T) {
	f := seed(t, "")
	nodes := f.walk(t)

	dramaDirs := map[string]int{}
	for _, n := range nodes {
		if !n.IsDir {
			continue
		}
		segs := strings.Split(n.Path, "/")
		if len(segs) == 4 { // 短剧/<分类>/<分片>/<剧目录>
			dramaDirs[n.Path]++
		}
		for _, s := range segs {
			if s == "全部" {
				t.Errorf("树里出现 全部/ 节点：%s", n.Path)
			}
		}
	}
	if len(dramaDirs) != 2 {
		t.Fatalf("剧目录数 = %d, 期望 2：%v", len(dramaDirs), dramaDirs)
	}
	for path, n := range dramaDirs {
		if n != 1 {
			t.Errorf("剧目录 %s 出现 %d 次，期望 1", path, n)
		}
	}
	if _, ok := dramaDirs[f.dirA]; !ok {
		t.Errorf("缺剧 A 的目录 %s：%v", f.dirA, dramaDirs)
	}
	if _, ok := dramaDirs[f.dirB]; !ok {
		t.Errorf("缺剧 B 的目录 %s（兜底分类）：%v", f.dirB, dramaDirs)
	}

	// 文件也对得上：A 的 tvshow + 2 集 ×2，B 的 tvshow。
	files := 0
	for _, n := range nodes {
		if !n.IsDir {
			files++
		}
	}
	if want := 6; files != want {
		t.Errorf("文件数 = %d, 期望 %d", files, want)
	}
}

// AC：分类缺失落兜底名；上游真有同名分类则合并（两个 genre 值一个目录）。
func TestGenreFallbackMergesUpstreamOther(t *testing.T) {
	f := seed(t, "") // 剧 B 的 genre 为空 → 其他
	// 上游真的有一个分类叫「其他」，且与剧 B 落在同一个分片才能并进一个目录断言列表。
	upstream := f.addDrama(t, store.Drama{
		Provider: "web", ProviderID: "b1", Title: "上游的其他分类剧", Year: 2025, Genre: otherGenre,
	})
	_ = upstream

	nodes, err := f.tree.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List(根): %v", err)
	}
	other := 0
	for _, n := range nodes {
		if n.Path == Root+"/"+otherGenre {
			other++
		}
	}
	if other != 1 {
		t.Fatalf("兜底分类目录数 = %d, 期望 1（空 genre 与上游「其他」合并）：%v", other, nodes)
	}

	// 两个 genre 值的剧都出现在「其他」分支下，各自一次。
	dirs := map[string]int{}
	for _, n := range f.walk(t) {
		if n.IsDir && strings.Count(n.Path, "/") == 3 && strings.HasPrefix(n.Path, Root+"/"+otherGenre+"/") {
			dirs[n.Path]++
		}
	}
	if dirs[f.dirB] != 1 {
		t.Errorf("空分类的剧没有出现在 其他/ 下：%v", dirs)
	}
	upstreamDir := dramaDirPath(otherGenre, "b1", "上游的其他分类剧 (2025)")
	if dirs[upstreamDir] != 1 {
		t.Errorf("上游「其他」分类的剧没有并进同一目录：%v（期望含 %s）", dirs, upstreamDir)
	}
}

// AC（S8 / PL 2026-10-30 裁决）：含路径分隔符的分类名按 sanitizeName 转义，
// 转义后的目录「能列、能进、能取 .strm」——不再跳过。
//
// 依据：不转义时该分类无法成为一个路径段，硬拼会让 x/net/webdav 的 walkFS 在 Stat
// 时失败、整份 multistatus 被截断；「跳过 + warn」则让这批剧在挂载点里静默不可达。
func TestListGenresEscapesPathSeparatorGenre(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	dirty := f.addDrama(t, store.Drama{
		Provider: "web", ProviderID: "dirty1", Title: "斜杠分类剧", Year: 2026, Genre: "剧情/古装",
	})
	// 反斜杠是另一个脏字符，转义后与上面同名：两个分类并进同一个目录。
	f.addDrama(t, store.Drama{
		Provider: "web", ProviderID: "dirty2", Title: "反斜杠分类剧", Year: 2026, Genre: `剧情\古装`,
	})
	epID, err := f.store().UpsertEpisode(ctx, store.Episode{DramaID: dirty, EpNo: 1, Source: "web", Vid: "v1"})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}
	if _, err := f.store().RebuildEmbyItems(ctx, dirty); err != nil {
		t.Fatalf("RebuildEmbyItems: %v", err)
	}

	buf := &bytes.Buffer{}
	tree := NewTree(store.NewStore(f.db.DB), store.NewSettings(f.db.DB), stubID,
		slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// 1) 根目录能列出转义后的分类，且每个子项仍是一个路径段。
	nodes, err := tree.List(ctx, "")
	if err != nil {
		t.Fatalf("List(根): %v", err)
	}
	got := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		got[n.Path] = true
		if strings.Contains(strings.TrimPrefix(n.Path, Root+"/"), "/") {
			t.Errorf("根目录子项含路径分隔符（会把父目录列举打挂）：%q", n.Path)
		}
	}
	escaped := Root + "/剧情_古装"
	if !got[escaped] {
		t.Fatalf("转义后的分类目录 %s 不在根目录里：%v", escaped, nodes)
	}

	// 2) 目录能进：分片层能列。
	shardNodes, err := tree.List(ctx, escaped)
	if err != nil {
		t.Fatalf("List(%s): %v", escaped, err)
	}
	if len(shardNodes) != store.ShardCount {
		t.Errorf("分片目录数 = %d, 期望 %d", len(shardNodes), store.ShardCount)
	}

	// 3) 该分类下的剧真的在这个目录里（两部脏分类都并进来）。
	shardDir := shardOf("dirty1")
	dirs, err := tree.List(ctx, escaped+"/"+shardDir)
	if err != nil {
		t.Fatalf("List(%s): %v", escaped+"/"+shardDir, err)
	}
	want := escaped + "/" + shardDir + "/斜杠分类剧 (2026)"
	found := false
	for _, n := range dirs {
		if n.Path == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("转义后的分类目录里没有这部剧：%v（期望含 %s）", dirs, want)
	}

	// 4) .strm 能取：内容仍是 {strm_prefix}/video/{id}.m3u8。
	body, err := tree.Render(ctx, want+"/Season 01/斜杠分类剧 - S01E01 - 第1集.strm")
	if err != nil {
		t.Fatalf("Render(.strm): %v", err)
	}
	wantStrm := "/video/" + stubID(testSecret, "e"+strconv.FormatInt(epID, 10)) + ".m3u8\n"
	if string(body) != wantStrm {
		t.Errorf("strm 内容 = %q, 期望 %q", body, wantStrm)
	}

	// 5) 转义后同名的两个分类合并时留一条 debug 日志（脏数据可追溯）。
	if logged := buf.String(); !strings.Contains(logged, "剧情") {
		t.Errorf("转义合并没有留日志：%q", logged)
	}
}

// AC：同名冲突全部加后缀（同分片内所有冲突成员都改），不冲突的保持基准名。
func TestConflictSuffix(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	// 三个同名同年的剧：前两个落在同一个分片（冲突），第三个落在别的分片。
	shardA := shardOf("c1")
	sameShard := providerIDsWithShard(shardA, "web", 2)
	other := providerIDsWithShard(differentShard(shardA), "web", 1)[0]

	for _, id := range sameShard {
		f.addDrama(t, store.Drama{Provider: "web", ProviderID: id, Title: "同名剧", Year: 2026, Genre: genreA})
	}
	f.addDrama(t, store.Drama{Provider: "web", ProviderID: other, Title: "同名剧", Year: 2026, Genre: genreA})

	nodes, err := f.tree.List(ctx, Root+"/"+genreA+"/"+shardA)
	if err != nil {
		t.Fatalf("List(冲突分片): %v", err)
	}
	got := map[string]bool{}
	for _, n := range nodes {
		got[baseOfPath(n.Path)] = true
	}
	for _, id := range sameShard {
		want := "同名剧 (2026) [web-" + id + "]"
		if !got[want] {
			t.Errorf("缺冲突成员的带后缀目录 %q：%v", want, keysOf(got))
		}
	}
	if got["同名剧 (2026)"] {
		t.Errorf("冲突成员不该保留基准名：%v", keysOf(got))
	}

	// 另一个分片里的同名剧不构成冲突，保持基准名。
	otherShard := shardOf(other)
	nodes, err = f.tree.List(ctx, Root+"/"+genreA+"/"+otherShard)
	if err != nil {
		t.Fatalf("List(另一分片): %v", err)
	}
	found := false
	for _, n := range nodes {
		if baseOfPath(n.Path) == "同名剧 (2026)" {
			found = true
		}
	}
	if !found {
		t.Errorf("不同分片的同名剧应保持基准名：%v", nodes)
	}
}

// AC：60 万规模不截断——一个分片里 1200 个同名剧全部出节点、全部加后缀、后缀不截断。
func TestConflictSuffixNoTruncationAtScale(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	const n = 1200
	shard := shardOf("scale-0")
	ids := providerIDsWithShard(shard, "web", n)
	if len(ids) != n {
		t.Fatalf("只造出 %d 个同分片身份，期望 %d", len(ids), n)
	}

	// 一个事务里批量写：这条测试要的是「列举不截断」，不是写路径的性能。
	err := f.store().InTx(ctx, func(tx *store.Store) error {
		for _, id := range ids {
			if _, err := tx.UpsertDrama(ctx, store.Drama{
				Provider: "web", ProviderID: id, Title: "批量同名剧", Year: 2027, Genre: genreA,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("批量写入: %v", err)
	}

	nodes, err := f.tree.List(ctx, Root+"/"+genreA+"/"+shard)
	if err != nil {
		t.Fatalf("List(分片): %v", err)
	}
	if len(nodes) != n {
		t.Fatalf("分片目录子项 = %d, 期望 %d（截断即此处失败）", len(nodes), n)
	}
	seen := map[string]bool{}
	for _, node := range nodes {
		dir := baseOfPath(node.Path)
		if seen[dir] {
			t.Fatalf("目录名重复：%s", dir)
		}
		seen[dir] = true
		if !strings.HasPrefix(dir, "批量同名剧 (2027) [web-") || !strings.HasSuffix(dir, "]") {
			t.Fatalf("目录名不是「基准名 + 全量后缀」形态：%s", dir)
		}
	}
	// 后缀必须是 provider_id 原文：随机取一个完整身份反查。
	if !seen["批量同名剧 (2027) [web-"+ids[n-1]+"]"] {
		t.Errorf("缺完整后缀目录 批量同名剧 (2027) [web-%s]", ids[n-1])
	}
}

// providerIDsWithShard 造 n 个落在指定分片的 provider_id。
//
// 用独立实现的 fnv（本文件 shardOf），因此这条断言同时钉住「分片是纯函数且
// 稳定」：同一个 id 永远同一个分片。
func providerIDsWithShard(shard, provider string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; len(out) < n; i++ {
		id := provider + "-" + strconv.Itoa(i)
		if shardOf(id) == shard {
			out = append(out, id)
		}
	}
	return out
}

// differentShard 返回一个与给定分片不同的分片目录名。
func differentShard(shard string) string {
	if shard == "00" {
		return "01"
	}
	return "00"
}

// baseOfPath 取路径最后一段。
func baseOfPath(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// keysOf 取 map 的键集合（用于失败信息）。
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// AC2：命名是纯函数（表驱动）。
func TestNames(t *testing.T) {
	drama := func(title string, year int) store.Drama {
		return store.Drama{Title: title, Year: year}
	}
	single := func(epNo int, title string) store.EmbyItem {
		return store.EmbyItem{Kind: "single", EpNo: epNo, Season: 1, Title: title, EpFrom: epNo, EpTo: epNo}
	}
	merged := func(epNo, from, to int) store.EmbyItem {
		return store.EmbyItem{Kind: "merged", EpNo: epNo, Season: 1, EpFrom: from, EpTo: to}
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"剧目录/正常", dramaDir(drama("你好世界", 2024)), "你好世界 (2024)"},
		{"剧目录/空年显式化为0", dramaDir(drama("无年份", 0)), "无年份 (0)"},
		{"剧目录/非法字符替换", dramaDir(drama(`a/b\c:d*e?f"g<h>i|j`, 2021)), "a_b_c_d_e_f_g_h_i_j (2021)"},
		{"冲突后缀/全量身份", conflictDir(store.Drama{Provider: "web", ProviderID: "7503745441910017049", Title: "同名", Year: 2026}),
			"同名 (2026) [web-7503745441910017049]"},
		{"集号/一位补零", embyNumber(single(1, "开局")), "S01E01"},
		{"集号/两位原样", embyNumber(single(12, "")), "S01E12"},
		{"集号/超两位展开", embyNumber(single(100, "")), "S01E100"},
		{"标签/有标题", embyLabel(single(1, "开局")), "开局"},
		{"标签/空标题回退", embyLabel(single(7, "")), "第7集"},
		{"标签/标题里的非法字符也替换", embyLabel(single(1, "a/b")), "a_b"},
		{"标签/合并集范围", embyLabel(merged(1, 1, 10)), "第1-10集"},
		{"标签/合并集单集退化为第N集", embyLabel(merged(1, 3, 3)), "第3集"},
		{"文件名主干/有标签", embyBase(drama("测试剧", 2024), single(1, "开局")), "测试剧 - S01E01 - 开局"},
		{"文件名主干/无标签", embyBase(drama("测试剧", 2024), single(2, "")), "测试剧 - S01E02 - 第2集"},
		{"文件名主干/合并集", embyBase(drama("测试剧", 2024), merged(1, 1, 10)), "测试剧 - S01E01 - 第1-10集"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// 分类目录名与分片目录名是纯函数（表驱动）：空 genre 落兜底名，分片两位十六进制。
func TestDirNames(t *testing.T) {
	if got, want := genreDirName(""), otherGenre; got != want {
		t.Errorf("genreDirName(\"\") = %q, 期望 %q", got, want)
	}
	if got, want := genreDirName("爱情"), "爱情"; got != want {
		t.Errorf("genreDirName(爱情) = %q, 期望 %q", got, want)
	}
	for shard, want := range map[int]string{0: "00", 10: "0a", 127: "7f"} {
		if got := shardDirName(shard); got != want {
			t.Errorf("shardDirName(%d) = %q, 期望 %q", shard, got, want)
		}
		// 目录名 → 分片号必须可逆（Render 靠它定位分片）。
		if got, ok := parseShard(want); !ok || got != shard {
			t.Errorf("parseShard(%q) = %d, %v, 期望 %d", want, got, ok, shard)
		}
	}
	for _, bad := range []string{"", "zz", "80", "ff", "100"} {
		if _, ok := parseShard(bad); ok {
			t.Errorf("parseShard(%q) 应失败（N=128，合法范围 00–7f）", bad)
		}
	}
}

// AC2：sanitizeName 表驱动。
func TestSanitizeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"干净的名字 2024", "干净的名字 2024"},
		{`/`, `_`},
		{`\`, `_`},
		{`:`, `_`},
		{`*`, `_`},
		{`?`, `_`},
		{`"`, `_`},
		{`<`, `_`},
		{`>`, `_`},
		{`|`, `_`},
		{`a/b\c:d*e?f"g<h>i|j`, `a_b_c_d_e_f_g_h_i_j`},
		{"", ""},
	}
	for _, tt := range tests {
		if got := sanitizeName(tt.in); got != tt.want {
			t.Errorf("sanitizeName(%q) = %q, 期望 %q", tt.in, got, tt.want)
		}
	}
}

// AC7：非文件路径一律 ErrNotFound（表驱动）。
func TestRenderNotFound(t *testing.T) {
	f := seed(t, "")
	dir := f.dirA
	season := dir + "/" + seasonDir

	paths := []string{
		"",
		"/",
		Root,
		dir,    // 剧目录当文件
		season, // 季目录当文件
		"其他/" + titleA + " (2024)/tvshow.nfo",                           // 不在 短剧/ 下
		Root + "/" + genreA + "/" + f.shardA + "/不存在 (2024)/tvshow.nfo", // 剧不存在
		dir + "/tvshow.txt",   // 剧级扩展名非法
		dir + "/tvshow.nfo/x", // 段数多余
		dir + "/Season 02/" + titleA + " - S01E01 - 开局.strm", // 季目录错
		season + "/" + titleA + " - S01E99 - 第99集.strm",      // 集不存在
		season + "/" + titleA + " - S01E01 - 开局.mp4",         // 集扩展名非法
		season + "/" + titleA + " - S01E01 - 开局",             // 无扩展名
		season + "/tvshow.nfo",                               // 季目录下的 tvshow.nfo 不是合法形状
		season + "/.strm",                                    // 空主干
		Root + "/" + genreA + "/" + differentShard(f.shardA) + "/" + titleA + " (2024)/tvshow.nfo", // 分片不符
		Root + "/" + otherGenre + "/" + f.shardA + "/" + titleA + " (2024)/tvshow.nfo",             // 分类不符
	}
	for _, p := range paths {
		body, err := f.tree.Render(context.Background(), p)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Render(%q) err = %v, 期望 ErrNotFound", p, err)
		}
		if body != nil {
			t.Errorf("Render(%q) 返回了 %d 字节, 期望 nil", p, len(body))
		}
	}
}

// AC（硬约束 6）：合并组在树里只产出 1 条文件对，标签是「第 A-B 集」，且 strm
// 的 id 里 uid 是合并集句柄 m<group_id>（不是任一单集 uid）。
func TestListMergedGroupCollapsesToOne(t *testing.T) {
	ctx := context.Background()
	f := seed(t, "")
	st := store.NewStore(f.db.DB)

	// 把 fixture 的两集并成 1 集。
	gid, err := st.CreateMergeGroup(ctx, 1, "第1-2集", []int64{f.epA1.ID, f.epA2.ID})
	if err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}
	items, err := st.ListEmbyItems(ctx, 1)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("合并后对外剧集数 = %d, 期望 1：%+v", len(items), items)
	}

	season := f.dirA + "/" + seasonDir
	base := titleA + " - S01E01 - 第1-2集"
	wantStrm := season + "/" + base + ".strm"
	wantNFO := season + "/" + base + ".nfo"

	nodes, err := f.tree.List(ctx, season)
	if err != nil {
		t.Fatalf("List(季目录): %v", err)
	}
	got := map[string]bool{}
	for _, n := range nodes {
		if !n.IsDir {
			got[n.Path] = true
		}
	}
	// 合并后季目录下只剩这一条文件对（其余节点不含集文件）。
	if len(got) != 2 || !got[wantStrm] || !got[wantNFO] {
		t.Fatalf("合并后季目录文件 = %v, 期望 %q 与 %q", got, wantStrm, wantNFO)
	}

	// strm 的 uid 是 m<group_id>，id 由注入的 IDFunc 计算。
	uid := "m" + strconv.FormatInt(gid, 10)
	body, err := f.tree.Render(ctx, wantStrm)
	if err != nil {
		t.Fatalf("Render(%s): %v", wantStrm, err)
	}
	if want := "/video/" + stubID(testSecret, uid) + ".m3u8\n"; string(body) != want {
		t.Errorf("合并集 strm = %q, 期望 %q", body, want)
	}

	// NFO 的 episode 号是对外集号 1，标题是「第1-2集」。
	nfo, err := f.tree.Render(ctx, wantNFO)
	if err != nil {
		t.Fatalf("Render(%s): %v", wantNFO, err)
	}
	var parsed episodeNFO
	if err := xml.Unmarshal(nfo, &parsed); err != nil {
		t.Fatalf("解析合并集 NFO: %v\n%s", err, nfo)
	}
	if parsed.Title != "第1-2集" {
		t.Errorf("合并集 NFO title = %q, 期望 第1-2集", parsed.Title)
	}
	if parsed.Episode != 1 {
		t.Errorf("合并集 NFO episode = %d, 期望 1（对外集号）", parsed.Episode)
	}
}

// countingStore 数 vfs 对 store 的每次方法调用：让「列举某一层到底查了什么」
// 变成可断言的事实（上一轮 Review 抓到的正是「新增了点查、但列举仍然全表扫」）。
type countingStore struct {
	inner Store
	calls map[string]int
}

func newCountingStore(inner Store) *countingStore {
	return &countingStore{inner: inner, calls: map[string]int{}}
}

func (c *countingStore) reset() { c.calls = map[string]int{} }

func (c *countingStore) count(method string) int { return c.calls[method] }

func (c *countingStore) ListGenres(ctx context.Context) ([]string, error) {
	c.calls["ListGenres"]++
	return c.inner.ListGenres(ctx)
}

func (c *countingStore) GenreExists(ctx context.Context, genre string) (bool, error) {
	c.calls["GenreExists"]++
	return c.inner.GenreExists(ctx, genre)
}

func (c *countingStore) ListDramasByShard(ctx context.Context, genres []string, shard int) ([]store.Drama, error) {
	c.calls["ListDramasByShard"]++
	return c.inner.ListDramasByShard(ctx, genres, shard)
}

func (c *countingStore) DramaByTitleYear(ctx context.Context, title string, year int) (store.Drama, error) {
	c.calls["DramaByTitleYear"]++
	return c.inner.DramaByTitleYear(ctx, title, year)
}

func (c *countingStore) DramaByProviderID(ctx context.Context, provider, providerID string) (store.Drama, error) {
	c.calls["DramaByProviderID"]++
	return c.inner.DramaByProviderID(ctx, provider, providerID)
}

func (c *countingStore) ListEmbyItems(ctx context.Context, dramaID int64) ([]store.EmbyItem, error) {
	c.calls["ListEmbyItems"]++
	return c.inner.ListEmbyItems(ctx, dramaID)
}

// AC（阻断 2 的回归）：只有根目录列举需要「全部分类」，其余层只做存在性判断。
//
// 这条数的是 **store 方法调用**（不是 SQL 文本）：上一轮把点查写成死代码、
// genresForDir 仍走 ListGenres，SQL 文本测试照样绿，只有数调用才能抓住。
func TestListDoesNotScanAllGenres(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	cs := newCountingStore(f.store())
	tree := NewTree(cs, store.NewSettings(f.db.DB), stubID, slog.New(slog.DiscardHandler))

	tests := []struct {
		name          string
		dir           string
		wantListGenre int // ListGenres 调用次数
		wantExists    int // GenreExists 调用次数下限
		wantTitle     int // DramaByTitleYear 调用次数下限
	}{
		{name: "根/必须拿全部分类", dir: "", wantListGenre: 1},
		{name: "分类层", dir: Root + "/" + genreA, wantExists: 1},
		{name: "分片层", dir: Root + "/" + genreA + "/" + f.shardA, wantExists: 1},
		{name: "剧目录", dir: f.dirA, wantTitle: 1},
		// 剧名被 sanitize 过的目录：title 点查不中 → 退到「本分片内比对」，
		// 分类解析走 GenreExists（绝不能退回全表 DISTINCT genre）。
		{name: "sanitize 过的剧目录", dir: f.dirB, wantExists: 1},
		{name: "季目录", dir: f.dirA + "/" + seasonDir, wantTitle: 1},
		{name: "兜底分类层", dir: Root + "/" + otherGenre, wantExists: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs.reset()
			if _, err := tree.List(ctx, tt.dir); err != nil {
				t.Fatalf("List(%q): %v", tt.dir, err)
			}
			if got := cs.count("ListGenres"); got != tt.wantListGenre {
				t.Errorf("List(%q) 调了 %d 次 ListGenres, 期望 %d（全表 DISTINCT genre 只属于根目录）",
					tt.dir, got, tt.wantListGenre)
			}
			if got := cs.count("GenreExists"); got < tt.wantExists {
				t.Errorf("List(%q) 调了 %d 次 GenreExists, 期望 ≥ %d（目录名要靠它判定存在）",
					tt.dir, got, tt.wantExists)
			}
			if got := cs.count("DramaByTitleYear"); got < tt.wantTitle {
				t.Errorf("List(%q) 调了 %d 次 DramaByTitleYear, 期望 ≥ %d（剧目点查）",
					tt.dir, got, tt.wantTitle)
			}
			// 兜底名要同时问两个取值（'' 与 上游同名分类），其余只问一个。
			if tt.dir == Root+"/"+otherGenre && cs.count("GenreExists") != 2 {
				t.Errorf("兜底分类层的 GenreExists 次数 = %d, 期望 2（'' 与 其他）", cs.count("GenreExists"))
			}
			// 分类/分片层的查询次数与库规模无关：不出现任何列举式扫描。
			if cs.count("ListDramasByShard") > 1 {
				t.Errorf("List(%q) 调了 %d 次 ListDramasByShard, 期望 ≤ 1", tt.dir, cs.count("ListDramasByShard"))
			}
		})
	}
}

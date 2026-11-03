package vfs

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/pgtest"
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

// dramaDirPath 独立拼出剧目录的 vfs 路径：短剧/<分类>/<剧目录>。
func dramaDirPath(genre, dir string) string {
	g := genre
	if g == "" {
		g = otherGenre
	}
	return Root + "/" + g + "/" + dir
}

// stubCoverer 是 Coverer 的替身：`has` 里有这个键就算「有封面」，字节就是值；
// `fails` 里的键模拟「回源失败」。
//
// 封面能力在 vfs 里只有两个用途（列出 poster.jpg、渲染它 / nfo 的 thumb），所以替身
// 只需要「有没有」与「给我字节」两个回答，不必碰缓存、HTTP 与数据库。
type stubCoverer struct {
	has   map[string][]byte
	fails map[string]bool
}

// coverKey 是 stubCoverer 的键：与生产实现的 artifact 键同形（provider/provider_id）。
func coverKey(provider, providerID string) string { return provider + "/" + providerID }

// HasCover 报告替身里有没有这部剧的封面。
func (s *stubCoverer) HasCover(_ context.Context, provider, providerID string) (bool, error) {
	_, ok := s.has[coverKey(provider, providerID)]
	return ok, nil
}

// Poster 返回替身里的封面字节；标了 fails 的键按「回源失败」报不可用。
func (s *stubCoverer) Poster(_ context.Context, provider, providerID, url string) ([]byte, bool, error) {
	k := coverKey(provider, providerID)
	if s.fails[k] || url == "" {
		return nil, false, nil
	}
	body, ok := s.has[k]
	if !ok {
		return nil, false, nil
	}
	return body, true, nil
}

// coverStub 是 fixture 里剧 A 的封面字节（JPEG 魔数 + 可断言的尾巴，内容不参与解析）。
var coverStub = []byte("\xff\xd8\xff\xe0 stub poster bytes")

// fixture 是一套两剧三集的数据：剧 A（有分类）有「有标签」与「无标签」两集，
// 剧 B（无分类）无集。默认两部剧都没有封面（poster.jpg 不出现）。
type fixture struct {
	tree   *Tree
	covers *stubCoverer
	db     *store.DB
	epA1   store.Episode
	epA2   store.Episode
	dirA   string // 剧 A 的 vfs 目录路径
	dirB   string // 剧 B 的 vfs 目录路径
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

	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	// 本文件钉的是目录/文件名/strm 形状，夹具按「每集一条对外剧集」布置（N=1）；
	// 自动分块本身在 internal/store 的 merge_test 里验。
	if err := settings.Set(ctx, store.KeyMergeChunkSize, "1"); err != nil {
		t.Fatalf("Set merge_chunk_size: %v", err)
	}

	aID, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "web", ProviderID: "a1", Title: titleA, Year: 2024,
		Plot: "第一行 & <第二行>", EpisodeTotal: 2, Genre: genreA,
		// 剧 A 声明了封面地址：poster.jpg 的按需回源要用它（替身只看 url 是否为空）。
		CoverURL: "https://cdn.example/a1.jpg",
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
	// 派生是读取时现算的（docs/adr/0014）：建好 episode 就能在树里看到。
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

	covers := &stubCoverer{has: map[string][]byte{}, fails: map[string]bool{}}
	return fixture{
		tree:   NewTree(st, settings, stubID, covers, slog.New(slog.DiscardHandler)),
		covers: covers,
		db:     db,
		epA1:   ep1,
		epA2:   ep2,
		dirA:   dramaDirPath(genreA, titleA+" (2024)"),
		dirB:   dramaDirPath("", "另一部_剧 (0)"),
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

// AC-1：分类目录只有剧目录（不再有分片层）；"短剧" 与 "" 等价（都是根）。
func TestListGenre_OnlyDramaDirs(t *testing.T) {
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

	nodes, err := f.tree.List(ctx, Root+"/"+genreA)
	if err != nil {
		t.Fatalf("List(分类): %v", err)
	}
	if len(nodes) != 1 || nodes[0].Path != f.dirA || !nodes[0].IsDir {
		t.Fatalf("分类目录子项 = %v, 期望只有剧目录 %s", nodes, f.dirA)
	}

	// 路径里不许出现两位十六进制段（已删除的中间目录层的形状）。
	hexSegment := regexp.MustCompile(`^[0-9a-f]{2}$`)
	for _, n := range f.walk(t) {
		for _, seg := range strings.Split(strings.TrimPrefix(n.Path, Root+"/"), "/") {
			if hexSegment.MatchString(seg) {
				t.Errorf("树里出现两位十六进制段 %q（分片层没删干净）：%s", seg, n.Path)
			}
		}
	}
}

// AC：剧目录是 tvshow.nfo + Season 01（无封面时没有 poster.jpg）；季目录只有该剧的文件对。
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

// MKACG-650 AC：有封面的剧，剧目录里有 poster.jpg；没有封面的剧目录一个节点都不多。
func TestListDramaDirPosterOnlyWithCover(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()

	// 剧 A 有一份本地封面（同步已本地化过）：目录里出现 poster.jpg。
	f.covers.has[coverKey("web", "a1")] = coverStub
	nodes, err := f.tree.List(ctx, f.dirA)
	if err != nil {
		t.Fatalf("List(剧 A 目录): %v", err)
	}
	want := []Node{
		{Path: f.dirA + "/tvshow.nfo"},
		{Path: f.dirA + "/poster.jpg"},
		{Path: f.dirA + "/" + seasonDir, IsDir: true},
	}
	if len(nodes) != len(want) {
		t.Fatalf("有封面的剧目录子项 = %v, 期望 %v", nodes, want)
	}
	for i := range want {
		if nodes[i] != want[i] {
			t.Errorf("节点[%d] = %+v, 期望 %+v", i, nodes[i], want[i])
		}
	}

	// 剧 B 没有封面（源未声明 / 同步时下载失败）：没有 poster.jpg，其余不变。
	nodes, err = f.tree.List(ctx, f.dirB)
	if err != nil {
		t.Fatalf("List(剧 B 目录): %v", err)
	}
	if len(nodes) != 2 || nodes[0].Path != f.dirB+"/tvshow.nfo" || nodes[1].Path != f.dirB+"/"+seasonDir {
		t.Fatalf("无封面的剧目录子项 = %v, 期望只有 tvshow.nfo 与 Season 01", nodes)
	}
}

// AC：不存在的目录一律 ErrNotFound（表驱动）。
func TestListNotFound(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	paths := []string{
		"其他/某种剧 (2024)",                     // 不在 短剧/ 下
		Root + "/不存在",                       // 分类不存在
		Root + "/" + genreA + "/不存在 (2024)", // 剧目录不存在
		Root + "/" + genreA + "/0a",         // 分片层已下线：两位十六进制就是普通剧名，不存在
		Root + "/" + genreA + "/Season 01",  // 段数不足：季目录不是剧目录
		f.dirA + "/tvshow.nfo",              // 文件当目录
		f.dirA + "/Season 02",               // 季目录名错
		f.dirA + "/Season 01/x",             // 季目录下的未知文件
		f.dirA + "/" + seasonDir + "/x",
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
		if len(segs) == 3 { // 短剧/<分类>/<剧目录>
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
	// 上游真的有一个分类叫「其他」：它与 genre='' 的剧并进同一个目录。
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
		if n.IsDir && strings.Count(n.Path, "/") == 2 && strings.HasPrefix(n.Path, Root+"/"+otherGenre+"/") {
			dirs[n.Path]++
		}
	}
	if dirs[f.dirB] != 1 {
		t.Errorf("空分类的剧没有出现在 其他/ 下：%v", dirs)
	}
	upstreamDir := dramaDirPath(otherGenre, "上游的其他分类剧 (2025)")
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
	buf := &bytes.Buffer{}
	tree := NewTree(store.NewStore(f.db.DB), store.NewSettings(f.db.DB), stubID, f.covers,
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

	// 2) 目录能进：两部脏分类的剧都并进这一个目录（转义后同名）。
	dirs, err := tree.List(ctx, escaped)
	if err != nil {
		t.Fatalf("List(%s): %v", escaped, err)
	}
	want := escaped + "/斜杠分类剧 (2026)"
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

	// 4) 转义后同名的两个分类合并时留一条 debug 日志（脏数据可追溯）。
	if logged := buf.String(); !strings.Contains(logged, "剧情") {
		t.Errorf("转义合并没有留日志：%q", logged)
	}
}

// AC-1：同名冲突全部加后缀（同一分类目录内的冲突成员都改），不冲突的保持基准名。
//
// 去掉分片层后父目录就是分类目录，所以「同名同年」在同一分类里**必定**冲突——
// 这正是 MKACG-651 契约里要求的必然变化（见 docs/DIRECTORY-CONTRACT.md）。
func TestConflictSuffix(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	ids := []string{"dup-a", "dup-b", "dup-c"}
	for _, id := range ids {
		f.addDrama(t, store.Drama{Provider: "web", ProviderID: id, Title: "同名剧", Year: 2026, Genre: genreA})
	}
	// 另一分类里的同名剧不构成冲突。
	f.addDrama(t, store.Drama{Provider: "web", ProviderID: "dup-other", Title: "同名剧", Year: 2026, Genre: "都市"})

	nodes, err := f.tree.List(ctx, Root+"/"+genreA)
	if err != nil {
		t.Fatalf("List(分类): %v", err)
	}
	got := map[string]bool{}
	for _, n := range nodes {
		got[baseOfPath(n.Path)] = true
	}
	for _, id := range ids {
		want := "同名剧 (2026) [web-" + id + "]"
		if !got[want] {
			t.Errorf("缺冲突成员的带后缀目录 %q：%v", want, keysOf(got))
		}
	}
	if got["同名剧 (2026)"] {
		t.Errorf("冲突成员不该保留基准名：%v", keysOf(got))
	}

	// 另一个分类里的同名剧不构成冲突，保持基准名。
	nodes, err = f.tree.List(ctx, Root+"/都市")
	if err != nil {
		t.Fatalf("List(另一分类): %v", err)
	}
	found := false
	for _, n := range nodes {
		if baseOfPath(n.Path) == "同名剧 (2026)" {
			found = true
		}
	}
	if !found {
		t.Errorf("不同分类的同名剧应保持基准名：%v", nodes)
	}
}

// AC：大目录不截断——一个分类里 1200 个同名剧全部出节点、全部加后缀、后缀不截断。
func TestConflictSuffixNoTruncationAtScale(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	const n = 1200
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, "scale-"+strconv.Itoa(i))
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

	nodes, err := f.tree.List(ctx, Root+"/"+genreA)
	if err != nil {
		t.Fatalf("List(分类): %v", err)
	}
	// seed 里已有一部剧 A，一起计入。
	if len(nodes) != n+1 {
		t.Fatalf("分类目录子项 = %d, 期望 %d（截断即此处失败）", len(nodes), n+1)
	}
	seen := map[string]bool{}
	for _, node := range nodes {
		dir := baseOfPath(node.Path)
		if seen[dir] {
			t.Fatalf("目录名重复：%s", dir)
		}
		seen[dir] = true
		if dir == titleA+" (2024)" {
			continue // seed 里的剧 A 与「批量同名剧」不同名，不参与冲突
		}
		if !strings.HasPrefix(dir, "批量同名剧 (2027) [web-") || !strings.HasSuffix(dir, "]") {
			t.Fatalf("目录名不是「基准名 + 全量后缀」形态：%s", dir)
		}
	}
	// 后缀必须是 provider_id 原文：随机取一个完整身份反查。
	if !seen["批量同名剧 (2027) [web-"+ids[n-1]+"]"] {
		t.Errorf("缺完整后缀目录 批量同名剧 (2027) [web-%s]", ids[n-1])
	}
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
	dramaDirOf := func(title string, year int) store.DramaDir {
		return store.DramaDir{Title: title, Year: year}
	}
	show := func(title string, year int) store.Drama {
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
		{"剧目录/正常", dramaDir(dramaDirOf("你好世界", 2024)), "你好世界 (2024)"},
		{"剧目录/空年显式化为0", dramaDir(dramaDirOf("无年份", 0)), "无年份 (0)"},
		{"剧目录/非法字符替换", dramaDir(dramaDirOf(`a/b\c:d*e?f"g<h>i|j`, 2021)), "a_b_c_d_e_f_g_h_i_j (2021)"},
		{"冲突后缀/全量身份", conflictDir(store.DramaDir{Provider: "web", ProviderID: "7503745441910017049", Title: "同名", Year: 2026}),
			"同名 (2026) [web-7503745441910017049]"},
		{"集号/一位补零", embyNumber(single(1, "开局")), "S01E01"},
		{"集号/两位原样", embyNumber(single(12, "")), "S01E12"},
		{"集号/超两位展开", embyNumber(single(100, "")), "S01E100"},
		{"标签/有标题", embyLabel(single(1, "开局")), "开局"},
		{"标签/空标题回退", embyLabel(single(7, "")), "第7集"},
		// 自动分块后单集块的对外集号与源集号可以不同：标签取源集号（MKACG-649 AC-4）。
		{"标签/单集块回退取源集号", embyLabel(store.EmbyItem{Kind: "single", Season: 1, EpNo: 3, EpFrom: 21, EpTo: 21}), "第21集"},
		{"标签/标题里的非法字符也替换", embyLabel(single(1, "a/b")), "a_b"},
		{"标签/合并集范围", embyLabel(merged(1, 1, 10)), "第1-10集"},
		{"标签/合并集单集退化为第N集", embyLabel(merged(1, 3, 3)), "第3集"},
		{"文件名主干/有标签", embyBase(show("测试剧", 2024), single(1, "开局")), "测试剧 - S01E01 - 开局"},
		{"文件名主干/无标签", embyBase(show("测试剧", 2024), single(2, "")), "测试剧 - S01E02 - 第2集"},
		{"文件名主干/单集块回退取源集号",
			embyBase(show("测试剧", 2024), store.EmbyItem{Kind: "single", Season: 1, EpNo: 3, EpFrom: 21, EpTo: 21}),
			"测试剧 - S01E03 - 第21集"},
		{"文件名主干/合并集", embyBase(show("测试剧", 2024), merged(1, 1, 10)), "测试剧 - S01E01 - 第1-10集"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// 分类目录名是纯函数（表驱动）：空 genre 落兜底名，脏分类按文件名规则转义。
func TestDirNames(t *testing.T) {
	tests := []struct{ genre, want string }{
		{"", otherGenre},
		{"爱情", "爱情"},
		{"剧情/古装", "剧情_古装"},
		{`剧情\古装`, "剧情_古装"},
	}
	for _, tt := range tests {
		if got := genreDir(tt.genre); got != tt.want {
			t.Errorf("genreDir(%q) = %q, 期望 %q", tt.genre, got, tt.want)
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
		"其他/" + titleA + " (2024)/tvshow.nfo",                         // 不在 短剧/ 下
		Root + "/" + genreA + "/不存在 (2024)/tvshow.nfo",                // 剧不存在
		dir + "/tvshow.txt",                                           // 剧级扩展名非法
		dir + "/tvshow.nfo/x",                                         // 段数多余
		dir + "/Season 02/" + titleA + " - S01E01 - 开局.strm",          // 季目录错
		season + "/" + titleA + " - S01E99 - 第99集.strm",               // 集不存在
		season + "/" + titleA + " - S01E01 - 开局.mp4",                  // 集扩展名非法
		season + "/" + titleA + " - S01E01 - 开局",                      // 无扩展名
		season + "/tvshow.nfo",                                        // 季目录下的 tvshow.nfo 不是合法形状
		season + "/.strm",                                             // 空主干
		Root + "/" + otherGenre + "/" + titleA + " (2024)/tvshow.nfo", // 分类不符
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

// AC（硬约束 6）：自动分块后的合并集在树里只产出 1 条文件对，标签是「第 A-B 集」，
// 且 strm 的 id 里 uid 是合并集句柄 m<dramaID>_<from>_<to>（不是任一单集 uid）。
func TestListMergedGroupCollapsesToOne(t *testing.T) {
	ctx := context.Background()
	f := seed(t, "")
	st := store.NewStore(f.db.DB)

	// 把 fixture 的两集并成 1 集：只改 N，下一次列举立即是新分块。
	if err := store.NewSettings(f.db.DB).Set(ctx, store.KeyMergeChunkSize, "2"); err != nil {
		t.Fatalf("Set merge_chunk_size: %v", err)
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

	// strm 的 uid 是 m<dramaID>_<from>_<to>，id 由注入的 IDFunc 计算。
	uid := items[0].UID
	if uid != "m1_1_2" {
		t.Fatalf("合并集 uid = %q, 期望 m1_1_2（剧 id_起始集_结束集）", uid)
	}
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

// AC（MKACG-649 AC-4，运行态口径）：21 集 / N=10 的第 3 块是单集，对外集号 3、源集号 21
// —— 文件名与 NFO 标题必须是「第21集」，不是「第3集」。
//
// 这条走真实 store.ListEmbyItems + 真实渲染（不只测 embyLabel 纯函数）：自动分块后
// 「第 i 块」的对外集号与它装着的源集号第一次可以不一致，链路上任何一处取错空间都该红。
func TestListRemainderSingleRendersSourceEpisodeNumber(t *testing.T) {
	ctx := context.Background()
	f := seed(t, "")
	st := f.store()
	if err := store.NewSettings(f.db.DB).Set(ctx, store.KeyMergeChunkSize, "10"); err != nil {
		t.Fatalf("Set merge_chunk_size: %v", err)
	}

	dramaID := f.addDrama(t, store.Drama{
		Provider: "web", ProviderID: "z21", Title: "余数剧", Year: 2026, Genre: genreA,
	})
	for i := 1; i <= 21; i++ {
		if _, err := st.UpsertEpisode(ctx, store.Episode{
			DramaID: dramaID, EpNo: i, DurationSec: 60, Source: "web", Vid: fmt.Sprintf("v%d", i),
		}); err != nil {
			t.Fatalf("UpsertEpisode %d: %v", i, err)
		}
	}
	items, err := st.ListEmbyItems(ctx, dramaID)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("对外集数 = %d, 期望 3（10 + 10 + 1）", len(items))
	}
	last := items[2]
	if last.Kind != "single" || last.EpNo != 3 || last.EpFrom != 21 {
		t.Fatalf("第 3 块 = %+v, 期望 single / ep_no=3 / 源集号=21", last)
	}

	dir := dramaDirPath(genreA, "余数剧 (2026)")
	base := "余数剧 - S01E03 - 第21集"
	nodes, err := f.tree.List(ctx, dir+"/"+seasonDir)
	if err != nil {
		t.Fatalf("List(季目录): %v", err)
	}
	got := map[string]bool{}
	for _, n := range nodes {
		if !n.IsDir {
			got[n.Path] = true
		}
	}
	for _, want := range []string{dir + "/" + seasonDir + "/" + base + ".strm", dir + "/" + seasonDir + "/" + base + ".nfo"} {
		if !got[want] {
			t.Fatalf("季目录文件 = %v, 缺 %q（单集块标题必须取源集号）", got, want)
		}
	}

	nfo, err := f.tree.Render(ctx, dir+"/"+seasonDir+"/"+base+".nfo")
	if err != nil {
		t.Fatalf("Render(NFO): %v", err)
	}
	var parsed episodeNFO
	if err := xml.Unmarshal(nfo, &parsed); err != nil {
		t.Fatalf("解析 NFO: %v\n%s", err, nfo)
	}
	if parsed.Title != "第21集" {
		t.Errorf("NFO title = %q, 期望 第21集（源集号，不是对外集号 3）", parsed.Title)
	}
	if parsed.Episode != 3 {
		t.Errorf("NFO episode = %d, 期望 3（Emby 的季内集号仍是对外集号）", parsed.Episode)
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

func (c *countingStore) ListDramaDirsByGenre(ctx context.Context, genres []string) ([]store.DramaDir, error) {
	c.calls["ListDramaDirsByGenre"]++
	return c.inner.ListDramaDirsByGenre(ctx, genres)
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

// TreeRevision 直接转发、不计数：它不是一次查询，计数要只反映「查了什么」。
func (c *countingStore) TreeRevision() uint64 { return c.inner.TreeRevision() }

// AC（阻断 2 的回归）：只有根目录列举需要「全部分类」，其余层只做存在性判断。
//
// 这条数的是 **store 方法调用**（不是 SQL 文本）：上一轮把点查写成死代码、
// genresForDir 仍走 ListGenres，SQL 文本测试照样绿，只有数调用才能抓住。
func TestListDoesNotScanAllGenres(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	cs := newCountingStore(f.store())
	tree := NewTree(cs, store.NewSettings(f.db.DB), stubID, f.covers, slog.New(slog.DiscardHandler))

	tests := []struct {
		name           string
		dir            string
		wantListGenre  int // ListGenres 调用次数
		wantExists     int // GenreExists 调用次数下限
		wantTitle      int // DramaByTitleYear 调用次数下限
		wantListDramas int // ListDramasByGenre 调用次数
	}{
		{name: "根/必须拿全部分类", dir: "", wantListGenre: 1},
		{name: "分类层", dir: Root + "/" + genreA, wantExists: 1, wantListDramas: 1},
		{name: "剧目录", dir: f.dirA, wantTitle: 1},
		// 剧名被 sanitize 过的目录：title 点查不中 → 退到「本分类内比对」，
		// 分类解析走 GenreExists（绝不能退回全表 DISTINCT genre）。
		{name: "sanitize 过的剧目录", dir: f.dirB, wantExists: 1, wantListDramas: 1},
		{name: "季目录", dir: f.dirA + "/" + seasonDir, wantTitle: 1},
		{name: "兜底分类层", dir: Root + "/" + otherGenre, wantExists: 1, wantListDramas: 1},
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
			// 每层至多一次列举式查询，且只在该层真正需要列剧时才发生。
			if got := cs.count("ListDramaDirsByGenre"); got != tt.wantListDramas {
				t.Errorf("List(%q) 调了 %d 次 ListDramaDirsByGenre, 期望 %d", tt.dir, got, tt.wantListDramas)
			}
		})
	}
}

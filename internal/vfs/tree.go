package vfs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/justforlxz/drama-hub/internal/store"
)

// Tree 是虚拟文件树的中枢：List 列出一个目录的直接子项，Render 渲染文件字节。
//
// 无状态、无缓存：每次请求现算，一次请求内完成（AGENTS.md 硬约束 3）。
// 由装配根（cmd）构造并单例持有；webdav 通过 List/Render 消费本树。
type Tree struct {
	st       Store
	settings *store.Settings
	id       IDFunc
	logger   *slog.Logger
}

// Store 是本包消费的只读能力（接口定义在消费方，见 docs/DEV-STANDARDS.md「代码组织」；
// 生产实现是 *store.Store）。声明成接口不是为了将来换实现，而是让测试能数「列举某一层
// 到底调了哪些查询」——`internal/vfs` 只认这一个缝，webdav 也够不到 store。
type Store interface {
	// ListGenres 只有根目录用：它必须拿到全部分类，代价是覆盖索引全扫。
	ListGenres(ctx context.Context) ([]string, error)
	// GenreExists 是非根层的唯一分类查询：目录名 → 取值集合是纯映射，只需存在性。
	GenreExists(ctx context.Context, genre string) (bool, error)
	ListDramasByShard(ctx context.Context, genres []string, shard int) ([]store.Drama, error)
	DramaByTitleYear(ctx context.Context, title string, year int) (store.Drama, error)
	DramaByProviderID(ctx context.Context, provider, providerID string) (store.Drama, error)
	ListEmbyItems(ctx context.Context, dramaID int64) ([]store.EmbyItem, error)
}

// IDFunc 由装配根注入，把 (stream_secret, uid) 算成对外播放 id。
//
// 形态见 docs/PLAYBACK-CONTRACT.md「id 怎么来」：`uid + "-" + 签名`。vfs（rank 30）
// 不 import internal/stream（rank 40），HMAC 算法在装配根接上。
type IDFunc func(secret, uid string) string

// NewTree 构造 Tree。id 与 logger 不可为 nil（渲染 strm 与报告被跳过的脏分类必需）。
func NewTree(st Store, settings *store.Settings, id IDFunc, logger *slog.Logger) *Tree {
	return &Tree{st: st, settings: settings, id: id, logger: logger}
}

// Node 是 List 输出的一个条目：相对 vfs 根（无前导 /）的路径与其类型。
type Node struct {
	Path  string
	IsDir bool
}

const (
	// Root 是 vfs 的固定一级目录名。DAV 挂载点根（`/`）映射到它：vfs 路径恒以
	// `短剧/` 开头，DAV 路径没有这一段（internal/webdav 的 resolve 负责映射）。
	Root = "短剧"
	// seasonDir 是季目录名。2.0 只有单季，emby_item.season 恒为 1。
	seasonDir = "Season 01"
	// otherGenre 是分类缺失（drama.genre 为空）时的兜底目录名
	// （docs/DIRECTORY-CONTRACT.md「分类缺失的兜底名」）。上游真有同名分类时
	// 两者并进同一个目录：目录名不做区分。
	otherGenre = "其他"
)

// List 返回某个目录的直接子项，不返回孙子及以下。
//
// dir 是相对 vfs 根的目录路径（`""` 与 `"短剧"` 都表示根）；不存在的目录返回
// ErrNotFound。层与层的成本（docs/DIRECTORY-CONTRACT.md「MUST：目录列举必须是
// 「一个目录一次查询」」）：
//
//	根        1 次 DISTINCT genre
//	分类       1 次 DISTINCT genre + 1 次 DISTINCT shard（兜底名「其他」最多两个 genre 值）
//	分片       1 次 DISTINCT genre + 1 次 WHERE genre IN (…) AND shard = ?
//	剧        1 次剧目点查（tvshow.nfo 与 Season 01 两个子项）
//	Season    1 次剧目点查 + 1 次 ListEmbyItems
//
// 与全库规模无关：不枚举整棵树、不做全表扫。
func (t *Tree) List(ctx context.Context, dir string) ([]Node, error) {
	segs, ok := splitDir(dir)
	if !ok {
		return nil, ErrNotFound
	}
	switch len(segs) {
	case 0:
		return t.listGenres(ctx)
	case 1:
		return t.listShards(ctx, segs[0])
	case 2:
		return t.listDramaDirs(ctx, segs[0], segs[1])
	case 3:
		return t.listDrama(ctx, segs[0], segs[1], segs[2])
	case 4:
		if segs[3] != seasonDir {
			return nil, ErrNotFound
		}
		return t.listSeason(ctx, segs[0], segs[1], segs[2])
	}
	return nil, ErrNotFound
}

// splitDir 把 vfs 目录路径切成「根之下的段」：根返回空切片，ok=false 表示
// 不是本树里的目录形状（不在 短剧/ 下、含空段）。
func splitDir(dir string) ([]string, bool) {
	p := strings.Trim(dir, "/")
	if p == "" || p == Root {
		return nil, true
	}
	segs := strings.Split(p, "/")
	if segs[0] != Root {
		return nil, false
	}
	for _, s := range segs[1:] {
		if s == "" {
			return nil, false
		}
	}
	return segs[1:], true
}

// listGenres 列根目录：只有分类目录（没有 `全部/`）。
//
// 分类目录集合 = SELECT DISTINCT genre；兜底名「其他」与上游同名的分类合并成
// 一个目录（两份 genre 值落同一个目录名）。
//
// 含路径分隔符的分类名不转义（docs/DIRECTORY-CONTRACT.md「分类名不做转义」），
// 但它无法成为一个路径段：强行拼进去会在根目录里产出跨段节点，x/net/webdav 的
// walkFS 在给它 Stat 时失败、整份 multistatus 被截断——整个挂载点对客户端不可解析。
// 因此这类脏分类跳过并记一条 warn（该分类下的剧在挂载点里不可见），
// 让父目录列举始终保持可解析。
func (t *Tree) listGenres(ctx context.Context) ([]Node, error) {
	genres, err := t.st.ListGenres(ctx)
	if err != nil {
		return nil, fmt.Errorf("列分类: %w", err)
	}
	seen := make(map[string]struct{}, len(genres))
	out := make([]Node, 0, len(genres))
	for _, g := range genres {
		name := genreDirName(g)
		if !pathSafeName(name) {
			t.logger.Warn("跳过含路径分隔符的分类（不能作为目录段）", "genre", g)
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, Node{Path: Root + "/" + name, IsDir: true})
	}
	sortNodes(out)
	return out, nil
}

// listShards 列分类目录：分片目录恒为 00–7f 的全部，不查库。
//
// docs/DIRECTORY-CONTRACT.md 的「分类目录 1 次 DISTINCT shard（或直接回 00–7f）」
// 取后者：`DISTINCT shard` 要扫该分类的全部索引行（183k 行约 20–40ms，随分类规模
// 线性增长），而分片集合本来就是代码常量（`store.ShardCount`）——直接回全部，
// 空分片列出来是空目录（不发生查询），分类目录一次请求只剩一次 genre 点查。
func (t *Tree) listShards(ctx context.Context, genreDir string) ([]Node, error) {
	if _, err := t.genresForDir(ctx, genreDir); err != nil {
		return nil, err
	}
	prefix := Root + "/" + genreDir
	out := make([]Node, 0, store.ShardCount)
	for shard := 0; shard < store.ShardCount; shard++ {
		out = append(out, Node{Path: prefix + "/" + shardDirName(shard), IsDir: true})
	}
	return out, nil
}

// listDramaDirs 列分片目录：只有剧目录，按目录名排序。
func (t *Tree) listDramaDirs(ctx context.Context, genreDir, shardDir string) ([]Node, error) {
	shard, ok := parseShard(shardDir)
	if !ok {
		return nil, ErrNotFound
	}
	genres, err := t.genresForDir(ctx, genreDir)
	if err != nil {
		return nil, err
	}
	dramas, err := t.st.ListDramasByShard(ctx, genres, shard)
	if err != nil {
		return nil, fmt.Errorf("列分片剧目: %w", err)
	}
	prefix := Root + "/" + genreDir + "/" + shardDir
	entries := dramaDirEntries(dramas)
	out := make([]Node, 0, len(entries))
	for _, e := range entries {
		out = append(out, Node{Path: prefix + "/" + e.dir, IsDir: true})
	}
	return out, nil
}

// listDrama 列剧目录：tvshow.nfo 与 Season 01。
func (t *Tree) listDrama(ctx context.Context, genreDir, shardDir, dramaDir string) ([]Node, error) {
	if _, err := t.locateDrama(ctx, genreDir, shardDir, dramaDir); err != nil {
		return nil, err
	}
	dir := Root + "/" + genreDir + "/" + shardDir + "/" + dramaDir
	return []Node{
		{Path: dir + "/tvshow.nfo"},
		{Path: dir + "/" + seasonDir, IsDir: true},
	}, nil
}

// listSeason 列季目录：每集一对 .strm / .nfo（合并集也只产出一对，硬约束 6）。
func (t *Tree) listSeason(ctx context.Context, genreDir, shardDir, dramaDir string) ([]Node, error) {
	d, err := t.locateDrama(ctx, genreDir, shardDir, dramaDir)
	if err != nil {
		return nil, err
	}
	items, err := t.st.ListEmbyItems(ctx, d.ID)
	if err != nil {
		return nil, fmt.Errorf("列对外剧集 drama_id=%d: %w", d.ID, err)
	}
	dir := Root + "/" + genreDir + "/" + shardDir + "/" + dramaDir + "/" + seasonDir
	out := make([]Node, 0, len(items)*2)
	for _, item := range items {
		base := embyBase(d, item)
		out = append(out,
			Node{Path: dir + "/" + base + ".strm"},
			Node{Path: dir + "/" + base + ".nfo"},
		)
	}
	return out, nil
}

// genresForDir 返回某个分类目录名对应的库内 genre 原值集合。
//
// 目录名 → 取值集合是**纯映射**：兜底名「其他」对应 `genre=”` 与「上游真的叫
// 其他」两种取值（契约要求合并），其余目录名就是 genre 原值。因此这里只做存在性
// 判断（`GenreExists`，`EXISTS` 点查，O(log n)），不做任何列举式查询。
//
// 不要退回 `SELECT DISTINCT genre FROM drama` 再过滤：那是整条覆盖索引全扫
// （60 万行约 74ms），而且每层列举都得付一次——这正是上一轮 Review 抓到的
// 「看起来接线了、其实仍在全表扫」。`TestListDoesNotScanAllGenres` 数的是
// **store 方法调用**，不是 SQL 文本，改回去立刻红。
func (t *Tree) genresForDir(ctx context.Context, name string) ([]string, error) {
	values := []string{name}
	if name == otherGenre {
		values = []string{"", otherGenre}
	}
	out := make([]string, 0, len(values))
	for _, g := range values {
		ok, err := t.st.GenreExists(ctx, g)
		if err != nil {
			return nil, fmt.Errorf("查分类 %q: %w", g, err)
		}
		if ok {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// locateDrama 由「分类目录名 + 分片目录名 + 剧目录名」定位一部剧。
//
// 优先走索引点查：剧目录名带冲突后缀时 `WHERE provider=? AND provider_id=?`，
// 否则 `WHERE title=? AND year=?`（idx_drama_title）。点查命中后仍比对目录名与
// 归属（分类、分片），防止把别的分片/分类里的同名剧渲染到本路径下。
//
// 标题含 `/ : * ?` 等字符时目录名是 sanitize 过的，点查查不到，于是退到
// 「本分片内的剧目比对」（`WHERE genre IN (…) AND shard = ?`，走
// idx_drama_genre_shard_title，不是全表扫）。
func (t *Tree) locateDrama(ctx context.Context, genreDir, shardDir, name string) (store.Drama, error) {
	shard, ok := parseShard(shardDir)
	if !ok {
		return store.Drama{}, ErrNotFound
	}
	if provider, providerID, ok := parseSuffix(name); ok {
		d, err := t.st.DramaByProviderID(ctx, provider, providerID)
		if errors.Is(err, store.ErrNotFound) {
			return store.Drama{}, ErrNotFound
		}
		if err != nil {
			return store.Drama{}, fmt.Errorf("按 provider_id 查剧目: %w", err)
		}
		if !located(d, genreDir, shard, name) {
			return store.Drama{}, ErrNotFound
		}
		return d, nil
	}

	if title, year, ok := parseBase(name); ok {
		d, err := t.st.DramaByTitleYear(ctx, title, year)
		switch {
		case err == nil && located(d, genreDir, shard, name):
			return d, nil
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return store.Drama{}, fmt.Errorf("按 title/year 查剧目: %w", err)
		}
	}

	genres, err := t.genresForDir(ctx, genreDir)
	if err != nil {
		return store.Drama{}, err
	}
	dramas, err := t.st.ListDramasByShard(ctx, genres, shard)
	if err != nil {
		return store.Drama{}, fmt.Errorf("列分片剧目: %w", err)
	}
	for _, e := range dramaDirEntries(dramas) {
		if e.dir == name {
			return e.drama, nil
		}
	}
	return store.Drama{}, ErrNotFound
}

// located 判断一部剧是否就落在这个「分类/分片/目录名」上（点查结果的归属校验）。
func located(d store.Drama, genreDir string, shard int, name string) bool {
	if genreDirName(d.Genre) != genreDir || d.Shard != shard {
		return false
	}
	return dramaDir(d) == name || conflictDir(d) == name
}

// genreDirName 把 drama.genre 映射成一级目录名：空串（未声明）落兜底名。
func genreDirName(genre string) string {
	if genre == "" {
		return otherGenre
	}
	return genre
}

// pathSafeName 报告一个目录名能不能当**一个**路径段。
//
// 含 `/` 或 `\` 的名字不能：它会把节点变成跨段路径，父目录列举随之无法 Stat 它。
// 分类名按契约不转义，这类脏值由 listGenres 跳过并记日志；剧名与文件名用
// sanitizeName 替换成 `_`，因此它们总是安全的。
func pathSafeName(name string) bool {
	return !strings.ContainsAny(name, `/\`)
}

// shardDirName 是分片目录名：两位小写十六进制（00–7f，N=128）。
func shardDirName(shard int) string {
	return fmt.Sprintf("%02x", shard)
}

// parseShard 解析分片目录名；非十六进制或超出 ShardCount 范围返回 false。
func parseShard(name string) (int, bool) {
	n, err := strconv.ParseUint(name, 16, 32)
	if err != nil || n >= uint64(store.ShardCount) {
		return 0, false
	}
	return int(n), true
}

// dramaDirEntry 是一部剧与它在树里的目录名。
type dramaDirEntry struct {
	drama store.Drama
	dir   string
}

// dramaDirEntries 给一个父目录（一个分片）下的剧目算目录名，并按目录名排序。
//
// 同名冲突规则（docs/DIRECTORY-CONTRACT.md「同名剧冲突规则」）：同一个父目录内
// 基准名重复时，所有冲突成员改用 `<基准名> [<provider>-<provider_id>]`；不冲突的
// 成员保持基准名。后缀用 provider_id 原文，不截断——截断在 60 万规模上会再撞一次。
func dramaDirEntries(dramas []store.Drama) []dramaDirEntry {
	count := make(map[string]int, len(dramas))
	for _, d := range dramas {
		count[dramaDir(d)]++
	}
	out := make([]dramaDirEntry, 0, len(dramas))
	for _, d := range dramas {
		dir := dramaDir(d)
		if count[dir] > 1 {
			dir = conflictDir(d)
		}
		out = append(out, dramaDirEntry{drama: d, dir: dir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out
}

// dramaDir 是剧目录基准名：<剧名> (<年份>)。年份是命名的一部分，Year==0 显式
// 写成 (<0>)，不省略括号——命名是纯函数，空态显式化。
func dramaDir(d store.Drama) string {
	return sanitizeName(d.Title) + " (" + strconv.Itoa(d.Year) + ")"
}

// conflictDir 是冲突成员的目录名：基准名 + ` [<provider>-<provider_id>]`。
func conflictDir(d store.Drama) string {
	return dramaDir(d) + " [" + d.Provider + "-" + d.ProviderID + "]"
}

// parseBase 把目录名拆回 (标题候选, 年份)：`<name> (<year>)`。
//
// 标题候选是 sanitize 过的文本，只用于点查；调用方必须再比对一次目录名，
// 不能拿它当剧名真值。
func parseBase(dir string) (string, int, bool) {
	if !strings.HasSuffix(dir, ")") {
		return "", 0, false
	}
	i := strings.LastIndex(dir, " (")
	if i < 0 {
		return "", 0, false
	}
	year, err := strconv.Atoi(dir[i+2 : len(dir)-1])
	if err != nil {
		return "", 0, false
	}
	return dir[:i], year, true
}

// parseSuffix 解析同名冲突后缀：`<基准名> [<provider>-<provider_id>]`。
//
// provider 不含 `-`（插件名），provider_id 取第一个 `-` 之后的原文。
func parseSuffix(dir string) (provider, providerID string, ok bool) {
	if !strings.HasSuffix(dir, "]") {
		return "", "", false
	}
	i := strings.LastIndex(dir, " [")
	if i < 0 {
		return "", "", false
	}
	provider, providerID, ok = strings.Cut(dir[i+2:len(dir)-1], "-")
	if !ok || provider == "" || providerID == "" {
		return "", "", false
	}
	return provider, providerID, true
}

// sortNodes 按路径字节序排序（目录名的排序就是契约里的排序口径）。
func sortNodes(nodes []Node) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Path < nodes[j].Path })
}

// embyBase 是对外剧集的文件名主干（不含扩展名）：<剧名> - S01E01 - <标签>。
// NFO 的 <title> 与 .strm/.nfo 的文件名共用它，杜绝两处漂移。
//
// 集号取 emby_item.ep_no、标签见 embyLabel，两者都只来自派生表（硬约束 6）：
// 合并调整重算派生表后文件名随之变化。
func embyBase(d store.Drama, item store.EmbyItem) string {
	return sanitizeName(d.Title) + " - " + embyNumber(item) + " - " + embyLabel(item)
}

// embyNumber 是集号文本 S%02dE%02d（2.0 恒为 Season 01）。
func embyNumber(item store.EmbyItem) string {
	return "S" + twoDigit(item.Season) + "E" + twoDigit(item.EpNo)
}

// twoDigit 把集号格式化为两位，超过两位时按原宽度展开。
func twoDigit(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// embyLabel 是对外剧集的标签：
//
//   - 合并集：`第 A-B 集`（A/B 是组内最小/最大源集号；A==B 时退化为 `第 A 集`）
//   - 单集：源集标题；空则回退 `第 N 集`（N 是对外集号）
//
// 文件名主干与 NFO <title> 共用本函数，避免两处漂移。
func embyLabel(item store.EmbyItem) string {
	if item.Kind == "merged" {
		if item.EpFrom == item.EpTo {
			return "第" + strconv.Itoa(item.EpFrom) + "集"
		}
		return "第" + strconv.Itoa(item.EpFrom) + "-" + strconv.Itoa(item.EpTo) + "集"
	}
	label := sanitizeName(item.Title)
	if label == "" {
		label = "第" + strconv.Itoa(item.EpNo) + "集"
	}
	return label
}

// sanitizeName 把 Windows / WebDAV 非法字符替换为 _。目录名与文件名共用。
func sanitizeName(s string) string {
	return strings.NewReplacer(
		"/", "_", `\`, "_", ":", "_", "*", "_",
		"?", "_", `"`, "_", "<", "_", ">", "_", "|", "_",
	).Replace(s)
}

// Render 渲染 path 处的文件字节：解析路径 → 查库 → 一次返回完整字节。
//
// 路径以 / 分隔、相对 vfs 根、无前导 /（WebDAV 的绝对路径由 internal/webdav 映射）。
// 合法形状只有剧级 tvshow.nfo 与季级 .strm/.nfo 两种；目录、未知路径、段数不符、
// 不存在的剧/集一律 ErrNotFound。返回字节的长度就是 len(body)（硬约束 3），
// 本包没有任何独立的长度字段或方法。
func (t *Tree) Render(ctx context.Context, p string) ([]byte, error) {
	req, ok := parsePath(p)
	if !ok {
		return nil, ErrNotFound
	}
	d, err := t.locateDrama(ctx, req.genreDir, req.shardDir, req.dramaDir)
	if err != nil {
		return nil, err
	}
	if req.isShow {
		return renderShowNFO(d)
	}
	items, err := t.st.ListEmbyItems(ctx, d.ID)
	if err != nil {
		return nil, fmt.Errorf("列对外剧集 drama_id=%d: %w", d.ID, err)
	}
	for _, item := range items {
		if embyBase(d, item) != req.itemBase {
			continue
		}
		if req.ext == ".strm" {
			return t.renderStrm(ctx, item)
		}
		return renderItemNFO(item)
	}
	return nil, ErrNotFound
}

// renderRequest 是 parsePath 的解构结果。
type renderRequest struct {
	genreDir string // 一级目录（分类，可能是兜底名「其他」）
	shardDir string // 分片目录名（两位十六进制）
	dramaDir string // 剧目录名（可能是带冲突后缀的形态）
	isShow   bool   // true = 剧级 tvshow.nfo；false = 季级文件
	itemBase string // 文件名主干（isShow 时为空）
	ext      string // ".strm" / ".nfo"（isShow 时为空）
}

// parsePath 解析相对路径；ok=false 表示不是本树里可渲染的文件形状。
//
// 合法：短剧/<分类>/<分片>/<剧目录>/tvshow.nfo（5 段）
//
//	短剧/<分类>/<分片>/<剧目录>/Season 01/<文件名>.strm|.nfo（6 段）
func parsePath(p string) (renderRequest, bool) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) < 5 || segs[0] != Root {
		return renderRequest{}, false
	}
	base := renderRequest{genreDir: segs[1], shardDir: segs[2], dramaDir: segs[3]}
	switch {
	case len(segs) == 5 && segs[4] == "tvshow.nfo":
		base.isShow = true
		return base, true
	case len(segs) == 6 && segs[4] == seasonDir:
		ext := extension(segs[5])
		if ext != ".strm" && ext != ".nfo" {
			return renderRequest{}, false
		}
		name := strings.TrimSuffix(segs[5], ext)
		if name == "" {
			return renderRequest{}, false
		}
		base.itemBase, base.ext = name, ext
		return base, true
	}
	return renderRequest{}, false
}

// extension 返回路径最后一段的扩展名（含点），没有则空串。
//
// 手写而不 import path：只认最后一个点，与 path.Ext 语义一致但少一个依赖。
func extension(base string) string {
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		return base[i:]
	}
	return ""
}

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
// 结构（目录列举结果）在内存里按目录分片缓存，按树结构版本号整体失效；文件字节仍然
// 每次现渲染（硬约束 3：Content-Length 必须精确）。缓存不落盘，进程重启后从空开始
// （AGENTS.md 硬约束 1、4；docs/adr/0015）。
// 由装配根（cmd）构造并单例持有；webdav 通过 List/Render 消费本树。
type Tree struct {
	st       Store
	settings *store.Settings
	id       IDFunc
	covers   Coverer
	logger   *slog.Logger
	cache    *dirCache
}

// Coverer 是树需要的封面能力（生产实现：*cache.CoverCache）。
//
// 接口定义在消费方（docs/DEV-STANDARDS.md「代码组织」）：vfs 只关心两件事——「这部剧
// 有没有封面」与「把封面字节给我」。回源、落盘、single-flight、超时全在实现方，
// vfs 不认识 HTTP 也不认识 artifact 表。
type Coverer interface {
	// HasCover 报告这部剧有没有可用封面。不触网、不读文件。
	HasCover(ctx context.Context, provider, providerID string) (bool, error)
	// Poster 返回封面字节；本地没有时回源一次。available=false 表示当前没有可用
	// 封面（源未声明或回源失败），树按「文件不存在」处理。
	Poster(ctx context.Context, provider, providerID, url string) (body []byte, available bool, err error)
}

// Store 是本包消费的只读能力（接口定义在消费方，见 docs/DEV-STANDARDS.md「代码组织」；
// 生产实现是 *store.Store）。声明成接口不是为了将来换实现，而是让测试能数「列举某一层
// 到底调了哪些查询」——`internal/vfs` 只认这一个缝，webdav 也够不到 store。
type Store interface {
	// ListGenres 只有根目录用：它必须拿到全部分类，代价是覆盖索引全扫。
	ListGenres(ctx context.Context) ([]string, error)
	// GenreExists 是非根层的唯一分类查询：目录名 → 取值集合是纯映射，只需存在性。
	GenreExists(ctx context.Context, genre string) (bool, error)
	ListDramaDirsByGenre(ctx context.Context, genres []string) ([]store.DramaDir, error)
	DramaByTitleYear(ctx context.Context, title string, year int) (store.Drama, error)
	DramaByProviderID(ctx context.Context, provider, providerID string) (store.Drama, error)
	ListEmbyItems(ctx context.Context, dramaID int64) ([]store.EmbyItem, error)
	// TreeRevision 返回树结构版本号：任何影响结构或文件长度的写入成功后都会递增，
	// 目录缓存据此整体失效（docs/adr/0015）。只读，不参与列举查询。
	TreeRevision() uint64
}

// IDFunc 由装配根注入，把 (stream_secret, uid) 算成对外播放 id。
//
// 形态见 docs/PLAYBACK-CONTRACT.md「id 怎么来」：`uid + "-" + 签名`。vfs（rank 30）
// 不 import internal/stream（rank 40），HMAC 算法在装配根接上。
type IDFunc func(secret, uid string) string

// NewTree 构造 Tree。id 与 logger 不可为 nil（渲染 strm 与报告「分类名转义后同名」必需）。
// covers 为 nil 等价于「这个库没有封面」：poster.jpg 永远不出现（单测可以用它省掉缓存）。
func NewTree(st Store, settings *store.Settings, id IDFunc, covers Coverer, logger *slog.Logger) *Tree {
	return &Tree{st: st, settings: settings, id: id, covers: covers, logger: logger, cache: newDirCache()}
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
	// seasonDir 是季目录名。2.0 只有单季，派生出的对外剧集 season 恒为 1。
	seasonDir = "Season 01"
	// posterName 是剧目录下的封面文件名（Emby / Jellyfin 认的剧集封面名）。
	//
	// 它和 tvshow.nfo 一样是**按请求渲染的虚拟文件**，字节取自该剧的本地封面缓存
	// （artifact(kind='cover')），磁盘上不会多出任何实体文件（硬约束 1）。
	posterName = "poster.jpg"
	// otherGenre 是分类缺失（drama.genre 为空）时的兜底目录名
	// （docs/DIRECTORY-CONTRACT.md「分类缺失的兜底名」）。上游真有同名分类时
	// 两者并进同一个目录：目录名不做区分。
	otherGenre = "其他"
)

// List 返回某个目录的直接子项，不返回孙子及以下。
//
// dir 是相对 vfs 根的目录路径（`""` 与 `"短剧"` 都表示根）；不存在的目录返回
// ErrNotFound。列举结果按目录缓存在内存里（版本号一致即命中，见 cache.go），下面
// 的成本是「未命中时」一次计算的成本（docs/DIRECTORY-CONTRACT.md「MUST：目录列举
// 必须是「一个目录一次查询」」）：
//
//	根        1 次 DISTINCT genre
//	分类       1–2 次 genre 存在性点查 + 1 次 WHERE genre IN (…) ORDER BY title
//	剧        1 次剧目点查 + 1 次封面行点查（tvshow.nfo、有封面时的 poster.jpg、Season 01）
//	Season    1 次剧目点查 + 1 次 ListEmbyItems
//
// 与全库规模无关：不枚举整棵树、不做全表扫。
func (t *Tree) List(ctx context.Context, dir string) ([]Node, error) {
	segs, ok := splitDir(dir)
	if !ok {
		return nil, ErrNotFound
	}
	// 版本号在计算之前取：计算期间发生的写入会递增版本号，于是结果只会被存到
	// 旧版本下（后续读不到），而不会把旧数据存进新版本（docs/adr/0015）。
	rev := t.st.TreeRevision()
	return t.cache.load(ctx, strings.Join(segs, "/"), rev, func(ctx context.Context) ([]Node, error) {
		return t.list(ctx, segs)
	})
}

// Revision 返回当前树结构版本号：webdav 用它给 PROPFIND 响应缓存打版本。
func (t *Tree) Revision() uint64 { return t.st.TreeRevision() }

// list 是 List 的现算实现：按目录层级分派，结果由调用方写入缓存。
func (t *Tree) list(ctx context.Context, segs []string) ([]Node, error) {
	switch len(segs) {
	case 0:
		return t.listGenres(ctx)
	case 1:
		return t.listDramaDirs(ctx, segs[0])
	case 2:
		return t.listDrama(ctx, segs[0], segs[1])
	case 3:
		if segs[2] != seasonDir {
			return nil, ErrNotFound
		}
		return t.listSeason(ctx, segs[0], segs[1])
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
// 目录名一律过 sanitizeName（docs/DIRECTORY-CONTRACT.md「分类名按文件名规则转义」）：
// 含路径分隔符的脏 tags[0]（如 `剧情/古装`）转义成 `剧情_古装`，于是它能作为一个
// 普通路径段被列举、被 Stat；不转义的话跨段节点会让 x/net/webdav 的 walkFS 在 Stat
// 时失败、整份 multistatus 被截断，整个挂载点对客户端不可解析。
// 转义后同名的两个分类（`剧情/古装` 与 `剧情_古装`）并进同一个目录，记一条 debug。
func (t *Tree) listGenres(ctx context.Context) ([]Node, error) {
	genres, err := t.st.ListGenres(ctx)
	if err != nil {
		return nil, fmt.Errorf("列分类: %w", err)
	}
	seen := make(map[string]string, len(genres))
	out := make([]Node, 0, len(genres))
	for _, g := range genres {
		name := genreDir(g)
		if prev, dup := seen[name]; dup {
			t.logger.Debug("分类名转义后同名，已合并为同一目录", "dir", name, "genre_a", prev, "genre_b", g)
			continue
		}
		seen[name] = g
		out = append(out, Node{Path: Root + "/" + name, IsDir: true})
	}
	sortNodes(out)
	return out, nil
}

// listDramaDirs 列分类目录：只有剧目录，按目录名排序。
//
// 一次 `WHERE genre IN (…) ORDER BY title`（走 idx_drama_genre_title），不分页、
// 不截断：一个分类目录的全部剧都在这一层。代价与结论（单目录子项上限）的真值是
// docs/DIRECTORY-CONTRACT.md「单目录子项上限」。
func (t *Tree) listDramaDirs(ctx context.Context, genreDir string) ([]Node, error) {
	genres, err := t.genresForDir(ctx, genreDir)
	if err != nil {
		return nil, err
	}
	dramas, err := t.st.ListDramaDirsByGenre(ctx, genres)
	if err != nil {
		return nil, fmt.Errorf("列分类剧目: %w", err)
	}
	prefix := Root + "/" + genreDir
	entries := dramaDirEntries(dramas)
	out := make([]Node, 0, len(entries))
	for _, e := range entries {
		out = append(out, Node{Path: prefix + "/" + e.dir, IsDir: true})
	}
	return out, nil
}

// listDrama 列剧目录：tvshow.nfo、有封面时的 poster.jpg、Season 01。
//
// poster.jpg 只在「这部剧有可用封面」时出现（MKACG-650）：封面是可选资产，没有它的
// 剧目录与之前完全一致。判据是「封面本地化成功过」（artifact 行在），不看文件是否还在
// ——文件被淘汰/手删后节点仍要留着，下一次 GET 才会按需回源。
func (t *Tree) listDrama(ctx context.Context, genreDir, dramaDir string) ([]Node, error) {
	d, err := t.locateDrama(ctx, genreDir, dramaDir)
	if err != nil {
		return nil, err
	}
	dir := Root + "/" + genreDir + "/" + dramaDir
	out := make([]Node, 0, 3)
	out = append(out, Node{Path: dir + "/tvshow.nfo"})
	cover, err := t.hasCover(ctx, d)
	if err != nil {
		return nil, err
	}
	if cover {
		out = append(out, Node{Path: dir + "/" + posterName})
	}
	return append(out, Node{Path: dir + "/" + seasonDir, IsDir: true}), nil
}

// listSeason 列季目录：每集一对 .strm / .nfo（合并集也只产出一对，硬约束 6）。
func (t *Tree) listSeason(ctx context.Context, genreDir, dramaDir string) ([]Node, error) {
	d, err := t.locateDrama(ctx, genreDir, dramaDir)
	if err != nil {
		return nil, err
	}
	items, err := t.st.ListEmbyItems(ctx, d.ID)
	if err != nil {
		return nil, fmt.Errorf("列对外剧集 drama_id=%d: %w", d.ID, err)
	}
	dir := Root + "/" + genreDir + "/" + dramaDir + "/" + seasonDir
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
// 目录名 → 取值集合是**纯映射**：兜底名「其他」对应 genre 为空串 与「上游真的叫
// 其他」两种取值（契约要求合并），其余目录名就是 `sanitizeName(genre)`。因此这里
// 先做存在性判断（`GenreExists`，`EXISTS` 点查，O(log n)），不做列举式查询；
// 点查全落空时（目录名是转义形态，如 `剧情_古装`）才退回一次 `ListGenres`
// 筛「转义后等于目录名」的分类——这条路径只在脏分类目录与不存在的目录上付代价。
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
	if len(out) > 0 {
		return out, nil
	}
	genres, err := t.st.ListGenres(ctx)
	if err != nil {
		return nil, fmt.Errorf("列分类: %w", err)
	}
	for _, g := range genres {
		if genreDir(g) == name {
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
// 归属（分类），防止把别的分类里的同名剧渲染到本路径下。
//
// 标题含 `/ : * ?` 等字符时目录名是 sanitize 过的，点查查不到，于是退到
// 「本分类内的剧目比对」（`WHERE genre IN (…) ORDER BY title`，走
// idx_drama_genre_title，不是全表扫）。
func (t *Tree) locateDrama(ctx context.Context, genreDir, name string) (store.Drama, error) {
	if provider, providerID, ok := parseSuffix(name); ok {
		d, err := t.st.DramaByProviderID(ctx, provider, providerID)
		if errors.Is(err, store.ErrNotFound) {
			return store.Drama{}, ErrNotFound
		}
		if err != nil {
			return store.Drama{}, fmt.Errorf("按 provider_id 查剧目: %w", err)
		}
		if !located(dirFieldsOf(d), genreDir, name) {
			return store.Drama{}, ErrNotFound
		}
		return d, nil
	}

	if title, year, ok := parseBase(name); ok {
		d, err := t.st.DramaByTitleYear(ctx, title, year)
		switch {
		case err == nil && located(dirFieldsOf(d), genreDir, name):
			return d, nil
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return store.Drama{}, fmt.Errorf("按 title/year 查剧目: %w", err)
		}
	}

	genres, err := t.genresForDir(ctx, genreDir)
	if err != nil {
		return store.Drama{}, err
	}
	dramas, err := t.st.ListDramaDirsByGenre(ctx, genres)
	if err != nil {
		return store.Drama{}, fmt.Errorf("列分类剧目: %w", err)
	}
	for _, e := range dramaDirEntries(dramas) {
		if e.dir != name {
			continue
		}
		// 列举只带回窄列：命中后再点查一次拿完整 Drama（渲染 NFO 需要 plot）。
		d, err := t.st.DramaByProviderID(ctx, e.drama.Provider, e.drama.ProviderID)
		if errors.Is(err, store.ErrNotFound) {
			return store.Drama{}, ErrNotFound
		}
		if err != nil {
			return store.Drama{}, fmt.Errorf("按 provider_id 查剧目: %w", err)
		}
		return d, nil
	}
	return store.Drama{}, ErrNotFound
}

// dirFieldsOf 把完整 Drama 收成目录列举用的窄字段：归属校验与目录名只看这几列。
func dirFieldsOf(d store.Drama) store.DramaDir {
	return store.DramaDir{
		ID: d.ID, Provider: d.Provider, ProviderID: d.ProviderID,
		Title: d.Title, Year: d.Year, Genre: d.Genre,
	}
}

// located 判断一部剧是否就落在这个「分类/目录名」上（点查结果的归属校验）。
func located(d store.DramaDir, genreDirName, name string) bool {
	if genreDir(d.Genre) != genreDirName {
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

// genreDir 是一级目录名的唯一算法：兜底映射后按文件名规则转义
// （`剧情/古装` → `剧情_古装`）。列举、进入目录、归属校验三处都调它，杜绝两处
// 各拼一遍导致的「列得出来但进不去」。
func genreDir(genre string) string {
	return sanitizeName(genreDirName(genre))
}

// dramaDirEntry 是一部剧与它在树里的目录名。
//
// drama 是目录列举拿到的窄行（store.DramaDir）：目录名只由 title / year /
// provider / provider_id 决定，列举不需要 plot/tags 这类大列。
type dramaDirEntry struct {
	drama store.DramaDir
	dir   string
}

// dramaDirEntries 给一个父目录（一个分类）下的剧目算目录名，并按目录名排序。
//
// 同名冲突规则（docs/DIRECTORY-CONTRACT.md「同名剧冲突规则」）：同一个父目录内
// 基准名重复时，所有冲突成员改用 `<基准名> [<provider>-<provider_id>]`；不冲突的
// 成员保持基准名。后缀用 provider_id 原文，不截断——截断在 60 万规模上会再撞一次。
func dramaDirEntries(dramas []store.DramaDir) []dramaDirEntry {
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
func dramaDir(d store.DramaDir) string {
	return sanitizeName(d.Title) + " (" + strconv.Itoa(d.Year) + ")"
}

// conflictDir 是冲突成员的目录名：基准名 + ` [<provider>-<provider_id>]`。
func conflictDir(d store.DramaDir) string {
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
// 集号取派生出的对外集号、标签见 embyLabel，两者都只来自派生规则（硬约束 6）：
// 改 merge_chunk_size 后下一次列举就按新分块给出文件名。
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
//   - 合并集：`第 A-B 集`（A/B 是块内最小/最大源集号；A==B 时退化为 `第 A 集`）
//   - 单集：源集标题；空则回退 `第 N 集`（N 是**源集号** EpFrom，不是对外集号）
//
// 单集块的回退必须取源集号：自动分块后「第 i 块」的对外集号是 i，而它可能只包含
// 源第 21 集（21 集 / N=10 的第 3 块）——取 EpNo 会把标题写成「第3集」，与内容不符
// （MKACG-649 AC-4）。集号文本 `S01E03` 仍取 EpNo，那是 Emby 的季内集号，两者是
// 两件事。
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
		label = "第" + strconv.Itoa(item.EpFrom) + "集"
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
// 合法形状只有剧级 tvshow.nfo / poster.jpg 与季级 .strm/.nfo 两种（后者为文件）；目录、
// 未知路径、段数不符、不存在的剧/集一律 ErrNotFound。返回字节的长度就是 len(body)
// （硬约束 3），本包没有任何独立的长度字段或方法。
func (t *Tree) Render(ctx context.Context, p string) ([]byte, error) {
	req, ok := parsePath(p)
	if !ok {
		return nil, ErrNotFound
	}
	d, err := t.locateDrama(ctx, req.genreDir, req.dramaDir)
	if err != nil {
		return nil, err
	}
	if req.isPoster {
		return t.renderPoster(ctx, d)
	}
	if req.isShow {
		cover, err := t.hasCover(ctx, d)
		if err != nil {
			return nil, err
		}
		return renderShowNFO(d, cover)
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

// renderPoster 渲染剧目录下的 poster.jpg：字节直接来自该剧的本地封面缓存。
//
// 本地没有（文件被手删或淘汰）时由 Coverer 按需回源一次（同一封面并发只回源一次）；
// 取不回来就让它「不存在」——不是 0 字节文件，也不是半截文件（MKACG-650 的 AC）。
// 「不存在」在 webdav 层映射成 404，且目录列举会跳过它，整份 multistatus 不受影响。
func (t *Tree) renderPoster(ctx context.Context, d store.Drama) ([]byte, error) {
	if t.covers == nil {
		return nil, ErrNotFound
	}
	body, available, err := t.covers.Poster(ctx, d.Provider, d.ProviderID, d.CoverURL)
	if err != nil {
		return nil, fmt.Errorf("取封面 provider=%s id=%s: %w", d.Provider, d.ProviderID, err)
	}
	if !available {
		return nil, ErrNotFound
	}
	return body, nil
}

// hasCover 报告这部剧在树里是否带 poster.jpg（判据见 listDrama）。
func (t *Tree) hasCover(ctx context.Context, d store.Drama) (bool, error) {
	if t.covers == nil {
		return false, nil
	}
	cover, err := t.covers.HasCover(ctx, d.Provider, d.ProviderID)
	if err != nil {
		return false, fmt.Errorf("查封面 provider=%s id=%s: %w", d.Provider, d.ProviderID, err)
	}
	return cover, nil
}

// renderRequest 是 parsePath 的解构结果。
type renderRequest struct {
	genreDir string // 一级目录（分类，可能是兜底名「其他」）
	dramaDir string // 剧目录名（可能是带冲突后缀的形态）
	isShow   bool   // true = 剧级 tvshow.nfo
	isPoster bool   // true = 剧级 poster.jpg
	itemBase string // 文件名主干（剧级文件时为空）
	ext      string // ".strm" / ".nfo"（剧级文件时为空）
}

// parsePath 解析相对路径；ok=false 表示不是本树里可渲染的文件形状。
//
// 合法：短剧/<分类>/<剧目录>/tvshow.nfo（4 段）
//
//	短剧/<分类>/<剧目录>/poster.jpg（4 段）
//
//	短剧/<分类>/<剧目录>/Season 01/<文件名>.strm|.nfo（5 段）
func parsePath(p string) (renderRequest, bool) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) < 4 || segs[0] != Root {
		return renderRequest{}, false
	}
	base := renderRequest{genreDir: segs[1], dramaDir: segs[2]}
	switch {
	case len(segs) == 4 && segs[3] == "tvshow.nfo":
		base.isShow = true
		return base, true
	case len(segs) == 4 && segs[3] == posterName:
		base.isPoster = true
		return base, true
	case len(segs) == 5 && segs[3] == seasonDir:
		ext := extension(segs[4])
		if ext != ".strm" && ext != ".nfo" {
			return renderRequest{}, false
		}
		name := strings.TrimSuffix(segs[4], ext)
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

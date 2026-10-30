package hongguo

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// 目录发现（plugin.Plugin.Enumerate）的实现。口径真值见 docs/L1-CORPUS-SCOPE.md：
//
//   - sitemap 608,319 部 = 29 份子 sitemap（`/player/{id}` 与 `/detail?series_id={id}`
//     两种形状，去重后 608,273）∪ `seo-important.xml`（2,000，其中 1,954 已含在前者内）；
//   - `/category` 封顶 800 部，其中 53 部不在 sitemap 里（sitemap 生成滞后于最新上架）。
//
// 两者互不包含，所以枚举是**三段串行**：子 sitemap 逐个走 → seo-important.xml →
// `/category` 全页。框架拿到的是三段 refs 的并集（跨页去重由框架做，见
// docs/SCHEDULER-MODEL.md「段 1」）。任何单一入口都不完备：只用 sitemap 漏最新
// 53 部，只用 `/category` 只剩 800 部。
//
// 一段一份文档 = 一页：子 sitemap 实测 5.0 MiB / 22,915–43,556 个 loc，整份读完再
// 返回，不在文件中间切页——切在中间就得把「第几份 + 已读位置」都塞进游标，而每页
// 的上游成本（一次 5 MiB GET）本来就大于一次游标往返的成本。

const (
	// seoImportantPath 是 robots.txt 声明的第二份站点地图：2,000 个 loc，其中 46 个
	// （去重后）只在它里面（docs/L1-CORPUS-SCOPE.md 取证 B）。
	seoImportantPath = "/seo-important.xml"
	// categoryPath 是 `/category` 的分页入口。第 1 页用裸路径：`?page=1` 会 301 到
	// 裸路径（多一次来回），而 robots.txt 只允许裸路径与 `?page=`。
	categoryPath = "/category"

	// cursorVersion 是游标文本的版本。游标对框架不透明，但换编码时必须能认出旧值：
	// 旧版本按 ErrSchema 打回（人一看就知道该清 source_state.cursor），不能被误读成
	// 某个合法位置而静默重来。
	cursorVersion = "v1"
)

// enumPhase 是枚举的三段。顺序固定：sitemap → seo → category（sitemap 先给出体量
// 最大的那部分，`/category` 补齐最新上架）。
type enumPhase string

const (
	phaseSitemap  enumPhase = "s" // 第 pos 份子 sitemap（pos 从 0 起，按 index.xml 的顺序）
	phaseSEO      enumPhase = "e" // seo-important.xml（pos 恒 0）
	phaseCategory enumPhase = "c" // /category 第 pos 页（pos 从 1 起）
)

// enumCursor 是 Enumerate 游标的内部结构。对框架不透明，只由本包编码 / 解码。
type enumCursor struct {
	phase enumPhase
	pos   int
}

// encodeCursor 把游标编码成 `v1:<段>:<位置>`：可读、定长、不依赖时间与主机。
func encodeCursor(c enumCursor) string {
	return cursorVersion + ":" + string(c.phase) + ":" + strconv.Itoa(c.pos)
}

// decodeCursor 解码游标。cur == nil / 空 Token 表示从头开始（框架用空串表示
// 「上一轮枚举走到了尽头」）。
//
// 认不出的文本按 ErrSchema 打回而不是「当作从头开始」：静默重来会把「游标被改坏」
// 伪装成「枚举走完了」，而枚举走完的语义是「源里就这么多」。
func decodeCursor(cur *plugin.Cursor) (enumCursor, error) {
	if cur == nil || cur.Token == "" {
		return enumCursor{phase: phaseSitemap}, nil
	}
	parts := strings.Split(cur.Token, ":")
	if len(parts) != 3 || parts[0] != cursorVersion {
		return enumCursor{}, fmt.Errorf("%w: 无法识别的枚举游标 %q", plugin.ErrSchema, cur.Token)
	}
	pos, err := strconv.Atoi(parts[2])
	if err != nil || pos < 0 {
		return enumCursor{}, fmt.Errorf("%w: 枚举游标 %q 的位置非法", plugin.ErrSchema, cur.Token)
	}
	phase := enumPhase(parts[1])
	switch phase {
	case phaseSitemap, phaseSEO:
	case phaseCategory:
		if pos < 1 {
			return enumCursor{}, fmt.Errorf("%w: 枚举游标 %q 的页码非法", plugin.ErrSchema, cur.Token)
		}
	default:
		return enumCursor{}, fmt.Errorf("%w: 枚举游标 %q 的段未知", plugin.ErrSchema, cur.Token)
	}
	return enumCursor{phase: phase, pos: pos}, nil
}

// Enumerate 实现 plugin.Plugin：三段串行枚举，每页一份上游文档。
//
// 返回的 Page.Next 永远指向「下一份还没读的文档」：子 sitemap 走完 → seo →
// `/category` 第 1 页 → … → 第 totalPages 页之后 Next == nil（本轮枚举结束）。
// 游标可从中途任意续跑：每页的状态只有「哪一段、第几份」两项，都在 Token 里。
func (p *Plugin) Enumerate(ctx context.Context, cur *plugin.Cursor) (plugin.Page, error) {
	pos, err := decodeCursor(cur)
	if err != nil {
		return plugin.Page{}, err
	}
	switch pos.phase {
	case phaseSEO:
		return p.enumerateSEO(ctx)
	case phaseCategory:
		return p.enumerateCategory(ctx, pos)
	default:
		return p.enumerateSitemap(ctx, pos)
	}
}

// enumerateSitemap 读第 pos 份子 sitemap，返回它的全部剧目标识。
//
// 每次都要先取一次 index.xml（4 KB）：游标只记「第几份」，URL 由索引的顺序决定，
// 这样源侧增删子 sitemap 也不会让游标指错文件；不为省这一个 4 KB 请求把 29 个 URL
// 塞进游标（游标会变成上游文件清单的副本，还得处理两部分不一致）。
func (p *Plugin) enumerateSitemap(ctx context.Context, pos enumCursor) (plugin.Page, error) {
	urls, err := p.sitemapURLs(ctx)
	if err != nil {
		return plugin.Page{}, err
	}
	// 索引比游标小（源侧删了子 sitemap）：直接进下一段，不从起点重来。
	if pos.pos >= len(urls) {
		return p.enumerateSEO(ctx)
	}
	body, err := p.getSitemap(ctx, urls[pos.pos])
	if err != nil {
		return plugin.Page{}, err
	}
	ids, err := parseSitemapIDs(body)
	if err != nil {
		return plugin.Page{}, err
	}
	next := enumCursor{phase: phaseSitemap, pos: pos.pos + 1}
	if pos.pos+1 >= len(urls) {
		next = enumCursor{phase: phaseSEO}
	}
	return plugin.Page{Refs: p.refs(ids), Next: &plugin.Cursor{Token: encodeCursor(next)}}, nil
}

// enumerateSEO 读 seo-important.xml，返回它的全部剧目标识，下一页是 `/category` 第 1 页。
func (p *Plugin) enumerateSEO(ctx context.Context) (plugin.Page, error) {
	body, err := p.getSitemap(ctx, p.absolute(seoImportantPath))
	if err != nil {
		return plugin.Page{}, err
	}
	ids, err := parseSitemapIDs(body)
	if err != nil {
		return plugin.Page{}, err
	}
	next := encodeCursor(enumCursor{phase: phaseCategory, pos: 1})
	return plugin.Page{Refs: p.refs(ids), Next: &plugin.Cursor{Token: next}}, nil
}

// enumerateCategory 读 `/category` 第 pos 页，返回该页的剧目标识。
//
// 末页由上游的 pagination.totalPages 判定（不是靠 404 兜底）：页数到了就 Next == nil，
// 枚举结束；源侧分页变少导致游标越过末页时返回空页 + Next == nil，同样算走完。
func (p *Plugin) enumerateCategory(ctx context.Context, pos enumCursor) (plugin.Page, error) {
	path := categoryPath
	if pos.pos > 1 {
		path = categoryPath + "?page=" + strconv.Itoa(pos.pos)
	}
	body, err := p.get(ctx, path)
	if err != nil {
		return plugin.Page{}, err
	}
	page, err := parseCategoryPage(string(body), pos.pos)
	if err != nil {
		return plugin.Page{}, err
	}
	var next *plugin.Cursor
	if pos.pos < page.totalPages {
		next = &plugin.Cursor{Token: encodeCursor(enumCursor{phase: phaseCategory, pos: pos.pos + 1})}
	}
	return plugin.Page{Refs: p.refs(page.ids), Next: next}, nil
}

// refs 把源侧 id 列表包成契约的 WorkRef。
//
// Plugin 字段 MUST 等于 Name()（docs/PLUGIN-CONTRACT.md 硬性要求 6，框架会校验），
// ID 由框架当不透明键、不解析格式。
func (p *Plugin) refs(ids []string) []plugin.WorkRef {
	out := make([]plugin.WorkRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, plugin.WorkRef{Plugin: Name, ID: id})
	}
	return out
}

// sitemapURLs 取目录索引，按上游给出的顺序返回子 sitemap 的 loc。
func (p *Plugin) sitemapURLs(ctx context.Context) ([]string, error) {
	body, err := p.getSitemap(ctx, p.absolute(sitemapIndexPath))
	if err != nil {
		return nil, err
	}
	return parseSitemapIndex(body)
}

// parseSitemapIndex 解析 `<sitemapindex>`：按文档顺序取 `<sitemap><loc>`。
//
// 空索引按 ErrSchema：0 份子 sitemap 意味着总集为空，与「源可枚举 608,372 部」
// 的事实冲突，不能当成「源里没有作品」静默收场。
func parseSitemapIndex(body []byte) ([]string, error) {
	var idx struct {
		XMLName  xml.Name `xml:"sitemapindex"`
		Sitemaps []struct {
			Loc string `xml:"loc"`
		} `xml:"sitemap"`
	}
	if err := xml.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("%w: sitemap 索引解码失败: %v", plugin.ErrSchema, err)
	}
	urls := make([]string, 0, len(idx.Sitemaps))
	for _, s := range idx.Sitemaps {
		if loc := strings.TrimSpace(s.Loc); loc != "" {
			urls = append(urls, loc)
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("%w: sitemap 索引里没有子 sitemap", plugin.ErrSchema)
	}
	return urls, nil
}

// sitemapIDRE 是 sitemap 里剧目 loc 的两种形状（docs/L1-CORPUS-SCOPE.md 取证 B）：
// `/player/{id}` 与 `/detail?series_id={id}`。两种都是剧目页，id 就是剧目标识。
var sitemapIDRE = regexp.MustCompile(`^(?:https?://[^/]+)?(?:/player/|/detail\?series_id=)(\d+)$`)

// parseSitemapIDs 解析 `<urlset>` 里的全部剧目标识，保持上游顺序。
//
// 非剧目 loc（其它路径）跳过；但「有 loc 却一个剧目都认不出」按 ErrSchema：那说明
// 上游把剧目 URL 换形状了，静默返回空等于宣称「源里一部剧都没有」。
func parseSitemapIDs(body []byte) ([]string, error) {
	var set struct {
		XMLName xml.Name `xml:"urlset"`
		URLs    []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
	}
	if err := xml.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("%w: sitemap 解码失败: %v", plugin.ErrSchema, err)
	}
	ids := make([]string, 0, len(set.URLs))
	for _, u := range set.URLs {
		m := sitemapIDRE.FindStringSubmatch(strings.TrimSpace(u.Loc))
		if m == nil {
			continue
		}
		ids = append(ids, m[1])
	}
	if len(set.URLs) > 0 && len(ids) == 0 {
		return nil, fmt.Errorf("%w: sitemap %d 个 loc 里没有可识别的剧目地址", plugin.ErrSchema, len(set.URLs))
	}
	return ids, nil
}

// categoryPageData 是 `loaderData.category_page` 的最小结构（只取枚举要的字段）。
type categoryPageData struct {
	IsSuccess  bool `json:"isSuccess"`
	Pagination struct {
		PageNum    int `json:"pageNum"`
		TotalPages int `json:"totalPages"`
	} `json:"pagination"`
	RecommendList []struct {
		SeriesID string `json:"series_id"`
	} `json:"recommendList"`
}

// categoryPage 是 `/category` 一页的解析结果。
type categoryPage struct {
	ids        []string
	totalPages int
}

// parseCategoryPage 解析 `/category` 一页的剧目标识与总页数。
//
// wantPage 是本页的页码：上游把 `?page=N` 当成第 1 页返回（实测 `?page=35` 是 404，
// 但「忽略参数」这种退化没有硬保证）时，这里按 ErrSchema 打回，而不是把同一页
// 800 部重复灌一遍。
func parseCategoryPage(html string, wantPage int) (categoryPage, error) {
	data, err := loaderData(html)
	if err != nil {
		return categoryPage{}, err
	}
	raw, ok := data["category_page"]
	if !ok {
		return categoryPage{}, fmt.Errorf("%w: loaderData 缺 category_page", plugin.ErrSchema)
	}
	var page categoryPageData
	if err := json.Unmarshal(raw, &page); err != nil {
		return categoryPage{}, fmt.Errorf("%w: 解析 category_page: %v", plugin.ErrSchema, err)
	}
	if !page.IsSuccess {
		return categoryPage{}, fmt.Errorf("%w: category_page.isSuccess=false", plugin.ErrSchema)
	}
	if page.Pagination.TotalPages < 1 {
		return categoryPage{}, fmt.Errorf("%w: category_page 缺分页信息", plugin.ErrSchema)
	}
	if page.Pagination.PageNum != 0 && page.Pagination.PageNum != wantPage {
		return categoryPage{}, fmt.Errorf("%w: 请求第 %d 页，上游返回第 %d 页",
			plugin.ErrSchema, wantPage, page.Pagination.PageNum)
	}
	ids := make([]string, 0, len(page.RecommendList))
	for _, item := range page.RecommendList {
		if !numericID(item.SeriesID) {
			continue
		}
		ids = append(ids, item.SeriesID)
	}
	if len(page.RecommendList) > 0 && len(ids) == 0 {
		return categoryPage{}, fmt.Errorf("%w: category_page 的 %d 条推荐里没有合法 series_id",
			plugin.ErrSchema, len(page.RecommendList))
	}
	return categoryPage{ids: ids, totalPages: page.Pagination.TotalPages}, nil
}

// numericID 报告 id 是否是「全数字、长度 1..30」的源侧标识。与 vid_list 的校验
// 同一口径：非数字不可能是红果的 series_id / vid。
func numericID(id string) bool {
	if len(id) == 0 || len(id) > 30 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

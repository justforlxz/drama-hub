package hongguo

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// series 是列表页解析出的剧目元数据（内部结构，无需导出）。
type series struct {
	sid              string
	name             string
	cover            string
	intro            string
	firstVisibleTime string
	tags             []string
	vids             []string
	// protected 是页面声明的「这剧的网页通道只给 SEO 预览」（protectedPlayback）。
	// 它决定 Resolve 走不走网页通道，见 app.go 的取址顺序。
	protected bool
}

// routerDataRE 定位 _ROUTER_DATA 赋值的起始（match 以开括号 { 结尾，解码时从该
// 括号起按整个 JSON 值解）。实测页面有 <script>…_ROUTER_DATA = {…};
// 与普通 script 两种形态，window. 前缀可有可无。
var routerDataRE = regexp.MustCompile(`(?:window\.)?_ROUTER_DATA\s*=\s*\{`)

// ldJSONRE 提取 <script type="application/ld+json">…</script> 块体。
var ldJSONRE = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)

// parsePlayerPage 从列表页 HTML 解析剧目元数据。sid 是请求时的身份，页面里两处
// series_id 都必须与之一致——被锁的集会 302 到别的剧/别的集页面，身份不符一律
// plugin.ErrSchema。tags 缺失记为 nil（= 上游未声明）；vid_list 必须存在、非空、
// 元素全数字且无重复（下标即集号）。
func parsePlayerPage(html, sid string) (series, error) {
	page, err := playerPage(html)
	if err != nil {
		return series{}, err
	}
	if !page.IsSuccess {
		return series{}, fmt.Errorf("%w: isSuccess=false", plugin.ErrSchema)
	}
	if page.SeriesID != sid || page.SeriesDetail.SeriesID != sid {
		return series{}, fmt.Errorf("%w: 页面 series_id=%q 与请求 sid=%q 不符", plugin.ErrSchema, page.SeriesID, sid)
	}
	vids, err := validateVidList(page.SeriesDetail.VidList)
	if err != nil {
		return series{}, err
	}
	return series{
		sid:              page.SeriesID,
		name:             page.SeriesDetail.Name,
		cover:            page.SeriesDetail.Cover,
		intro:            page.SeriesDetail.Intro,
		firstVisibleTime: page.SeriesDetail.FirstVisibleTime,
		tags:             page.SeriesDetail.Tags,
		vids:             vids,
		protected:        page.ProtectedPlayback,
	}, nil
}

// parseDirectURL 从单集页 HTML 解析本集的直链。用 @type == "VideoObject" + 绑定
// 校验过滤：页面里另有推荐位其它剧的 JSON-LD 块，不匹配的跳过；都匹配不上才
// plugin.ErrSchema；VideoObject 存在但 contentUrl 缺失 → plugin.ErrNoStream。
func parseDirectURL(html, sid, vid string) (string, error) {
	page, err := playerPage(html)
	if err != nil {
		return "", err
	}
	if !page.IsSuccess {
		return "", fmt.Errorf("%w: isSuccess=false", plugin.ErrSchema)
	}
	if page.SeriesID != sid {
		return "", fmt.Errorf("%w: 页面 series_id=%q 与请求 sid=%q 不符", plugin.ErrSchema, page.SeriesID, sid)
	}

	for _, block := range ldJSONRE.FindAllStringSubmatch(html, -1) {
		attr := block[1]
		if !strings.Contains(attr, "application/ld+json") {
			continue
		}
		var v videoObject
		if err := json.Unmarshal([]byte(block[2]), &v); err != nil {
			continue // 非 VideoObject 块（@graph 等）可能解不进我们的结构，跳过
		}
		if v.Type != "VideoObject" {
			continue
		}
		if !videoBound(v, sid, vid) {
			continue // 别的剧/别的集的块
		}
		if v.ContentURL == "" {
			return "", plugin.ErrNoStream
		}
		u, err := normalizeURL(v.ContentURL)
		if err != nil {
			return "", fmt.Errorf("%w: contentUrl 非法: %v", plugin.ErrSchema, err)
		}
		return u, nil
	}
	return "", fmt.Errorf("%w: 无绑定本集(%s/%s)的 VideoObject", plugin.ErrSchema, sid, vid)
}

// loaderData 从 HTML 里解出 _ROUTER_DATA.loaderData（key → 原始 JSON 载荷）。
//
// 三种页面（列表页、单集页、`/category`）共用一个外壳，只是 key 不同；取壳的一步
// 放在这里，各页面的解析各自按 key 取自己的载荷。
func loaderData(html string) (map[string]json.RawMessage, error) {
	loc := routerDataRE.FindStringIndex(html)
	if loc == nil {
		return nil, fmt.Errorf("%w: 缺 _ROUTER_DATA", plugin.ErrSchema)
	}
	dec := json.NewDecoder(strings.NewReader(html[loc[1]-1:]))
	var router struct {
		LoaderData map[string]json.RawMessage `json:"loaderData"`
	}
	if err := dec.Decode(&router); err != nil {
		return nil, fmt.Errorf("%w: _ROUTER_DATA JSON 解码失败: %v", plugin.ErrSchema, err)
	}
	if len(router.LoaderData) == 0 {
		return nil, fmt.Errorf("%w: _ROUTER_DATA 缺 loaderData", plugin.ErrSchema)
	}
	return router.LoaderData, nil
}

// playerPage 解出 player 页的载荷：按 key 顺序取第一个存在的 page。
func playerPage(html string) (playerPageData, error) {
	loader, err := loaderData(html)
	if err != nil {
		return playerPageData{}, err
	}
	// 列表页与单集页两个 key 同构，按固定优先序取第一个存在的：列表页优先，
	// 单集页兜底（实测两类页面各只有其中一个 key）。
	for _, key := range []string{"player_(series_id)/page", "player_(series_id)/(vid)/page"} {
		raw, ok := loader[key]
		if !ok {
			continue
		}
		var page playerPageData
		if err := json.Unmarshal(raw, &page); err != nil {
			return playerPageData{}, fmt.Errorf("%w: 解析 %s: %v", plugin.ErrSchema, key, err)
		}
		return page, nil
	}
	return playerPageData{}, fmt.Errorf("%w: loaderData 缺 player_(series_id)/page", plugin.ErrSchema)
}

// validateVidList 校验 vid_list：非空、元素全数字（1..30 位）、无重复。
func validateVidList(vids []string) ([]string, error) {
	if len(vids) == 0 {
		return nil, fmt.Errorf("%w: vid_list 为空", plugin.ErrSchema)
	}
	seen := make(map[string]bool, len(vids))
	for _, v := range vids {
		if len(v) == 0 || len(v) > 30 {
			return nil, fmt.Errorf("%w: vid %q 长度非法", plugin.ErrSchema, v)
		}
		for _, c := range v {
			if c < '0' || c > '9' {
				return nil, fmt.Errorf("%w: vid %q 含非数字字符", plugin.ErrSchema, v)
			}
		}
		if seen[v] {
			return nil, fmt.Errorf("%w: vid %q 重复", plugin.ErrSchema, v)
		}
		seen[v] = true
	}
	return vids, nil
}

// videoBound 校验一个 VideoObject 是否绑定到 (sid, vid)：
// url 的 pathname 是 /player/{sid} 或 /player/{sid}/{vid}，且 contentUrl 的
// pathname 含 /{vid}（实测形如 /video/public/{vid}/360p.mp4）。
// contentUrl 为空时跳过绑定校验：它仍属于本集，缺失与否由调用方决定。
func videoBound(v videoObject, sid, vid string) bool {
	u, err := url.Parse(v.URL)
	if err != nil {
		return false
	}
	if u.Path != "/player/"+sid && u.Path != "/player/"+sid+"/"+vid {
		return false
	}
	if v.ContentURL == "" {
		return true
	}
	normalized, err := normalizeURL(v.ContentURL)
	if err != nil {
		return false
	}
	c, err := url.Parse(normalized)
	if err != nil {
		return false
	}
	return strings.Contains(c.Path, "/"+vid)
}

// normalizeURL 规整上游给的直链：还原 \u002F / \u0026、HTML 实体 &amp;，删除
// Range 参数与尾部空 query。返回能成功 url.Parse 的 URL。
func normalizeURL(raw string) (string, error) {
	s := raw
	s = strings.ReplaceAll(s, `\u002F`, "/")
	s = strings.ReplaceAll(s, `\u0026`, "&")
	s = strings.ReplaceAll(s, "&amp;", "&")

	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k := range q {
		if strings.EqualFold(k, "Range") {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	if u.RawQuery == "" {
		u.ForceQuery = false // 删完参数后不留裸问号
	}
	return u.String(), nil
}

// playerPageData 是 player_(series_id)/(vid)/page 载荷的最小结构。
type playerPageData struct {
	IsSuccess bool   `json:"isSuccess"`
	SeriesID  string `json:"series_id"`
	// ProtectedPlayback 是页面声明的「这剧的网页通道只给 10 秒 SEO 预览」。
	//
	// 它在 **page 级**，不在 seriesDetail 里（MKACG-637 实测抓包）：
	// loaderData["player_(series_id)/page"].protectedPlayback，整页只出现一次，
	// seriesDetail 下没有这个键。读错层级会让 protected 恒为 false，网页通道的
	// 10.07 秒预览被当成完整片源交付（MKACG-625 F）。
	ProtectedPlayback bool `json:"protectedPlayback"`
	SeriesDetail      struct {
		SeriesID         string   `json:"series_id"`
		Name             string   `json:"series_name"`
		Cover            string   `json:"series_cover"`
		Intro            string   `json:"series_intro"`
		FirstVisibleTime string   `json:"first_visible_time"`
		Tags             []string `json:"tags"`
		VidList          []string `json:"vid_list"`
	} `json:"seriesDetail"`
}

// videoObject 是 JSON-LD VideoObject 块的最小结构。
type videoObject struct {
	Type       string `json:"@type"`
	URL        string `json:"url"`
	ContentURL string `json:"contentUrl"`
}

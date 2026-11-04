package web

import (
	"bytes"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// parseDoc 把响应体解析成 DOM。页面断言走结构而不是字符串片段：
// 断「body 的 data-page」与「导航链接与当前页」这类 JS 与模板之间的契约。
func parseDoc(t *testing.T, body []byte) *html.Node {
	t.Helper()
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("页面不是合法 HTML: %v", err)
	}
	return doc
}

// findNode 深度优先找第一个满足条件的节点。
func findNode(n *html.Node, match func(*html.Node) bool) *html.Node {
	if match(n) {
		return n
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if found := findNode(child, match); found != nil {
			return found
		}
	}
	return nil
}

// hasClass 判断元素是否带某个 class。
func hasClass(n *html.Node, class string) bool {
	for _, attr := range n.Attr {
		if attr.Key != "class" {
			continue
		}
		for _, field := range bytes.Fields([]byte(attr.Val)) {
			if string(field) == class {
				return true
			}
		}
	}
	return false
}

// attrOf 取元素属性值，缺失返回空串。
func attrOf(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

// hasAttr 判断布尔属性是否存在（hidden 这类属性的值是空串，不能按值判断）。
func hasAttr(n *html.Node, key string) bool {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return true
		}
	}
	return false
}

// isElement 判断节点是不是指定标签。
func isElement(n *html.Node, tag string) bool {
	return n.Type == html.ElementNode && n.Data == tag
}

// byID 找 id 等于给定值的元素。
func byID(n *html.Node, id string) *html.Node {
	return findNode(n, func(node *html.Node) bool { return attrOf(node, "id") == id })
}

// TestPages 覆盖页面集合：200 + text/html + 精确 Content-Length，body[data-page]
// 与页面一致（JS 靠它分派），导航恰为四页且只有当前页带 aria-current。
func TestPages(t *testing.T) {
	srv, st, _ := newTestServer(t)
	dramaID := seedDrama(t, st, "9101", "导航用剧", []string{"n1"})
	cases := []struct{ path, page, current string }{
		{"/", "overview", "/"},
		{"/library", "library", "/library"},
		{"/categories", "categories", "/categories"},
		{"/settings", "settings", "/settings"},
		{"/library/" + strconv.FormatInt(dramaID, 10), "drama", "/library"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			rec := do(t, srv, http.MethodGet, c.path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s 状态码 = %d, 期望 200", c.path, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
				t.Errorf("Content-Type = %q", ct)
			}
			if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
				t.Errorf("Content-Length = %q, body = %d 字节", cl, rec.Body.Len())
			}

			doc := parseDoc(t, rec.Body.Bytes())
			body := findNode(doc, func(n *html.Node) bool { return isElement(n, "body") })
			if body == nil {
				t.Fatal("页面没有 body")
			}
			if got := attrOf(body, "data-page"); got != c.page {
				t.Errorf("body[data-page] = %q, 期望 %q", got, c.page)
			}

			nav := findNode(doc, func(n *html.Node) bool { return isElement(n, "nav") && hasClass(n, "tabs") })
			if nav == nil {
				t.Fatal("页面没有导航")
			}
			var hrefs []string
			current := ""
			for child := nav.FirstChild; child != nil; child = child.NextSibling {
				if !isElement(child, "a") {
					continue
				}
				href := attrOf(child, "href")
				hrefs = append(hrefs, href)
				if attrOf(child, "aria-current") == "page" {
					current = href
				}
			}
			want := []string{"/", "/library", "/categories", "/settings"}
			if strings.Join(hrefs, ",") != strings.Join(want, ",") {
				t.Fatalf("导航链接 = %v, 期望 %v", hrefs, want)
			}
			if current != c.current {
				t.Errorf("当前页高亮 = %q, 期望 %q", current, c.current)
			}
		})
	}
}

// TestNoTables 锁「删表就是移动端策略」：所有页面的 <table> 计数都是 0
// （台账用 ARIA table 语义 + CSS Grid，源与媒体库用卡片、键值用 dl）。
func TestNoTables(t *testing.T) {
	srv, st, _ := newTestServer(t)
	dramaID := seedDrama(t, st, "9102", "台账用剧", []string{"t1"})
	paths := []string{"/", "/library", "/categories", "/settings", "/library/" + strconv.FormatInt(dramaID, 10)}
	for _, path := range paths {
		doc := parseDoc(t, do(t, srv, http.MethodGet, path, "").Body.Bytes())
		if table := findNode(doc, func(n *html.Node) bool { return isElement(n, "table") }); table != nil {
			t.Errorf("GET %s 仍有 <table>：移动端策略是删表，不是横向滚动", path)
		}
	}
}

// TestOverviewPage 锁定概览页的健康带：摘要、播放源容器、Jellyfin 进度与
// 同步按钮、写操作提示 —— 少一个，app.js 上对应的动作就静默失效。
// 补录表单已迁到剧库页，这里反向锁「概览不再有补录入口」。
func TestOverviewPage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	doc := parseDoc(t, do(t, srv, http.MethodGet, "/", "").Body.Bytes())

	for _, id := range []string{"summary", "notice", "sources", "jellyfin-progress", "jellyfin-sync", "admin-token", "write-hint", "playback", "playback-empty"} {
		if byID(doc, id) == nil {
			t.Errorf("概览页缺少 #%s", id)
		}
	}
	if progress := byID(doc, "jellyfin-progress"); progress != nil && !isElement(progress, "p") {
		t.Errorf("#jellyfin-progress 是 %s, 期望 p（664 的进度行）", progress.Data)
	}
	if empty := byID(doc, "playback-empty"); empty != nil && !isElement(empty, "p") {
		t.Errorf("#playback-empty 是 %s, 期望 p（空态文案）", empty.Data)
	}
	if lookup := byID(doc, "lookup"); lookup != nil {
		t.Error("补录入口应只在剧库页，#lookup 不该再出现在概览")
	}
}

// TestLibraryPage 锁定剧库页的工具条、补录表单迁移与台账的 ARIA 结构：
// role=table + 九列 columnheader（顺序由 paging.js 的 LEDGER_COLUMNS 对齐）。
func TestLibraryPage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	doc := parseDoc(t, do(t, srv, http.MethodGet, "/library", "").Body.Bytes())

	for _, id := range []string{
		"library-summary", "notice", "library-search", "library-genre", "library-filter",
		"library-view-list", "library-view-wall", "lookup-toggle",
		"lookup", "drama-form", "series-id", "drama-submit", "drama-notice",
		"library-ledger", "library-rows", "library-wall", "library-empty",
		"library-pager", "pager-prev", "pager-info", "pager-next",
	} {
		if byID(doc, id) == nil {
			t.Errorf("剧库页缺少 #%s", id)
		}
	}
	if lookup := byID(doc, "lookup"); lookup != nil && !isElement(lookup, "details") {
		t.Errorf("#lookup 是 %s, 期望 details（补录默认收起）", lookup.Data)
	}
	if form := byID(doc, "drama-form"); form != nil && !isElement(form, "form") {
		t.Errorf("#drama-form 是 %s, 期望 form", form.Data)
	}

	ledger := byID(doc, "library-ledger")
	if ledger == nil {
		t.Fatal("剧库页没有台账 #library-ledger")
	}
	if got := attrOf(ledger, "role"); got != "table" {
		t.Errorf("#library-ledger role = %q, 期望 table", got)
	}
	head := findNode(ledger, func(n *html.Node) bool { return attrOf(n, "role") == "row" })
	if head == nil {
		t.Fatal("台账没有表头行 role=row")
	}
	var headers []string
	for child := head.FirstChild; child != nil; child = child.NextSibling {
		if attrOf(child, "role") == "columnheader" {
			headers = append(headers, strings.TrimSpace(legendText(child)))
		}
	}
	want := []string{"封面", "标题", "年份", "分类", "集数", "上游 id", "上次同步", "Jellyfin", "操作"}
	if strings.Join(headers, ",") != strings.Join(want, ",") {
		t.Errorf("台账列 = %v, 期望 %v", headers, want)
	}
	if rows := byID(doc, "library-rows"); rows == nil || attrOf(rows, "role") != "rowgroup" {
		t.Error("台账行容器 #library-rows 应是 role=rowgroup")
	}
	if wall := byID(doc, "library-wall"); wall != nil && !hasAttr(wall, "hidden") {
		t.Error("海报墙默认应隐藏（列表是默认视图）")
	}
}

// TestTokenDialog 锁定按需解锁的令牌 UX：顶栏不再常驻密码框，令牌输入框
// 只在原生 dialog 里；每页都由 base.html 提供同一份弹窗。
func TestTokenDialog(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{"/", "/library", "/categories", "/settings"} {
		doc := parseDoc(t, do(t, srv, http.MethodGet, path, "").Body.Bytes())

		dialog := byID(doc, "token-dialog")
		if dialog == nil {
			t.Fatalf("GET %s 缺少 #token-dialog", path)
		}
		if !isElement(dialog, "dialog") {
			t.Errorf("GET %s 的 #token-dialog 是 %s, 期望原生 dialog", path, dialog.Data)
		}
		if byID(dialog, "admin-token") == nil {
			t.Errorf("GET %s 的令牌弹窗里缺少 #admin-token", path)
		}
		for _, id := range []string{"token-button", "token-state", "token-form", "token-clear", "token-save", "token-notice"} {
			if byID(doc, id) == nil {
				t.Errorf("GET %s 缺少 #%s", path, id)
			}
		}

		topbar := findNode(doc, func(n *html.Node) bool { return isElement(n, "header") && hasClass(n, "topbar") })
		if topbar == nil {
			t.Fatalf("GET %s 没有顶栏", path)
		}
		if input := findNode(topbar, func(n *html.Node) bool { return isElement(n, "input") }); input != nil {
			t.Errorf("GET %s 顶栏仍有常驻输入框（AC-10 要求改为按需解锁弹窗）", path)
		}
	}
}

// eachNode 深度优先遍历所有节点。
func eachNode(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		eachNode(child, visit)
	}
}

// TestScriptIDsExistInTemplates 锁 app.js 与模板之间的 id 契约：脚本里每个
// getElementById 字面量都得在某个页面的 DOM 里出现 —— 否则那个动作会静默失效。
func TestScriptIDsExistInTemplates(t *testing.T) {
	srv, st, _ := newTestServer(t)
	dramaID := seedDrama(t, st, "9103", "契约用剧", []string{"c1"})

	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	refs := make(map[string]bool)
	for _, m := range regexp.MustCompile(`getElementById\('([^']+)'\)`).FindAllStringSubmatch(string(js), -1) {
		refs[m[1]] = true
	}
	if len(refs) == 0 {
		t.Fatal("没在 app.js 里找到 getElementById 字面量，扫描规则可能失效了")
	}

	ids := make(map[string]bool)
	paths := []string{"/", "/library", "/categories", "/settings", "/library/" + strconv.FormatInt(dramaID, 10)}
	for _, path := range paths {
		doc := parseDoc(t, do(t, srv, http.MethodGet, path, "").Body.Bytes())
		eachNode(doc, func(n *html.Node) {
			if id := attrOf(n, "id"); id != "" {
				ids[id] = true
			}
		})
	}
	for ref := range refs {
		if !ids[ref] {
			t.Errorf("app.js 引用了 #%s，但任何页面模板里都没有这个 id", ref)
		}
	}
}

// TestSettingsPageForm 锁定设置页：一个表单、五个分组、每个可改项都带
// data-timing（即时 / ≤60 秒 / ≤5 分钟），并保留可折叠的只读运行参数。
func TestSettingsPageForm(t *testing.T) {
	srv, _, _ := newTestServer(t)
	doc := parseDoc(t, do(t, srv, http.MethodGet, "/settings", "").Body.Bytes())

	form := byID(doc, "settings-form")
	if form == nil || !isElement(form, "form") {
		t.Fatal("/settings 没有 #settings-form 表单")
	}
	if byID(doc, "admin-token") == nil {
		t.Error("设置页没有管理令牌输入框")
	}
	if readonly := byID(doc, "readonly"); readonly == nil {
		t.Error("设置页缺少只读运行参数 #readonly")
	}

	// 输入框 ↔ 生效时机。data-timing 是机器读的稳定标识，徽标文字是给人看的。
	fields := map[string]string{
		"set-webdav-user":      "immediate",
		"set-webdav-pass":      "immediate",
		"set-strm-prefix":      "immediate",
		"set-refresh-interval": "refresh",
		"set-merge-chunk-size": "immediate",
		"set-cache-max-mb":     "evict",
		"set-cache-lru-days":   "evict",
		"set-jellyfin-url":     "immediate",
		"set-jellyfin-api-key": "immediate",
		"set-jellyfin-root":    "immediate",
		"set-jellyfin-prefix":  "immediate",
		"set-jellyfin-timeout": "immediate",
	}
	for id, timing := range fields {
		input := byID(doc, id)
		if input == nil || !isElement(input, "input") {
			t.Errorf("设置页缺少输入框 #%s", id)
			continue
		}
		field := input.Parent
		for field != nil && !hasClass(field, "field") {
			field = field.Parent
		}
		badgeNode := findNode(field, func(n *html.Node) bool { return hasClass(n, "badge") })
		if badgeNode == nil {
			t.Errorf("#%s 没有生效时机徽标", id)
			continue
		}
		if got := attrOf(badgeNode, "data-timing"); got != timing {
			t.Errorf("#%s 的 data-timing = %q, 期望 %q", id, got, timing)
		}
	}

	// 分组：按「一起改的东西」切，五组一个不少。
	var legends []string
	for n := form.FirstChild; n != nil; n = n.NextSibling {
		if isElement(n, "fieldset") {
			if legend := findNode(n, func(c *html.Node) bool { return isElement(c, "legend") }); legend != nil {
				legends = append(legends, strings.TrimSpace(legendText(legend)))
			}
		}
	}
	want := []string{"访问", "同步与刷新", "合并与对外集号", "缓存", "Jellyfin"}
	if strings.Join(legends, ",") != strings.Join(want, ",") {
		t.Errorf("分组 = %v, 期望 %v", legends, want)
	}
}

// legendText 取 legend 的纯文本。
func legendText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return b.String()
}

// TestStaticAssets 覆盖内嵌样式与脚本：两种后缀各自的内容类型、精确长度；
// 其它后缀与不存在的文件都是 404。
func TestStaticAssets(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, c := range []struct{ path, contentType string }{
		{"/static/app.css", "text/css; charset=utf-8"},
		{"/static/app.js", "text/javascript; charset=utf-8"},
	} {
		rec := do(t, srv, http.MethodGet, c.path, "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s 状态码 = %d, 期望 200", c.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != c.contentType {
			t.Errorf("GET %s Content-Type = %q, 期望 %q", c.path, ct, c.contentType)
		}
		if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
			t.Errorf("GET %s Content-Length = %q, body = %d 字节", c.path, cl, rec.Body.Len())
		}
	}

	for _, path := range []string{"/static/app.txt", "/static/missing.js", "/static/a/b.js"} {
		if rec := do(t, srv, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 404", path, rec.Code)
		}
	}
}

// TestStyleContract 锁定设计系统的两块不可回归项：深色只覆盖颜色（这里至少
// 保证有 prefers-color-scheme: dark 媒体查询）与减少动效分支；且样式表里没有
// 动画关键帧（不做炫技）。
func TestStyleContract(t *testing.T) {
	body, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("读 app.css: %v", err)
	}
	css := string(body)
	for _, want := range []string{"prefers-color-scheme: dark", "prefers-reduced-motion: reduce", ":root"} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css 缺少 %q", want)
		}
	}
	if strings.Contains(css, "@keyframes") {
		t.Error("app.css 不该有动画关键帧")
	}

	// 视图切换靠 element.hidden = …；.wall{display:grid} / .pager{display:flex} 是
	// author 声明，会盖掉 UA 的 [hidden]{display:none}，所以必须有一条显式覆盖
	// （MKACG-675 评审阻断项 1）。没有它，列表视图下海报墙照样整片渲染、单页也显示分页条。
	//
	// 先剥注释再匹配：注释里那句「盖掉 UA 的 [hidden] { display: none }」示例本身
	// 就满足这条正则，不剥的话真规则被删掉也照样绿（评审实测过的空转断言）。
	code := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	hiddenRule := regexp.MustCompile(`\[hidden\][^{]*\{[^}]*display:\s*none`)
	if !hiddenRule.MatchString(code) {
		t.Error("app.css 缺少 [hidden]{display:none}：.wall / .pager 的 display 会让隐藏视图仍然显示")
	}
	// 反向自检：从剥完注释的 CSS 里删掉这条规则，正则必须不再命中 ——
	// 保证上一条断言真的在守它要守的东西，而不是被注释或别的文本蒙混过关。
	withoutHidden := regexp.MustCompile(`\[hidden\]\s*\{[^}]*\}`).ReplaceAllString(code, "")
	if hiddenRule.MatchString(withoutHidden) {
		t.Error("删掉 [hidden] 规则后正则仍然命中：断言空转，无法防住回归")
	}
	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	for _, want := range []string{"ledger.hidden = wall", "wallBox.hidden = !wall", "pager.hidden ="} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js 缺少 %q：视图/分页的显隐开关与 CSS 契约对不上", want)
		}
	}
}

// TestUnknownPaths 锁定管理面之外的路径：未登记路径仍是 404，剧详情页只接受
// 合法 id（非数字、不存在的剧、空 id 都是 404），页面只接受 GET。
func TestUnknownPaths(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{"/nope", "/merges", "/merges/x", "/status", "/status/", "/static", "/library/abc", "/library/0", "/library/999999", "/library/"} {
		if rec := do(t, srv, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 404", path, rec.Code)
		}
	}
	if rec := do(t, srv, http.MethodGet, "/library", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /library 状态码 = %d, 期望 200", rec.Code)
	}
	if rec := do(t, srv, http.MethodPost, "/settings", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /settings 状态码 = %d, 期望 405", rec.Code)
	}
}

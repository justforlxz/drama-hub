package web

import (
	"bytes"
	"net/http"
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

// isElement 判断节点是不是指定标签。
func isElement(n *html.Node, tag string) bool {
	return n.Type == html.ElementNode && n.Data == tag
}

// byID 找 id 等于给定值的元素。
func byID(n *html.Node, id string) *html.Node {
	return findNode(n, func(node *html.Node) bool { return attrOf(node, "id") == id })
}

// TestPages 覆盖页面集合：200 + text/html + 精确 Content-Length，body[data-page]
// 与页面一致（JS 靠它分派），导航恰为 / 与 /settings 且只有当前页带 aria-current。
func TestPages(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cases := []struct{ path, page, current string }{
		{"/", "overview", "/"},
		{"/settings", "settings", "/settings"},
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
			want := []string{"/", "/settings"}
			if strings.Join(hrefs, ",") != strings.Join(want, ",") {
				t.Fatalf("导航链接 = %v, 期望 %v", hrefs, want)
			}
			if current != c.current {
				t.Errorf("当前页高亮 = %q, 期望 %q", current, c.current)
			}
		})
	}
}

// TestNoTables 锁「删表就是移动端策略」：两页的 <table> 计数都是 0
// （源与媒体库用卡片、键值用 dl）。
func TestNoTables(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{"/", "/settings"} {
		doc := parseDoc(t, do(t, srv, http.MethodGet, path, "").Body.Bytes())
		if table := findNode(doc, func(n *html.Node) bool { return isElement(n, "table") }); table != nil {
			t.Errorf("GET %s 仍有 <table>：移动端策略是删表，不是横向滚动", path)
		}
	}
}

// TestOverviewPage 锁定概览页的操作落点：摘要、播放源容器、Jellyfin 进度与
// 同步按钮、补录表单、管理令牌框 —— 少一个，app.js 上对应的动作就静默失效。
func TestOverviewPage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	doc := parseDoc(t, do(t, srv, http.MethodGet, "/", "").Body.Bytes())

	for _, id := range []string{"summary", "notice", "sources", "jellyfin-progress", "jellyfin-sync", "drama-form", "series-id", "drama-submit", "admin-token", "write-hint"} {
		if byID(doc, id) == nil {
			t.Errorf("概览页缺少 #%s", id)
		}
	}
	if progress := byID(doc, "jellyfin-progress"); progress != nil && !isElement(progress, "p") {
		t.Errorf("#jellyfin-progress 是 %s, 期望 p（664 的进度行）", progress.Data)
	}
	if form := byID(doc, "drama-form"); form != nil && !isElement(form, "form") {
		t.Errorf("#drama-form 是 %s, 期望 form", form.Data)
	}
	if details := byID(doc, "lookup"); details == nil || !isElement(details, "details") {
		t.Error("概览页缺少 <details id=lookup>：补录入口应该默认收起")
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
}

// TestUnknownPaths 锁定管理面之外的路径：未登记路径仍是 404（旧的 /status 不再
// 是页面），页面只接受 GET。
func TestUnknownPaths(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{"/nope", "/merges", "/merges/x", "/status", "/status/", "/static"} {
		if rec := do(t, srv, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 404", path, rec.Code)
		}
	}
	if rec := do(t, srv, http.MethodPost, "/settings", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /settings 状态码 = %d, 期望 405", rec.Code)
	}
}

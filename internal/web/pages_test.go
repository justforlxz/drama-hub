package web

import (
	"bytes"
	"net/http"
	"os"
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

// isElement 判断节点是不是指定标签。
func isElement(n *html.Node, tag string) bool {
	return n.Type == html.ElementNode && n.Data == tag
}

// byID 找 id 等于给定值的元素。
func byID(n *html.Node, id string) *html.Node {
	return findNode(n, func(node *html.Node) bool { return attrOf(node, "id") == id })
}

// eachNode 深度优先遍历，visit 返回 false 时继续下一个。
func eachNode(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		eachNode(child, visit)
	}
}

// legendText 收 legend 里的全部文本（视觉上隐藏也算）。
func legendText(n *html.Node) string {
	var b strings.Builder
	eachNode(n, func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
	})
	return strings.Join(strings.Fields(b.String()), " ")
}

// fetchPage 拉一页并解析。
func fetchPage(t *testing.T, srv *Server, path string) *html.Node {
	t.Helper()
	rec := do(t, srv, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d, 期望 200", path, rec.Code)
	}
	return parseDoc(t, rec.Body.Bytes())
}

// TestPages 覆盖页面集合：200 + text/html + 精确 Content-Length，body[data-page]
// 与页面一致（JS 靠它分派），导航恰为三页且只有当前页带 aria-current。
func TestPages(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cases := []struct{ path, page, current string }{
		{"/", "overview", "/"},
		{"/sync", "sync", "/sync"},
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

			nav := findNode(doc, func(n *html.Node) bool { return isElement(n, "nav") && hasClass(n, "nav") })
			if nav == nil {
				t.Fatal("页面没有导航")
			}
			var hrefs []string
			var current []string
			eachNode(nav, func(n *html.Node) {
				if !isElement(n, "a") {
					return
				}
				href := attrOf(n, "href")
				hrefs = append(hrefs, href)
				if attrOf(n, "aria-current") == "page" {
					current = append(current, href)
				}
			})
			want := []string{"/", "/sync", "/settings"}
			if strings.Join(hrefs, ",") != strings.Join(want, ",") {
				t.Errorf("导航 = %v, 期望 %v", hrefs, want)
			}
			if len(current) != 1 || current[0] != c.current {
				t.Errorf("aria-current 链接 = %v, 期望只有 %q", current, c.current)
			}
		})
	}
}

// TestNoTables 守住「管理面不出表格」：台账改成卡片式列表后不应回退。
func TestNoTables(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{"/", "/sync", "/settings"} {
		doc := fetchPage(t, srv, path)
		if table := findNode(doc, func(n *html.Node) bool { return isElement(n, "table") }); table != nil {
			t.Errorf("%s 出现 <table>，管理面应为卡片式列表", path)
		}
	}
}

// TestOverviewPage 锁定概览页的三个信息块（系统状态、内容同步、正在播放），
// 并守住「概览不出现 Jellyfin 库管理」这条信息架构边界。
func TestOverviewPage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	doc := fetchPage(t, srv, "/")

	for _, id := range []string{
		"notice", "write-hint",
		"system-status", "system-badge", "system-title", "system-copy", "sync-state", "source-state",
		"sync-badge", "summary-dramas", "summary-episodes", "summary-emby", "summary",
		"sync-last-run", "sync-last-ok", "sync-next",
		"playback", "playback-streams", "playback-summary", "playback-badge", "playback-empty", "playback-list",
		"admin-token",
	} {
		if byID(doc, id) == nil {
			t.Errorf("概览页缺少 #%s", id)
		}
	}

	empty := byID(doc, "playback-empty")
	if empty == nil || !isElement(empty, "p") {
		t.Errorf("#playback-empty 必须是 <p>（无播放时显示空态）")
	}

	for _, id := range []string{"jellyfin-sync", "jellyfin-categories", "library-ledger", "lookup"} {
		if byID(doc, id) != nil {
			t.Errorf("概览页不应出现 #%s（库管理已移到 /sync）", id)
		}
	}
}

// TestSyncPage 锁定同步页：Jellyfin 摘要、托管库列表、二次确认对话框。
func TestSyncPage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	doc := fetchPage(t, srv, "/sync")

	for _, id := range []string{
		"notice", "write-hint",
		"jellyfin-summary", "jellyfin-badge", "jellyfin-server", "jellyfin-when", "jellyfin-last",
		"jellyfin-result", "jellyfin-progress", "jellyfin-issues", "jellyfin-hint",
		"jellyfin-sync", "jellyfin-categories",
		"confirm-dialog", "confirm-text", "confirm-ok", "confirm-cancel",
		"admin-token",
	} {
		if byID(doc, id) == nil {
			t.Errorf("同步页缺少 #%s", id)
		}
	}

	if progress := byID(doc, "jellyfin-progress"); !isElement(progress, "p") {
		t.Errorf("#jellyfin-progress 必须是 <p>（可空文本区域）")
	}
	if syncBtn := byID(doc, "jellyfin-sync"); !isElement(syncBtn, "button") {
		t.Errorf("#jellyfin-sync 必须是 <button>")
	}
	if dialog := byID(doc, "confirm-dialog"); !isElement(dialog, "dialog") {
		t.Errorf("#confirm-dialog 必须是原生 <dialog>")
	}
}

// TestTokenDialog 守住令牌弹窗契约：原生 dialog、令牌输入在弹窗里、
// 顶部工具条不能有输入框（避免密码框常驻可见）。
func TestTokenDialog(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{"/", "/sync", "/settings"} {
		doc := fetchPage(t, srv, path)

		dialog := byID(doc, "token-dialog")
		if !isElement(dialog, "dialog") {
			t.Fatalf("%s: #token-dialog 必须是原生 <dialog>", path)
		}
		if byID(dialog, "admin-token") == nil {
			t.Errorf("%s: 令牌输入框应在 #token-dialog 内", path)
		}
		for _, id := range []string{"token-button", "token-state", "token-form", "token-clear", "token-save", "token-notice"} {
			if byID(doc, id) == nil {
				t.Errorf("%s: 缺少 #%s", path, id)
			}
		}

		toolbar := findNode(doc, func(n *html.Node) bool { return isElement(n, "header") && hasClass(n, "toolbar") })
		if toolbar == nil {
			t.Fatalf("%s: 缺少 header.toolbar", path)
		}
		if input := findNode(toolbar, func(n *html.Node) bool { return isElement(n, "input") }); input != nil {
			t.Errorf("%s: 顶部工具条里不应有输入框", path)
		}
	}
}

// TestScriptIDsExistInTemplates 扫 app.js 里 getElementById('...') 的字面量，
// 要求每个 id 都能在某个页面里找到 —— 脚本与模板的 id 契约不能靠运行时才暴露。
func TestScriptIDsExistInTemplates(t *testing.T) {
	script, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	pattern := regexp.MustCompile(`getElementById\('([^']+)'\)`)
	ids := map[string]bool{}
	for _, match := range pattern.FindAllStringSubmatch(string(script), -1) {
		ids[match[1]] = true
	}
	if len(ids) == 0 {
		t.Fatal("app.js 里没找到 getElementById 字面量")
	}

	srv, _, _ := newTestServer(t)
	seen := map[string]bool{}
	for _, path := range []string{"/", "/sync", "/settings"} {
		doc := fetchPage(t, srv, path)
		eachNode(doc, func(n *html.Node) {
			if id := attrOf(n, "id"); id != "" {
				seen[id] = true
			}
		})
	}
	for id := range ids {
		if !seen[id] {
			t.Errorf("app.js 引用 #%s，但三个页面都没有这个元素", id)
		}
	}
}

// TestSettingsPageForm 锁定设置页的五组字段、生效时机徽标与漏填语义。
func TestSettingsPageForm(t *testing.T) {
	srv, _, _ := newTestServer(t)
	doc := fetchPage(t, srv, "/settings")

	form := byID(doc, "settings-form")
	if !isElement(form, "form") {
		t.Fatal("#settings-form 必须是 <form>")
	}
	if byID(doc, "readonly") == nil {
		t.Error("缺少只读运行参数 #readonly")
	}

	timing := map[string]string{
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
	for id, want := range timing {
		input := byID(doc, id)
		if input == nil {
			t.Errorf("缺少输入框 #%s", id)
			continue
		}
		field := findNode(doc, func(n *html.Node) bool {
			if !hasClass(n, "field") {
				return false
			}
			return byID(n, id) != nil
		})
		if field == nil {
			t.Errorf("#%s 不在 .field 里", id)
			continue
		}
		badge := findNode(field, func(n *html.Node) bool {
			return hasClass(n, "badge") && attrOf(n, "data-timing") == want
		})
		if badge == nil {
			t.Errorf("#%s 所在 .field 缺少 .badge[data-timing=%q]", id, want)
		}
	}

	var legends []string
	for child := form.FirstChild; child != nil; child = child.NextSibling {
		if !isElement(child, "fieldset") {
			continue
		}
		legend := findNode(child, func(n *html.Node) bool { return isElement(n, "legend") })
		if legend == nil {
			t.Fatal("fieldset 缺少 legend")
		}
		legends = append(legends, legendText(legend))
	}
	want := []string{"访问", "同步与刷新", "合并与对外集号", "缓存", "Jellyfin"}
	if strings.Join(legends, ",") != strings.Join(want, ",") {
		t.Errorf("设置分组 = %v, 期望 %v", legends, want)
	}

	if byID(doc, "settings-save") == nil || byID(doc, "settings-reset") == nil {
		t.Error("缺少保存/重置按钮")
	}
}

// TestStaticAssets 守住静态资源只放行 .css / .js、且长度精确（内嵌文件）。
func TestStaticAssets(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for name, contentType := range map[string]string{
		"app.css": "text/css; charset=utf-8",
		"app.js":  "text/javascript; charset=utf-8",
	} {
		body, err := assets.ReadFile("static/" + name)
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		rec := do(t, srv, http.MethodGet, "/static/"+name, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /static/%s 状态码 = %d", name, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != contentType {
			t.Errorf("%s Content-Type = %q, 期望 %q", name, ct, contentType)
		}
		if rec.Body.Len() != len(body) {
			t.Errorf("%s 长度 = %d, 期望 %d", name, rec.Body.Len(), len(body))
		}
	}
	for _, path := range []string{"/static/app.txt", "/static/missing.js", "/static/a/b.js"} {
		if rec := do(t, srv, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 404", path, rec.Code)
		}
	}
}

// TestStyleContract 守住样式契约：设计 token 走 :root、[hidden] 有效隐藏、
// 尊重减少动效偏好，且不用 @keyframes 做动画。
func TestStyleContract(t *testing.T) {
	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("读 app.css: %v", err)
	}
	style := string(css)
	for _, want := range []string{"prefers-color-scheme: dark", "prefers-reduced-motion: reduce", ":root"} {
		if !strings.Contains(style, want) {
			t.Errorf("app.css 缺少 %q", want)
		}
	}
	if strings.Contains(style, "@keyframes") {
		t.Error("app.css 不应使用 @keyframes（动效只做颜色/位移动画）")
	}

	// 剥掉注释再断 [hidden]：否则注释里提一句就能糊弄过去。
	stripped := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(style, "")
	hidden := regexp.MustCompile(`\[hidden\][^{]*\{[^}]*display:\s*none`)
	if !hidden.MatchString(stripped) {
		t.Fatal("app.css 缺少 [hidden]{display:none} 规则")
	}
	if hidden.MatchString(regexp.MustCompile(`\[hidden\]\s*\{[^}]*\}`).ReplaceAllString(stripped, "")) {
		t.Error("[hidden] 断言自检失败：删掉规则后仍在匹配")
	}

	script, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	for _, want := range []string{"playback-empty", "aria-checked", "confirm-dialog"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("app.js 缺少 %q（空态 / 开关状态 / 二次确认）", want)
		}
	}
}

// TestWritableStateRecompute 守住「写操作控件的可用性必须随状态重算」：
// 开关与「立即同步」按钮的 disabled 原先只在渲染行时算一次，解锁令牌后要刷新页面
// 才能点，同步运行中也不禁用。现在统一由 updateControls 重算，并由令牌变化事件驱动。
func TestWritableStateRecompute(t *testing.T) {
	script, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	src := string(script)
	for _, want := range []string{
		"function updateControls()",
		"listEl.querySelectorAll('.switch')",
		"admin-token-changed",
		"document.addEventListener(TOKEN_EVENT",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js 缺少 %q：写操作可用性没有集中重算或没有令牌变化广播", want)
		}
	}
	// 反向自检：渲染行时不再各算各的 disabled，可用性只有 updateControls 一处来源。
	if strings.Contains(src, "switchBtn.disabled =") {
		t.Error("app.js 仍在渲染行时单独设置 switch 的 disabled：解锁后不会重算")
	}
}

// TestSyncConcludeGuard 守住同步收尾提示的归属与成败判据：
//
//  1. 只有本轮真的从「运行中」跑到「结束」才由收尾下结论；POST 被拒（400/401）、
//     轮询失败、超 30 分钟各自留着自己的提示，不能被 finally 覆盖。
//  2. 「跑到结束」不等于「成功」：整轮中止（Jellyfin 不可达 / 认证失败 / 列库失败）
//     只推进 last_run_at，last_ok_at 停在上一轮成功。收尾必须用 roundSucceeded()
//     比这两个时刻，失败时写「同步失败」而不是绿色完成。
//  3. 不能用 last_error 判成败：同名库冲突 / 孤儿库 / 部分分类被拒也会写它，
//     但那些轮次是成功的（manager.go 的 record 会同时推进 last_ok_at）。页面上的
//     成败标注（徽标、上次结果、进度行）与收尾提示必须共用 roundSucceeded()。
func TestSyncConcludeGuard(t *testing.T) {
	script, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	src := string(script)
	for _, want := range []string{"let sawRunning = false", "let concluded = false"} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js 缺少 %q：无法判断本轮是否真的跑完", want)
		}
	}

	// 判据：成功轮次两个时刻相等，这是 manager.go 的 record 给的不变量。
	criterion := "j.last_ok_at === j.last_run_at"
	pi := strings.Index(src, "function roundSucceeded(")
	ci := strings.Index(src, criterion)
	if pi < 0 || ci < 0 {
		t.Fatalf("app.js 缺少成败判据：roundSucceeded() 或 %q（%d/%d）", criterion, pi, ci)
	}
	if ci < pi {
		t.Error("成败判据不在 roundSucceeded() 里：收尾可能绕过它自行判断")
	}

	guard := "if ((sawRunning || expectRun === true) && !concluded)"
	verdict := "if (roundSucceeded("
	done := "say('同步完成："
	failed := "say('同步失败："
	gi := strings.Index(src, guard)
	vi := strings.Index(src, verdict)
	di := strings.Index(src, done)
	fi := strings.Index(src, failed)
	if gi < 0 || vi < 0 || di < 0 || fi < 0 {
		t.Fatalf("app.js 缺少收尾结构：guard=%d verdict=%d done=%d failed=%d", gi, vi, di, fi)
	}
	if di < gi {
		t.Error("「同步完成」提示不在 sawRunning/concluded 分支里：失败与超时会被覆盖成成功")
	}
	if di < vi || fi < vi {
		t.Error("同步结论不在 roundSucceeded() 分支里：整轮中止会被报成成功")
	}
	if strings.Contains(src, "else say('同步完成") {
		t.Error("「同步完成」仍有 else 兜底：同步根本没跑起来也会报成功")
	}
	if strings.Contains(src, "if (j.last_error) say('同步失败") {
		t.Error("app.js 仍用 last_error 判失败：冲突 / 孤儿库会被误报为同步失败")
	}

	// 判据只有一个出处：roundSucceeded 的定义 + 收尾 + 页面成败标注（徽标/上次结果/进度行）。
	if n := strings.Count(src, "roundSucceeded("); n < 3 {
		t.Errorf("roundSucceeded() 出现 %d 次，期望至少 3 次（定义、收尾、页面成败标注）：页面可能自行判成败", n)
	}
	if strings.Contains(src, "else if (j.last_error)") {
		t.Error("app.js 仍用 last_error 判成败：同名库冲突 / 部分分类被拒会被误报为同步失败")
	}
}

// TestUnknownPaths 守住页面集合：旧剧库 / 剧详情 / 分类页已移除，一律 404；
// 未登记方法与路径不落到任何页面。
func TestUnknownPaths(t *testing.T) {
	srv, st, _ := newTestServer(t)
	dramaID := seedDrama(t, st, "9201", "已下线页面用剧", []string{"x1"})
	for _, path := range []string{
		"/nope", "/merges", "/merges/x", "/status", "/status/", "/static",
		"/library", "/library/abc", "/library/0", "/library/999999", "/library/",
		"/library/" + strconv.FormatInt(dramaID, 10), "/categories",
	} {
		if rec := do(t, srv, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 404", path, rec.Code)
		}
	}
	if rec := do(t, srv, http.MethodGet, "/sync", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /sync 状态码 = %d, 期望 200", rec.Code)
	}
	for _, path := range []string{"/settings", "/sync"} {
		if rec := do(t, srv, http.MethodPost, path, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s 状态码 = %d, 期望 405", path, rec.Code)
		}
	}
}

// TestGenreDirContract 守住浏览器端 genreDir 与 internal/vfs/tree.go 的唯一算法一致。
//
// 同步页按「分类目录名」归并剧数（`/{分类}/` 这一层由 vfs.genreDir 决定），浏览器
// 拿不到那个函数，只能自己拼一遍；两边的替换字符集或兜底名一旦漂移，页面上的
// 「N 部短剧」会静默算错，而且不会有任何报错。所以这里直接读两侧源码做契约断言：
// 字符集必须相同、兜底名必须相同。
func TestGenreDirContract(t *testing.T) {
	server, err := os.ReadFile("../vfs/tree.go")
	if err != nil {
		t.Fatalf("读 tree.go: %v", err)
	}

	replacer := regexp.MustCompile(`(?s)func sanitizeName\(s string\) string \{.*?\.Replace\(s\)`).Find(server)
	if replacer == nil {
		t.Fatal("tree.go 里找不到 sanitizeName 的替换表")
	}
	wantChars := map[rune]bool{}
	// 替换表里既用双引号字面量（"/"、"?:*"）也用反引号（`\`、`"`），两种都收。
	pairs := regexp.MustCompile("(?:\"([^\"]*)\"|`([^`]*)`)\\s*,\\s*\"_\"")
	for _, m := range pairs.FindAllSubmatch(replacer, -1) {
		lit := m[1]
		if len(lit) == 0 {
			lit = m[2]
		}
		for _, r := range string(lit) {
			wantChars[r] = true
		}
	}
	if len(wantChars) == 0 {
		t.Fatal("sanitizeName 里没解析出替换字符")
	}

	fallback := regexp.MustCompile(`otherGenre\s*=\s*"([^"]*)"`).FindSubmatch(server)
	if fallback == nil {
		t.Fatal("tree.go 里找不到 otherGenre 兜底名")
	}

	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	class := regexp.MustCompile(`function genreDir\(genre\)[\s\S]*?\.replace\(/\[([^\]]*)\]\/g`).FindSubmatch(js)
	if class == nil {
		t.Fatal("app.js 的 genreDir 里找不到替换字符类")
	}
	gotChars := map[rune]bool{}
	escaped := false
	for _, r := range string(class[1]) {
		if escaped {
			gotChars[r] = true
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		gotChars[r] = true
	}

	for r := range wantChars {
		if !gotChars[r] {
			t.Errorf("app.js 的 genreDir 少转义 %q（tree.go 的 sanitizeName 会换）", r)
		}
	}
	for r := range gotChars {
		if !wantChars[r] {
			t.Errorf("app.js 的 genreDir 多转义 %q（tree.go 的 sanitizeName 不换）", r)
		}
	}
	if want := "? '" + string(fallback[1]) + "' :"; !bytes.Contains(js, []byte(want)) {
		t.Errorf("app.js 的 genreDir 兜底名不是 %q（tree.go 的 otherGenre）", string(fallback[1]))
	}
}

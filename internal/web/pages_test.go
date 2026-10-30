package web

import (
	"bytes"
	"net/http"
	"strconv"
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

// TestPages 覆盖三页界面的契约：200 + text/html + 精确 Content-Length，
// body[data-page] 与页面一致（JS 靠它分派），导航三个链接齐全且只有当前页带
// aria-current。
func TestPages(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cases := []struct{ path, page string }{
		{"/", "dramas"},
		{"/merges", "merges"},
		{"/status", "status"},
	}
	for _, c := range cases {
		t.Run(c.page, func(t *testing.T) {
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
			want := []string{"/", "/merges", "/status"}
			if len(hrefs) != len(want) {
				t.Fatalf("导航链接 = %v, 期望 %v", hrefs, want)
			}
			for i := range want {
				if hrefs[i] != want[i] {
					t.Errorf("导航第 %d 项 = %q, 期望 %q", i, hrefs[i], want[i])
				}
			}
			if current != c.path {
				t.Errorf("当前页高亮 = %q, 期望 %q", current, c.path)
			}
		})
	}
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

// TestUnknownPaths 锁定管理面之外的路径：未登记路径仍是 404（不是 501、不是 200）。
func TestUnknownPaths(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{"/nope", "/merges/x", "/status/", "/static"} {
		if rec := do(t, srv, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 404", path, rec.Code)
		}
	}
	// 页面只接受 GET（HEAD 由 GET 模式隐式覆盖）；写方法一律 405。
	if rec := do(t, srv, http.MethodPost, "/merges", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /merges 状态码 = %d, 期望 405", rec.Code)
	}
}

package webdav

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/vfs"
)

// 超时预算：所有 DAV 请求都在这两条上限内完成。此前 AC-4 是手工起服务 +
// 手工跑 rclone（`rclone mount --daemon` 永不退出），既不可重复也会卡住整个
// 测试进程；现在把「遍历整棵树」做成进程内的 mock 客户端，由这里的两条超时
// 兜住——请求级 5s、整棵遍历级 30s，超时立即让测试失败而不是挂死。
const (
	requestTimeout = 5 * time.Second
	walkTimeout    = 30 * time.Second
)

// fixture 是一次测试的服务与数据：真 SQLite 临时库 + 真 vfs.Tree + 真 handler。
type fixture struct {
	srv      *httptest.Server
	client   *http.Client
	user     string
	pass     string
	prefix   string
	titleA   string
	epIDs    []int64
	settings *store.Settings
}

// stubToken 是 TokenFunc 替身：webdav（rank 40）不能 import stream（rank 40），
// 装配根（cmd）才是接签名的地方。把 secret 编进结果证明参数确实来自注入缝。
func stubToken(secret, uid string) string { return "tok(" + secret + ":" + uid + ")" }

// tokenFor 是可预期的 token 值（与 stubToken 同规则，独立写出）。
func tokenFor(secret, uid string) string { return "tok(" + secret + ":" + uid + ")" }

// newFixture 起一个用真 handler 的 httptest.Server（不 mock 自己的 handler）。
func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	const (
		user   = "davuser"
		pass   = "davpass"
		prefix = "http://proxy.example:8080"
		secret = "0123456789abcdef0123456789abcdef"
		title  = "测试剧"
	)

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "webdav.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	for key, value := range map[store.SettingKey]string{
		store.KeyWebDAVUser:   user,
		store.KeyWebDAVPass:   pass,
		store.KeyStrmPrefix:   prefix,
		store.KeyStreamSecret: secret,
	} {
		if err := settings.Set(ctx, key, value); err != nil {
			t.Fatalf("Set %s: %v", key, err)
		}
	}

	dramaID, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "web", ProviderID: "a1", Title: title, Year: 2024, Plot: "简介",
	})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	var epIDs []int64
	for _, e := range []store.Episode{
		{EpNo: 1, Title: "开局", DurationSec: 125, Source: "web", Vid: "v1"},
		{EpNo: 2, Source: "web", Vid: "v2"}, // 空标签 → 第2集
	} {
		e.DramaID = dramaID
		id, err := st.UpsertEpisode(ctx, e)
		if err != nil {
			t.Fatalf("UpsertEpisode %d: %v", e.EpNo, err)
		}
		epIDs = append(epIDs, id)
	}

	// 与 cmd 的装配方式一致：handler 挂在 MountPath 上，路径前缀由它自己剥。
	mux := http.NewServeMux()
	mux.Handle(MountPath, NewHandler(vfs.NewTree(st, settings, stubToken), settings, newDiscardLogger()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return fixture{
		srv: srv, client: &http.Client{Timeout: requestTimeout}, user: user, pass: pass,
		prefix: prefix, titleA: title, epIDs: epIDs, settings: settings,
	}
}

// newDiscardLogger 是丢弃输出的 logger：测试不关心日志内容。
func newDiscardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// setPassword 改库里的 webdav_pass，用于验证「每请求现读、改值即时生效」。
func (f fixture) setPassword(t *testing.T, pass string) error {
	t.Helper()
	return f.settings.Set(context.Background(), store.KeyWebDAVPass, pass)
}

// seasonPath 是 fixture 里剧 A 的季目录在 DAV 上的路径。
func (f fixture) seasonPath() string { return "/" + f.titleA + " (2024)/Season 01" }

// request 发一个带 Basic 认证的 DAV 请求；path 相对挂载点（"/" 即挂载点根）。
func (f fixture) request(t *testing.T, method, path string, body io.Reader, hdr map[string]string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, method, f.srv.URL+strings.TrimSuffix(MountPath, "/")+path, body)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.SetBasicAuth(f.user, f.pass)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// multistatus 是 PROPFIND 响应体里本测试关心的部分。
type multistatus struct {
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Status string `xml:"status"`
			Prop   struct {
				ContentLength string `xml:"getcontentlength"`
				Resourcetype  struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// propfind 发 Depth: 1 的 PROPFIND 并解析响应。
func (f fixture) propfind(t *testing.T, path string) (int, multistatus) {
	t.Helper()
	resp := f.request(t, "PROPFIND", path, nil, map[string]string{"Depth": "1"})
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读 PROPFIND body: %v", err)
	}
	var ms multistatus
	if resp.StatusCode == http.StatusMultiStatus {
		if err := xml.Unmarshal(body, &ms); err != nil {
			t.Fatalf("PROPFIND 响应不是合法 multistatus XML: %v\n%s", err, body)
		}
	}
	return resp.StatusCode, ms
}

// davEntry 是 PROPFIND 响应里的一条记录：解码后的路径、长度、是否目录。
type davEntry struct {
	path  string
	size  int64
	isDir bool
	hasCL bool
}

// entries 把 multistatus 摊平成可断言的记录；缺 getcontentlength 的项 hasCL=false。
func entries(t *testing.T, ms multistatus) map[string]davEntry {
	t.Helper()
	out := map[string]davEntry{}
	for _, r := range ms.Responses {
		p, err := url.PathUnescape(r.Href)
		if err != nil {
			t.Fatalf("href 不是合法转义路径 %q: %v", r.Href, err)
		}
		if len(r.Propstat) != 1 {
			t.Fatalf("%s 的 propstat 数 = %d, 期望 1", p, len(r.Propstat))
		}
		ps := r.Propstat[0]
		if !strings.Contains(ps.Status, "200") {
			t.Fatalf("%s 的 propstat status = %q, 期望 200", p, ps.Status)
		}
		e := davEntry{path: p, isDir: ps.Prop.Resourcetype.Collection != nil}
		if ps.Prop.ContentLength != "" {
			n, err := strconv.ParseInt(strings.TrimSpace(ps.Prop.ContentLength), 10, 64)
			if err != nil {
				t.Fatalf("%s 的 getcontentlength = %q 不是整数: %v", p, ps.Prop.ContentLength, err)
			}
			e.size, e.hasCL = n, true
		}
		out[p] = e
	}
	return out
}

// AC1：PROPFIND Depth: 1 返回 207；DAV 根下直接是剧目录，剧目录下是 tvshow.nfo + Season 01。
func TestPropfindDepth1_Tree(t *testing.T) {
	f := newFixture(t)

	code, ms := f.propfind(t, "/")
	if code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND / 状态码 = %d, 期望 207", code)
	}
	got := entries(t, ms)
	// DAV 根就是库根：`rclone lsf --max-depth 3 dramahub:/` 第一级必须是剧目录
	// （docs/ACCEPTANCE.md P2），不能多套一层 `短剧/`。
	dramaHref := "/dav/" + f.titleA + " (2024)/"
	if e, ok := got[dramaHref]; !ok {
		t.Fatalf("根列举缺剧目录 %s，实际：%v", dramaHref, keys(got))
	} else if !e.isDir {
		t.Errorf("%s 应带 resourcetype collection（目录）", e.path)
	}
	// vfs 的根目录名（`短剧`）不得成为 DAV 路径的一段：DAV 根即库根。
	if _, ok := got["/dav/短剧/"]; ok {
		t.Errorf("根下不该再套一层 短剧：%v", keys(got))
	}

	code, ms = f.propfind(t, "/"+f.titleA+" (2024)")
	if code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND 剧目录 状态码 = %d, 期望 207", code)
	}
	got = entries(t, ms)
	seasonHref := dramaHref + "Season 01/"
	if e, ok := got[seasonHref]; !ok {
		t.Fatalf("剧目录列举缺 %s，实际：%v", seasonHref, keys(got))
	} else if !e.isDir {
		t.Errorf("%s 应是目录", seasonHref)
	}
}

// AC1 + AC2：季目录下的每个文件都带 getcontentlength，且与 GET 实际 body 长度一致。
func TestPropfindDepth1_ContentLengthMatchesBody(t *testing.T) {
	f := newFixture(t)

	code, ms := f.propfind(t, f.seasonPath())
	if code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND %s 状态码 = %d, 期望 207", f.seasonPath(), code)
	}
	got := entries(t, ms)

	// 2 集 × (strm + nfo) = 4 个文件。
	files := 0
	for path, e := range got {
		if e.isDir {
			continue
		}
		files++
		if !e.hasCL {
			t.Errorf("%s 的 propstat 缺 getcontentlength", path)
			continue
		}
		if e.size <= 0 {
			t.Errorf("%s 的 getcontentlength = %d, 期望 > 0", path, e.size)
		}
		body := f.getBody(t, strings.TrimPrefix(path, "/dav"), true)
		if int64(len(body)) != e.size {
			t.Errorf("%s: getcontentlength = %d, GET body 长度 = %d", path, e.size, len(body))
		}
	}
	if files != 4 {
		t.Errorf("季目录下的文件数 = %d, 期望 4（2 集 × strm/nfo），实际：%v", files, keys(got))
	}
}

// AC2：GET strm/nfo 返回内容，Content-Length 正确且与 body 长度一致。
func TestGet_Files(t *testing.T) {
	f := newFixture(t)
	season := f.seasonPath()

	for _, tc := range []struct {
		name      string
		ep        int64
		ext       string
		wantRoot  string
		wantBytes func(f fixture) []byte
	}{
		{
			name: "strm/首集", ep: f.epIDs[0], ext: ".strm",
			wantBytes: func(f fixture) []byte {
				uid := strconv.FormatInt(f.epIDs[0], 10)
				return []byte(f.prefix + "/s/" + tokenFor("0123456789abcdef0123456789abcdef", uid) + "/e/" + uid + "\n")
			},
		},
		{
			name: "nfo/首集", ep: f.epIDs[0], ext: ".nfo", wantRoot: "episodedetails",
		},
		{
			name: "strm/第二集", ep: f.epIDs[1], ext: ".strm",
			wantBytes: func(f fixture) []byte {
				uid := strconv.FormatInt(f.epIDs[1], 10)
				return []byte(f.prefix + "/s/" + tokenFor("0123456789abcdef0123456789abcdef", uid) + "/e/" + uid + "\n")
			},
		},
		{
			name: "nfo/第二集", ep: f.epIDs[1], ext: ".nfo", wantRoot: "episodedetails",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := season + "/" + f.episodeBase(tc.ep) + tc.ext
			resp := f.request(t, http.MethodGet, path, nil, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s 状态码 = %d, 期望 200", path, resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("读 body: %v", err)
			}
			if len(body) == 0 {
				t.Fatalf("GET %s 产出 0 字节", path)
			}
			if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
				t.Errorf("GET %s: Content-Length = %q, body 长度 = %d", path, cl, len(body))
			}
			if tc.wantBytes != nil {
				if want := tc.wantBytes(f); !bytes.Equal(body, want) {
					t.Errorf("GET %s body = %q, 期望 %q", path, body, want)
				}
			}
			if tc.wantRoot != "" {
				var probe struct {
					XMLName xml.Name
				}
				if err := xml.Unmarshal(body, &probe); err != nil {
					t.Fatalf("GET %s 的 body 不是合法 XML: %v", path, err)
				}
				if probe.XMLName.Local != tc.wantRoot {
					t.Errorf("GET %s 根元素 = %q, 期望 %q", path, probe.XMLName.Local, tc.wantRoot)
				}
			}
		})
	}
}

// getBody 取一条 DAV 路径的 body（带认证），并断言 200 + Content-Length 一致。
func (f fixture) getBody(t *testing.T, path string, checkLength bool) []byte {
	t.Helper()
	resp := f.request(t, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d, 期望 200", path, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读 %s body: %v", path, err)
	}
	if checkLength {
		if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
			t.Errorf("GET %s: Content-Length = %q, body 长度 = %d", path, cl, len(body))
		}
	}
	return body
}

// episodeBase 独立拼出集文件名主干（不复用 vfs 内部函数，避免自证）。
func (f fixture) episodeBase(epID int64) string {
	no := 1
	if epID == f.epIDs[1] {
		no = 2
	}
	label := "开局"
	if no == 2 {
		label = "第2集"
	}
	return f.titleA + " - S01E0" + strconv.Itoa(no) + " - " + label
}

// AC3：无认证 / 错误凭据 → 401（含 WWW-Authenticate，body 是统一 JSON 错误体）。
func TestAuth(t *testing.T) {
	f := newFixture(t)
	path := f.seasonPath() + "/" + f.episodeBase(f.epIDs[0]) + ".strm"

	// 无认证。
	resp, err := f.client.Get(f.srv.URL + strings.TrimSuffix(MountPath, "/") + path)
	if err != nil {
		t.Fatalf("GET（无认证）: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无认证 GET 状态码 = %d, 期望 401", resp.StatusCode)
	}
	if aw := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(aw, "Basic ") {
		t.Errorf("WWW-Authenticate = %q, 期望 Basic 方案", aw)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读 401 body: %v", err)
	}
	var eb struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("401 body 不是统一 JSON 错误体: %v（%s）", err, body)
	}
	if eb.Code != "unauthorized" {
		t.Errorf("401 body code = %q, 期望 unauthorized", eb.Code)
	}

	// 错误凭据（用户名对、密码错；用户名错、密码对）。
	for _, tc := range []struct{ user, pass string }{{f.user, "wrong"}, {"wrong", f.pass}} {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+strings.TrimSuffix(MountPath, "/")+path, nil)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		req.SetBasicAuth(tc.user, tc.pass)
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatalf("GET（错误凭据）: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("凭据 %q/%q → 状态码 %d, 期望 401", tc.user, tc.pass, resp.StatusCode)
		}
	}

	// 正确凭据放行。
	if resp := f.request(t, http.MethodGet, path, nil, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("正确凭据 GET 状态码 = %d, 期望 200", resp.StatusCode)
	}
}

// AC3 补充：凭据每请求现读，改值即时生效（硬约束 5）。
func TestAuth_ReadsSettingsLive(t *testing.T) {
	f := newFixture(t)

	// 换掉密码：旧凭据立即失效，新凭据立即生效——无需重建 handler。
	if err := f.setPassword(t, "newpass"); err != nil {
		t.Fatalf("Set webdav_pass: %v", err)
	}
	if resp := f.request(t, "PROPFIND", "/", nil, map[string]string{"Depth": "1"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("旧密码 PROPFIND 状态码 = %d, 期望 401", resp.StatusCode)
	}
	f.pass = "newpass"
	if code, _ := f.propfind(t, "/"); code != http.StatusMultiStatus {
		t.Errorf("新密码 PROPFIND 状态码 = %d, 期望 207", code)
	}
}

// AC4（只读契约）：写方法一律失败，挂载点不会因协议层写请求改动数据面。
func TestReadOnly(t *testing.T) {
	f := newFixture(t)
	path := f.seasonPath() + "/" + f.episodeBase(f.epIDs[0]) + ".strm"

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPut, path},
		{http.MethodDelete, path},
		{"MKCOL", f.seasonPath() + "/新目录"},
	}
	for _, c := range cases {
		resp := f.request(t, c.method, c.path, strings.NewReader("x"), nil)
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusNoContent {
			t.Errorf("%s %s 状态码 = %d, 只读挂载点不应成功", c.method, c.path, resp.StatusCode)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatalf("读 %s %s body: %v", c.method, c.path, err)
		}
	}

	// 写请求之后读仍然正常（没有状态污染）。
	if resp := f.request(t, http.MethodGet, path, nil, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("写请求之后 GET 状态码 = %d, 期望 200", resp.StatusCode)
	}
}

// AC：未知路径 → 404（不是 200/207，也不泄漏别的资源）。
func TestNotFound(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{
		"/不存在 (2024)/tvshow.nfo",
		f.seasonPath() + "/不存在.strm",
		f.seasonPath() + "/" + f.episodeBase(f.epIDs[0]) + ".txt",
	} {
		if resp := f.request(t, http.MethodGet, path, nil, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 404", path, resp.StatusCode)
		}
	}
}

// keys 取 map 的键集合（用于失败信息）。
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// walkNode 是一次递归遍历收集到的节点：DAV 路径（相对挂载点）+ 类型 + 长度。
type walkNode struct {
	path  string
	isDir bool
	size  int64
}

// walkTree 模拟 WebDAV 客户端的递归列举（rclone `lsf --max-depth N` / Emby 扫库
// 的请求序列）：对每个目录发一次 `PROPFIND Depth: 1`，把子项并入结果，遇到目录
// 再下钻，直到达到 maxDepth。
//
// 这是对「手工装 rclone、手工 `rclone mount --daemon`、手工看输出」那一步的替代：
// 同一条协议路径、同一个 handler，但完全进程内、可重复、受 walkTimeout 约束。
// 客户端一侧是真实 HTTP 请求（不是 mock handler），只是不再依赖外部 rclone 二进制。
func (f fixture) walkTree(t *testing.T, maxDepth int) []walkNode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), walkTimeout)
	t.Cleanup(cancel)

	var out []walkNode
	queue := []struct {
		path  string
		depth int
	}{{path: "/", depth: 0}}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			t.Fatalf("遍历超过 %v 仍未结束（挂死）：%v", walkTimeout, err)
		}
		cur := queue[0]
		queue = queue[1:]

		// 只把「子项」入结果：PROPFIND Depth:1 的响应首项是目录自身。
		code, ms := f.propfindCtx(t, ctx, cur.path)
		if code != http.StatusMultiStatus {
			t.Fatalf("PROPFIND %s 状态码 = %d, 期望 207", cur.path, code)
		}
		// href 里的目录带尾斜杠、文件不带；统一成「无尾斜杠」再比较，self 项
		// （目录自身，href == 本次请求路径）不入结果。
		for href, e := range entries(t, ms) {
			rel := strings.TrimSuffix(strings.TrimPrefix(href, "/dav"), "/")
			if strings.TrimSuffix(cur.path, "/") == rel && cur.path != "/" {
				continue
			}
			if rel == "" {
				continue // 根自身
			}
			out = append(out, walkNode{path: rel, isDir: e.isDir, size: e.size})
			if e.isDir && cur.depth+1 < maxDepth {
				queue = append(queue, struct {
					path  string
					depth int
				}{path: rel, depth: cur.depth + 1})
			}
		}
	}
	return out
}

// propfindCtx 是带 ctx 的 Depth:1 PROPFIND（walkTree 用；普通用例走 f.propfind）。
func (f fixture) propfindCtx(t *testing.T, ctx context.Context, path string) (int, multistatus) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", f.srv.URL+strings.TrimSuffix(MountPath, "/")+path, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.SetBasicAuth(f.user, f.pass)
	req.Header.Set("Depth", "1")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("PROPFIND %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读 PROPFIND body: %v", err)
	}
	var ms multistatus
	if resp.StatusCode == http.StatusMultiStatus {
		if err := xml.Unmarshal(body, &ms); err != nil {
			t.Fatalf("PROPFIND 响应不是合法 multistatus XML: %v\n%s", err, body)
		}
	}
	return resp.StatusCode, ms
}

// AC-4：整棵目录树可被递归列举到（max-depth 3 覆盖 剧/Season/文件 三层），
// 且每个文件项都带 getcontentlength = GET 实际 body 长度。
//
// 这条替代了原先「手工跑 rclone lsf --max-depth 3」的验收方式：同一协议语义，
// 进程内、无外部二进制、受 requestTimeout/walkTimeout 约束。
func TestWalkTree_CompleteDepth3(t *testing.T) {
	f := newFixture(t)
	nodes := f.walkTree(t, 3)

	got := make(map[string]walkNode, len(nodes))
	for _, n := range nodes {
		if _, dup := got[n.path]; dup {
			t.Errorf("路径 %s 被列举两次", n.path)
		}
		got[n.path] = n
	}

	want := map[string]bool{ // path -> isDir
		"/" + f.titleA + " (2024)":                                 true,
		"/" + f.titleA + " (2024)/tvshow.nfo":                      false,
		"/" + f.titleA + " (2024)/Season 01":                       true,
		f.seasonPath() + "/" + f.episodeBase(f.epIDs[0]) + ".strm": false,
		f.seasonPath() + "/" + f.episodeBase(f.epIDs[0]) + ".nfo":  false,
		f.seasonPath() + "/" + f.episodeBase(f.epIDs[1]) + ".strm": false,
		f.seasonPath() + "/" + f.episodeBase(f.epIDs[1]) + ".nfo":  false,
	}
	for path, isDir := range want {
		n, ok := got[path]
		if !ok {
			t.Errorf("遍历结果缺 %s；实际：%v", path, keys(got))
			continue
		}
		if n.isDir != isDir {
			t.Errorf("%s isDir = %v, 期望 %v", path, n.isDir, isDir)
		}
	}
	if len(got) != len(want) {
		t.Errorf("遍历到 %d 项，期望 %d 项\n实际：%v", len(got), len(want), keys(got))
	}

	// 文件项的长度 == 真实 GET body 长度（AC-1/AC-2 在遍历视角下的同一条断言）。
	for path, n := range got {
		if n.isDir {
			continue
		}
		body := f.getBody(t, path, true)
		if int64(len(body)) != n.size {
			t.Errorf("%s: getcontentlength = %d, GET body 长度 = %d", path, n.size, len(body))
		}
	}
}

// AC-4 的反面：遍历必须有限且在预算内结束——maxDepth 3 时不漏层、也不无限下钻。
// 这条守住「测试自己不会挂死」，避免再出现手工命令超时不退出的情况。
func TestWalkTree_DepthBounded(t *testing.T) {
	f := newFixture(t)
	start := time.Now()
	nodes := f.walkTree(t, 3)
	if elapsed := time.Since(start); elapsed > walkTimeout {
		t.Fatalf("遍历耗时 %v 超过预算 %v", elapsed, walkTimeout)
	}
	for _, n := range nodes {
		if depth := strings.Count(n.path, "/"); depth > 3 {
			t.Errorf("max-depth 3 却出现深度 %d 的路径 %s", depth, n.path)
		}
	}
}

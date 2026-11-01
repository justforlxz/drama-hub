package webdav

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/pgtest"
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

// fixture 是一次测试的服务与数据：真数据库（独立 schema）+ 真 vfs.Tree + 真 handler。
type fixture struct {
	srv      *httptest.Server
	client   *http.Client
	user     string
	pass     string
	prefix   string
	titleA   string
	genre    string // 剧 A 的分类目录名
	shard    string // 剧 A 的分片目录名（两位十六进制）
	epIDs    []int64
	settings *store.Settings
}

// stubID 是 IDFunc 替身：webdav（rank 40）不能 import stream（rank 40），
// 装配根（cmd）才是接签名的地方。把 secret 编进结果证明参数确实来自注入缝。
func stubID(secret, uid string) string { return "id(" + secret + ":" + uid + ")" }

// idFor 是可预期的 id 值（与 stubID 同规则，独立写出）。
func idFor(secret, uid string) string { return "id(" + secret + ":" + uid + ")" }

// shardOf 独立实现目录分片号 fnv32a(provider_id) % 128（不复用生产代码）：
// 测试用它把「这个剧目录应该在哪个分片下」算出来，而不是从被测树里抄。
func shardOf(providerID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(providerID))
	return fmt.Sprintf("%02x", h.Sum32()%128)
}

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

	db, err := store.Open(ctx, pgtest.DSN(t))
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
		Genre: "爱情",
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
	// vfs 树只认 emby_item 派生表（硬约束 6）：等价于生产里「同步完成」的重建。
	if _, err := st.RebuildEmbyItems(ctx, dramaID); err != nil {
		t.Fatalf("RebuildEmbyItems: %v", err)
	}

	// 与 cmd 的装配方式一致：handler 挂在 MountPath 上，路径前缀由它自己剥。
	mux := http.NewServeMux()
	mux.Handle(MountPath, NewHandler(vfs.NewTree(st, settings, stubID, newDiscardLogger()), settings, newDiscardLogger()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return fixture{
		srv: srv, client: &http.Client{Timeout: requestTimeout}, user: user, pass: pass,
		prefix: prefix, titleA: title, genre: "爱情", shard: shardOf("a1"),
		epIDs: epIDs, settings: settings,
	}
}

// newDiscardLogger 是丢弃输出的 logger：测试不关心日志内容。
func newDiscardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// setPassword 改库里的 webdav_pass，用于验证「每请求现读、改值即时生效」。
func (f fixture) setPassword(t *testing.T, pass string) error {
	t.Helper()
	return f.settings.Set(context.Background(), store.KeyWebDAVPass, pass)
}

// dramaPath 是 fixture 里剧 A 的剧目录在 DAV 上的路径：/<分类>/<分片>/<剧目录>。
func (f fixture) dramaPath() string {
	return "/" + f.genre + "/" + f.shard + "/" + f.titleA + " (2024)"
}

// seasonPath 是 fixture 里剧 A 的季目录在 DAV 上的路径。
func (f fixture) seasonPath() string { return f.dramaPath() + "/Season 01" }

// shardPath 是剧 A 所在分片目录在 DAV 上的路径。
func (f fixture) shardPath() string { return "/" + f.genre + "/" + f.shard }

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

// shardHrefs 返回某个分类目录下全部分片目录的 href（00–7f）。
func shardHrefs(genre string) []string {
	out := make([]string, 0, 128)
	for shard := 0; shard < 128; shard++ {
		out = append(out, "/webdav/"+genre+"/"+fmt.Sprintf("%02x", shard)+"/")
	}
	return out
}

// AC：<LAYER> Depth: 1 返回 207，逐层只有该层的节点——根只有分类、分类只有分片、
// 分片只有剧目录、剧目录是 tvshow.nfo + Season 01（没有 全部/、没有额外的 短剧/ 一层）。
func TestPropfindDepth1_Layers(t *testing.T) {
	f := newFixture(t)
	const davPrefix = "/webdav"

	tests := []struct {
		name     string
		path     string
		wantDirs []string // 期望的目录 href（带尾斜杠）
		wantFile []string // 期望的文件 href（不带尾斜杠）
	}{
		{
			name:     "根只有分类",
			path:     "/",
			wantDirs: []string{davPrefix + "/" + f.genre + "/"},
		},
		{
			name:     "分类只有分片",
			path:     "/" + f.genre,
			wantDirs: shardHrefs(f.genre), // 00–7f 的全部（docs/DIRECTORY-CONTRACT.md 允许）
		},
		{
			name:     "分片只有剧目录",
			path:     f.shardPath(),
			wantDirs: []string{davPrefix + f.dramaPath() + "/"},
		},
		{
			name:     "剧目录是 tvshow.nfo + Season 01",
			path:     f.dramaPath(),
			wantDirs: []string{davPrefix + f.dramaPath() + "/Season 01/"},
			wantFile: []string{davPrefix + f.dramaPath() + "/tvshow.nfo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, ms := f.propfind(t, tt.path)
			if code != http.StatusMultiStatus {
				t.Fatalf("Depth:1 %s 状态码 = %d, 期望 207", tt.path, code)
			}
			got := entries(t, ms)
			// 把这一层的 href 打出来：这是 PROPFIND 的真实输出（真 handler + httptest），
			// 逐层「只有该层节点」的证据就在这段日志里（-test.v 可见）。
			hrefs := make([]string, 0, len(got))
			for href := range got {
				hrefs = append(hrefs, href)
			}
			sort.Strings(hrefs)
			t.Logf("PROPFIND Depth:1 %-28s → %v", tt.path, hrefs)

			// Depth:1 的响应首项是目录自身（目录 href 带尾斜杠）。
			delete(got, davPrefix+strings.TrimSuffix(tt.path, "/")+"/")

			for _, href := range tt.wantDirs {
				e, ok := got[href]
				if !ok {
					t.Fatalf("%s 缺目录 %s，实际：%v", tt.path, href, keys(got))
				}
				if !e.isDir {
					t.Errorf("%s 应带 resourcetype collection（目录）", href)
				}
				delete(got, href)
			}
			for _, href := range tt.wantFile {
				e, ok := got[href]
				if !ok {
					t.Fatalf("%s 缺文件 %s，实际：%v", tt.path, href, keys(got))
				}
				if e.isDir {
					t.Errorf("%s 不该是目录", href)
				}
				delete(got, href)
			}
			if len(got) != 0 {
				t.Errorf("%s 多出节点：%v", tt.path, keys(got))
			}
			// vfs 的根目录名（`短剧`）不得成为 DAV 路径的一段：DAV 根即库根。
			for href := range entries(t, ms) {
				if strings.Contains(href, "/webdav/短剧") {
					t.Errorf("DAV 路径里出现了 vfs 根目录名 短剧：%s", href)
				}
				if strings.Contains(href, "全部") {
					t.Errorf("树里出现 全部/ 节点：%s", href)
				}
			}
		})
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
		body := f.getBody(t, strings.TrimPrefix(path, "/webdav"), true)
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
				uid := "e" + strconv.FormatInt(f.epIDs[0], 10)
				return []byte(f.prefix + "/video/" + idFor("0123456789abcdef0123456789abcdef", uid) + ".m3u8\n")
			},
		},
		{
			name: "nfo/首集", ep: f.epIDs[0], ext: ".nfo", wantRoot: "episodedetails",
		},
		{
			name: "strm/第二集", ep: f.epIDs[1], ext: ".strm",
			wantBytes: func(f fixture) []byte {
				uid := "e" + strconv.FormatInt(f.epIDs[1], 10)
				return []byte(f.prefix + "/video/" + idFor("0123456789abcdef0123456789abcdef", uid) + ".m3u8\n")
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

// AC（阻断 3 的回归）：不存在的目录，PROPFIND 必须回 404，不是 405。
//
// x/net/webdav 的 handlePropfind 用 os.IsNotExist 分流，而它只认 *fs.PathError；
// 早先的 fmt.Errorf 包装让它落到 405 Method Not Allowed（客户端看起来像「协议不支持」）。
func TestPropfindNotFoundIs404(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{
		"/无此分类",
		"/" + f.genre + "/fe", // 分片号越界（N=128，最大 7f）
		"/" + f.genre + "/" + f.shard + "/无此剧 (2024)",
		"/" + f.genre + "/" + f.shard + "/" + f.titleA + " (2024)/Season 02",
	} {
		resp := f.request(t, "PROPFIND", path, nil, map[string]string{"Depth": "1"})
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("PROPFIND %s 状态码 = %d, 期望 404", path, resp.StatusCode)
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
			rel := strings.TrimSuffix(strings.TrimPrefix(href, "/webdav"), "/")
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

// AC：整棵目录树可被递归列举到（max-depth 5 覆盖 分类/分片/剧/Season/文件 五层），
// 且每个文件项都带 getcontentlength = GET 实际 body 长度。
//
// 这条替代了原先「手工跑 rclone lsf --max-depth 3」的验收方式：同一协议语义，
// 进程内、无外部二进制、受 requestTimeout/walkTimeout 约束。
func TestWalkTree_Complete(t *testing.T) {
	f := newFixture(t)
	nodes := f.walkTree(t, 5)

	got := make(map[string]walkNode, len(nodes))
	for _, n := range nodes {
		if _, dup := got[n.path]; dup {
			t.Errorf("路径 %s 被列举两次", n.path)
		}
		got[n.path] = n
	}

	want := map[string]bool{ // path -> isDir
		"/" + f.genre:                 true,
		f.dramaPath():                 true,
		f.dramaPath() + "/tvshow.nfo": false,
		f.dramaPath() + "/Season 01":  true,
		f.seasonPath() + "/" + f.episodeBase(f.epIDs[0]) + ".strm": false,
		f.seasonPath() + "/" + f.episodeBase(f.epIDs[0]) + ".nfo":  false,
		f.seasonPath() + "/" + f.episodeBase(f.epIDs[1]) + ".strm": false,
		f.seasonPath() + "/" + f.episodeBase(f.epIDs[1]) + ".nfo":  false,
	}
	// 分片目录恒为 00–7f 的全部（空分片列出来是空目录），一并计入期望集合。
	for shard := 0; shard < 128; shard++ {
		want["/"+f.genre+"/"+fmt.Sprintf("%02x", shard)] = true
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

// AC 的反面：遍历必须有限且在预算内结束——maxDepth 5 时不漏层、也不无限下钻。
// 这条守住「测试自己不会挂死」，避免再出现手工命令超时不退出的情况。
func TestWalkTree_DepthBounded(t *testing.T) {
	f := newFixture(t)
	start := time.Now()
	nodes := f.walkTree(t, 5)
	if elapsed := time.Since(start); elapsed > walkTimeout {
		t.Fatalf("遍历耗时 %v 超过预算 %v", elapsed, walkTimeout)
	}
	for _, n := range nodes {
		if depth := strings.Count(n.path, "/"); depth > 5 {
			t.Errorf("max-depth 5 却出现深度 %d 的路径 %s", depth, n.path)
		}
	}
}

// countingTree 是 vfs.Tree 的计数装饰器：记录 List / Render 调用次数与查过的目录。
//
// 「一次 Depth:1 的查询次数与目录规模无关」是一条可断言的事实（不是时间），
// 所以性能契约用它钉住，而不是钉墙钟。
type countingTree struct {
	inner   *vfs.Tree
	lists   int
	renders int
	dirs    map[string]int
}

func newCountingTree(inner *vfs.Tree) *countingTree {
	return &countingTree{inner: inner, dirs: map[string]int{}}
}

func (c *countingTree) List(ctx context.Context, dir string) ([]vfs.Node, error) {
	c.lists++
	c.dirs[dir]++
	return c.inner.List(ctx, dir)
}

func (c *countingTree) Render(ctx context.Context, p string) ([]byte, error) {
	c.renders++
	return c.inner.Render(ctx, p)
}

// reset 清空计数（一次请求前调用）。
func (c *countingTree) reset() {
	c.lists, c.renders, c.dirs = 0, 0, map[string]int{}
}

// bulkFixture 是「n 部剧、同一分类」的服务端：真数据库 + 计数树 + 真 handler。
type bulkFixture struct {
	fixture
	tree *countingTree
}

// newBulkFixture 造 n 部剧（同一分类，分片由 fnv 自动摊开）并起 httptest.Server。
func newBulkFixture(t *testing.T, n int) bulkFixture {
	t.Helper()
	ctx := context.Background()
	const (
		user   = "davuser"
		pass   = "davpass"
		genre  = "剧情"
		secret = "0123456789abcdef0123456789abcdef"
	)

	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	for k, v := range map[store.SettingKey]string{
		store.KeyWebDAVUser:   user,
		store.KeyWebDAVPass:   pass,
		store.KeyStreamSecret: secret,
	} {
		if err := settings.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	// 一个事务里灌库：这条测试要的是「列举规模」，不是写路径性能。
	err = st.InTx(ctx, func(tx *store.Store) error {
		for i := 0; i < n; i++ {
			if _, err := tx.UpsertDrama(ctx, store.Drama{
				Provider: "web", ProviderID: "bulk-" + strconv.Itoa(i),
				Title: "剧" + strconv.Itoa(i), Year: 2024, Genre: genre,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("灌入 %d 部剧: %v", n, err)
	}

	tree := newCountingTree(vfs.NewTree(st, settings, stubID, newDiscardLogger()))
	mux := http.NewServeMux()
	mux.Handle(MountPath, NewHandler(tree, settings, newDiscardLogger()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return bulkFixture{
		fixture: fixture{
			srv: srv, client: &http.Client{Timeout: requestTimeout},
			user: user, pass: pass, genre: genre,
		},
		tree: tree,
	}
}

// AC（阻断 1、2 的回归）：Depth:1 的查询次数与目录规模无关，且不渲染孙级文件。
//
// 旧实现：OpenFile 目录即列子项，x/net 对每个子项都 OpenFile 一次 ⇒ 每层列举变成
// 「子项数」次查询 + 把孙级文件全渲染一遍（608k 库实测 /剧情 13.30s）。
// 现在：每层只查一次，一次列举的查询次数是常数。
func TestPropfindDepth1_QueryCountIndependentOfSize(t *testing.T) {
	type sample struct{ name, path string }
	measure := func(t *testing.T, f bulkFixture, s sample) (lists, renders int) {
		t.Helper()
		f.tree.reset()
		code, _ := f.propfind(t, s.path)
		if code != http.StatusMultiStatus {
			t.Fatalf("%s: Depth:1 %s 状态码 = %d, 期望 207", s.name, s.path, code)
		}
		return f.tree.lists, f.tree.renders
	}

	const small, large = 200, 12000
	fs, fl := newBulkFixture(t, small), newBulkFixture(t, large)

	// 找一个「有剧」的分片目录（两部 fixture 的 provider_id 前缀相同，分布一致）。
	shard := shardOf("bulk-0")

	for _, s := range []sample{
		{name: "根", path: "/"},
		{name: "分类", path: "/剧情"},
		{name: "分片", path: "/剧情/" + shard},
	} {
		t.Run(s.name, func(t *testing.T) {
			smallLists, smallRenders := measure(t, fs, s)
			largeLists, largeRenders := measure(t, fl, s)

			t.Logf("%s：%d 部剧 → List=%d Render=%d；%d 部剧 → List=%d Render=%d（查过的目录 %v）",
				s.name, small, smallLists, smallRenders, large, largeLists, largeRenders, fl.tree.dirs)

			if smallLists != largeLists {
				t.Errorf("%s 的查询次数随库规模变化：%d 部 = %d 次，%d 部 = %d 次",
					s.name, small, smallLists, large, largeLists)
			}
			// 常数上限：根 1 次 DISTINCT genre；分类/分片 = 父目录 + 自己。
			if largeLists > 2 {
				t.Errorf("%s 的 List 次数 = %d, 期望 ≤ 2（每层一次）", s.name, largeLists)
			}
			// 列举某一层只碰这一层：孙级文件一个都不渲染。
			if largeRenders != 0 {
				t.Errorf("%s 列举时渲染了 %d 个孙级文件, 期望 0", s.name, largeRenders)
			}
			// 只查「这一层 + 它的父目录」，一个孙级目录都不碰。
			if len(fl.tree.dirs) > 2 {
				t.Errorf("%s 查了 %d 个目录（%v），期望 ≤ 2 个", s.name, len(fl.tree.dirs), fl.tree.dirs)
			}
		})
	}
}

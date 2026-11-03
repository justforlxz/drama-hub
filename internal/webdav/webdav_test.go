package webdav

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/cache"
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
	epIDs    []int64
	settings *store.Settings

	// covers 是接到树上的海报缓存（与生产装配同一个实例）；cdn 是它的假封面源，
	// coverPath 是本地已落盘的封面文件路径。三者只在 newFixtureWithCover 里填。
	covers    *cache.CoverCache
	cdn       *fakeCoverCDN
	coverPath string

	// st 是这套 fixture 的 store（与树、封面缓存同一个库）。测试用它把封面行改老，
	// 再跑一轮真的淘汰，验证「系统淘汰后按需回源」整条链路。
	st *store.Store
}

// stubID 是 IDFunc 替身：webdav（rank 40）不能 import stream（rank 40），
// 装配根（cmd）才是接签名的地方。把 secret 编进结果证明参数确实来自注入缝。
func stubID(secret, uid string) string { return "id(" + secret + ":" + uid + ")" }

// idFor 是可预期的 id 值（与 stubID 同规则，独立写出）。
func idFor(secret, uid string) string { return "id(" + secret + ":" + uid + ")" }

// fakeCoverCDN 是假封面源：记请求次数，用来断言「命中不回源 / 按需回源一次」。
type fakeCoverCDN struct {
	mu     sync.Mutex
	hits   int
	status int
	body   []byte
}

// set 切换下一次响应的状态码与字节。
func (c *fakeCoverCDN) set(status int, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status, c.body = status, body
}

// count 返回累计请求次数。
func (c *fakeCoverCDN) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

// newFakeCoverCDN 起一个假封面源，默认回 200 + 一小段 JPEG 字节。
func newFakeCoverCDN(t *testing.T) (*fakeCoverCDN, string) {
	t.Helper()
	cdn := &fakeCoverCDN{status: http.StatusOK, body: coverBytes}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cdn.mu.Lock()
		cdn.hits++
		status, body := cdn.status, cdn.body
		cdn.mu.Unlock()
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "image/jpeg")
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return cdn, srv.URL + "/cover.jpg"
}

// coverBytes 是假封面源的字节（JPEG 魔数 + 可断言的尾巴；测试不解析图像）。
var coverBytes = []byte("\xff\xd8\xff\xe0 dav poster bytes")

// coverFixture 描述「剧 A 的封面源」：cdn 记请求次数，url 写进 drama.cover_url。
type coverFixture struct {
	cdn *fakeCoverCDN
	url string
}

// newFixture 起一个用真 handler 的 httptest.Server（不 mock 自己的 handler）。
// 默认没有封面：poster.jpg 不出现在树里。
func newFixture(t *testing.T) fixture {
	t.Helper()
	return newFixtureWith(t, nil)
}

// newFixtureWithCover 建一个「剧 A 的封面已经本地化成功」的 fixture：按生产路径把封面
// 落进临时 cache_dir 并写 artifact 行，再把同一个 *cache.CoverCache 接到 vfs 树上。
func newFixtureWithCover(t *testing.T) fixture {
	t.Helper()
	cdn, url := newFakeCoverCDN(t)
	return newFixtureWith(t, &coverFixture{cdn: cdn, url: url})
}

// newFixtureWith 起一个用真 handler 的 httptest.Server（不 mock 自己的 handler）。
// cover 非 nil 时把剧 A 的 cover_url 指向它，并在装配树之前做一次封面本地化
// （等价于「同步已经跑过一轮」）。
func newFixtureWith(t *testing.T, cover *coverFixture) fixture {
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
		// 夹具是「每集一条对外剧集」：本文件验的是 WebDAV 协议，不是分块。
		store.KeyMergeChunkSize: "1",
		// 海报缓存落临时目录：poster.jpg 的命中 / 按需回源都在这里发生（MKACG-650）。
		store.KeyCacheDir: filepath.Join(t.TempDir(), "cache"),
	} {
		if err := settings.Set(ctx, key, value); err != nil {
			t.Fatalf("Set %s: %v", key, err)
		}
	}

	coverURL := ""
	if cover != nil {
		coverURL = cover.url
	}
	dramaID, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "web", ProviderID: "a1", Title: title, Year: 2024, Plot: "简介",
		Genre: "爱情", CoverURL: coverURL,
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
	// vfs 的对外形态按当前分块规则现算（硬约束 6，docs/adr/0014）。

	// 海报缓存与 cmd 的装配方式一致：同步（真实部署里由 scheduler 触发）与 poster.jpg
	// 的按需回源共用一份实例。
	covers := cache.NewCoverCache(st, settings, &http.Client{Timeout: requestTimeout}, newDiscardLogger())
	coverPath := ""
	if cover != nil {
		if err := covers.LocalizeCover(ctx, "web", "a1", cover.url); err != nil {
			t.Fatalf("LocalizeCover: %v", err)
		}
		row, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, "web/a1")
		if err != nil {
			t.Fatalf("封面本地化后 ArtifactByKey: %v", err)
		}
		coverPath = row.Path
	}

	// 与 cmd 的装配方式一致：handler 挂在 MountPath 上，路径前缀由它自己剥。
	mux := http.NewServeMux()
	mux.Handle(MountPath, NewHandler(vfs.NewTree(st, settings, stubID, covers, newDiscardLogger()), settings, newDiscardLogger()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f := fixture{
		srv: srv, client: &http.Client{Timeout: requestTimeout}, user: user, pass: pass,
		prefix: prefix, titleA: title, genre: "爱情",
		epIDs: epIDs, settings: settings, covers: covers, coverPath: coverPath, st: st,
	}
	if cover != nil {
		f.cdn = cover.cdn
	}
	return f
}

// newDiscardLogger 是丢弃输出的 logger：测试不关心日志内容。
func newDiscardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// setPassword 改库里的 webdav_pass，用于验证「每请求现读、改值即时生效」。
func (f fixture) setPassword(t *testing.T, pass string) error {
	t.Helper()
	return f.settings.Set(context.Background(), store.KeyWebDAVPass, pass)
}

// dramaPath 是 fixture 里剧 A 的剧目录在 DAV 上的路径：/<分类>/<剧目录>。
func (f fixture) dramaPath() string {
	return "/" + f.genre + "/" + f.titleA + " (2024)"
}

// seasonPath 是 fixture 里剧 A 的季目录在 DAV 上的路径。
func (f fixture) seasonPath() string { return f.dramaPath() + "/Season 01" }

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

// AC-1：<LAYER> Depth: 1 返回 207，逐层只有该层的节点——根只有分类、分类只有剧目录、
// 剧目录是 tvshow.nfo + Season 01（没有 全部/、没有额外的 短剧/ 一层、没有分片层）。
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
			name:     "分类只有剧目录",
			path:     "/" + f.genre,
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

// `Depth` 的真实语义（MKACG-651 功能检测纠正的口径）：0 = 只有资源自己、1 = 直接子项、
// 省略头或 infinity = 递归整棵子树、数值 ≥2 = 协议库直接 400。
//
// 这条测试的存在原因很具体：交付文档曾经断言「本服务只支持 Depth 0/1，不存在一次请求
// 拉多层」，而 `x/net/webdav` 缺头时默认 infiniteDepth，实测 21.6 万剧的分类目录在
// 省略 Depth 时是 369 MB / 116 s。文档因此写错，代码行为没变——所以用测试把真实语义
// 钉住，避免下一次再照着文档猜。
func TestPropfindDepthSemantics(t *testing.T) {
	f := newFixture(t)
	// 多状态响应里的 href 是 DAV 绝对路径（带 /webdav 挂载点前缀）。
	const dav = "/webdav"
	season := dav + f.seasonPath()
	strm := season + "/" + f.episodeBase(f.epIDs[0]) + ".strm"

	hrefs := func(t *testing.T, resp *http.Response) map[string]bool {
		t.Helper()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("读 PROPFIND body: %v", err)
		}
		var ms multistatus
		if err := xml.Unmarshal(body, &ms); err != nil {
			t.Fatalf("响应不是合法 multistatus XML: %v\n%s", err, body)
		}
		out := make(map[string]bool, len(ms.Responses))
		for _, r := range ms.Responses {
			p, err := url.PathUnescape(r.Href)
			if err != nil {
				t.Fatalf("href 不是合法转义路径 %q: %v", r.Href, err)
			}
			out[p] = true
		}
		return out
	}

	t.Run("Depth: 0 只有资源自己", func(t *testing.T) {
		resp := f.request(t, "PROPFIND", f.dramaPath(), nil, map[string]string{"Depth": "0"})
		if resp.StatusCode != http.StatusMultiStatus {
			t.Fatalf("状态码 = %d, 期望 207", resp.StatusCode)
		}
		got := hrefs(t, resp)
		if len(got) != 1 || !got[dav+f.dramaPath()+"/"] {
			t.Errorf("Depth:0 的响应 = %v, 期望只有剧目录自己 %s", got, dav+f.dramaPath()+"/")
		}
	})

	t.Run("Depth: 1 只有直接子项", func(t *testing.T) {
		resp := f.request(t, "PROPFIND", f.dramaPath(), nil, map[string]string{"Depth": "1"})
		if resp.StatusCode != http.StatusMultiStatus {
			t.Fatalf("状态码 = %d, 期望 207", resp.StatusCode)
		}
		got := hrefs(t, resp)
		if got[strm] {
			t.Errorf("Depth:1 递归到了孙级文件 %s：%v", strm, got)
		}
	})

	t.Run("省略 Depth 等价于 infinity，递归整棵子树", func(t *testing.T) {
		resp := f.request(t, "PROPFIND", "/"+f.genre, nil, nil)
		if resp.StatusCode != http.StatusMultiStatus {
			t.Fatalf("状态码 = %d, 期望 207", resp.StatusCode)
		}
		got := hrefs(t, resp)
		if !got[strm] {
			t.Errorf("省略 Depth 时没递归到孙级文件 %s（文档口径依赖这条事实）：%v", strm, got)
		}
	})

	t.Run("显式 infinity 同样递归", func(t *testing.T) {
		resp := f.request(t, "PROPFIND", "/"+f.genre, nil, map[string]string{"Depth": "infinity"})
		if resp.StatusCode != http.StatusMultiStatus {
			t.Fatalf("状态码 = %d, 期望 207", resp.StatusCode)
		}
		if got := hrefs(t, resp); !got[strm] {
			t.Errorf("Depth:infinity 没递归到孙级文件 %s：%v", strm, got)
		}
	})

	t.Run("Depth: 2 被协议库拒绝", func(t *testing.T) {
		resp := f.request(t, "PROPFIND", "/"+f.genre, nil, map[string]string{"Depth": "2"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("Depth:2 状态码 = %d, 期望 400", resp.StatusCode)
		}
	})
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
		"/" + f.genre + "/无此剧 (2024)",
		f.dramaPath() + "/Season 02",
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

// AC：整棵目录树可被递归列举到（max-depth 4 覆盖 分类/剧/Season/文件 四层），
// 且每个文件项都带 getcontentlength = GET 实际 body 长度。
//
// 这条替代了原先「手工跑 rclone lsf --max-depth 3」的验收方式：同一协议语义，
// 进程内、无外部二进制、受 requestTimeout/walkTimeout 约束。
func TestWalkTree_Complete(t *testing.T) {
	f := newFixture(t)
	nodes := f.walkTree(t, 4)

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

// AC 的反面：遍历必须有限且在预算内结束——maxDepth 4 时不漏层、也不无限下钻。
// 这条守住「测试自己不会挂死」，避免再出现手工命令超时不退出的情况。
func TestWalkTree_DepthBounded(t *testing.T) {
	f := newFixture(t)
	start := time.Now()
	nodes := f.walkTree(t, 4)
	if elapsed := time.Since(start); elapsed > walkTimeout {
		t.Fatalf("遍历耗时 %v 超过预算 %v", elapsed, walkTimeout)
	}
	for _, n := range nodes {
		if depth := strings.Count(n.path, "/"); depth > 4 {
			t.Errorf("max-depth 4 却出现深度 %d 的路径 %s", depth, n.path)
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

// Revision 转发给真实树：缓存键必须跟着真实结构版本走。
func (c *countingTree) Revision() uint64 { return c.inner.Revision() }

// reset 清空计数（一次请求前调用）。
func (c *countingTree) reset() {
	c.lists, c.renders, c.dirs = 0, 0, map[string]int{}
}

// bulkFixture 是「n 部剧、同一分类」的服务端：真数据库 + 计数树 + 真 handler。
type bulkFixture struct {
	fixture
	tree *countingTree
}

// newBulkFixture 造 n 部剧（同一分类，全部直接挂在分类目录下）并起 httptest.Server。
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

	tree := newCountingTree(vfs.NewTree(st, settings, stubID, nil, newDiscardLogger()))
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

	for _, s := range []sample{
		{name: "根", path: "/"},
		{name: "分类", path: "/剧情"},
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
			// 常数上限：根 1 次 DISTINCT genre；分类 = 父目录 + 自己。
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

// MKACG-650 AC：有封面的剧，剧目录里能列到 poster.jpg，PROPFIND 的 getcontentlength
// 与 GET 的实际字节数一致；内容就是本地封面，列举与读取都不回源。
func TestPropfindDramaDirIncludesPoster(t *testing.T) {
	f := newFixtureWithCover(t)
	posterHref := "/webdav" + f.dramaPath() + "/poster.jpg"

	code, ms := f.propfind(t, f.dramaPath())
	if code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND Depth:1 %s 状态码 = %d, 期望 207", f.dramaPath(), code)
	}
	entry, ok := entries(t, ms)[posterHref]
	if !ok {
		t.Fatalf("有封面的剧目录里没有 poster.jpg：%v", entries(t, ms))
	}
	if entry.isDir || !entry.hasCL {
		t.Fatalf("poster.jpg 的列举项 = %+v, 期望「文件 + 带 getcontentlength」", entry)
	}

	body := f.getBody(t, f.dramaPath()+"/poster.jpg", true)
	if !bytes.Equal(body, coverBytes) {
		t.Fatalf("poster.jpg 字节 = %q, 期望本地封面 %q", body, coverBytes)
	}
	if entry.size != int64(len(body)) {
		t.Fatalf("getcontentlength = %d, GET 字节数 = %d", entry.size, len(body))
	}
	if got := f.cdn.count(); got != 1 {
		t.Fatalf("列举 + 读取封面回源 %d 次, 期望 1（只有建 fixture 时那次本地化）", got)
	}

	// 同一个剧目录的 tvshow.nfo：thumb 指同目录的 poster.jpg（相对路径）。
	nfo := f.getBody(t, f.dramaPath()+"/tvshow.nfo", true)
	if want := []byte(`<thumb aspect="poster">poster.jpg</thumb>`); !bytes.Contains(nfo, want) {
		t.Fatalf("tvshow.nfo 缺 %s：\n%s", want, nfo)
	}
}

// MKACG-650 AC（按需重新下载）：封面文件被淘汰（手删）后请求 poster.jpg → 200 +
// 有效内容，且恰好回源一次；下一次请求不再回源（索引行已指回真实文件）。
func TestPosterRefetchesAfterCoverFileRemoved(t *testing.T) {
	f := newFixtureWithCover(t)
	if err := os.Remove(f.coverPath); err != nil {
		t.Fatalf("删本地封面 %s: %v", f.coverPath, err)
	}
	before := f.cdn.count()

	body := f.getBody(t, f.dramaPath()+"/poster.jpg", true)
	if !bytes.Equal(body, coverBytes) {
		t.Fatalf("按需回源后 poster.jpg 字节 = %q, 期望 %q", body, coverBytes)
	}
	after := f.cdn.count()
	if after != before+1 {
		t.Fatalf("回源次数 %d → %d, 期望 +1", before, after)
	}

	if body := f.getBody(t, f.dramaPath()+"/poster.jpg", true); !bytes.Equal(body, coverBytes) {
		t.Fatalf("第二次读取的字节 = %q, 期望 %q", body, coverBytes)
	}
	if got := f.cdn.count(); got != after {
		t.Fatalf("第二次请求仍回源：%d → %d", after, got)
	}
}

// MKACG-650 AC-6（系统淘汰子项，真 DAV 请求）：走生产淘汰路径（cache_lru_days 过期）
// 把封面淘汰之后，poster.jpg 必须**仍然出现在目录里**、GET 200 且恰好回源一次。
//
// 这条对应用户报告的现场：旧实现在淘汰时连 artifact 行一起删，HasCover 转假，
// poster.jpg 直接从剧目录消失、GET 404，要等该剧被下一轮同步重新本地化才回来。
func TestPosterSurvivesSystemEviction(t *testing.T) {
	f := newFixtureWithCover(t)
	ctx := context.Background()

	// 走真的淘汰：把封面行的 last_used_at 改到 40 天前，再跑一轮 Cache.Evict
	// （与装配根起的 5 分钟循环是同一份实现）。cache_lru_days 默认 30。
	if err := f.st.TouchArtifact(ctx, store.ArtifactKindCover, "web/a1", "2020-01-01T00:00:00Z"); err != nil {
		t.Fatalf("把封面行改老: %v", err)
	}
	evictor := cache.New(f.st, f.settings, newDiscardLogger())
	if err := evictor.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, err := os.Stat(f.coverPath); !os.IsNotExist(err) {
		t.Fatalf("淘汰后封面文件仍在: %v", err)
	}

	posterHref := "/webdav" + f.dramaPath() + "/poster.jpg"
	code, ms := f.propfind(t, f.dramaPath())
	if code != http.StatusMultiStatus {
		t.Fatalf("淘汰后剧目录 PROPFIND = %d, 期望 207", code)
	}
	entry, ok := entries(t, ms)[posterHref]
	if !ok {
		t.Fatalf("淘汰后 poster.jpg 从剧目录消失（MKACG-650 的失败项）：%v", entries(t, ms))
	}
	if entry.isDir || !entry.hasCL {
		t.Fatalf("poster.jpg 列举项 = %+v, 期望「文件 + 带 getcontentlength」", entry)
	}

	// PROPFIND 要给 getcontentlength 就必须渲染一次（硬约束 3），所以那次渲染已经是
	// 「第一次按需回源」——它把字节写回了本地。这里断言的是「整条列举+GET 只回源一次」：
	// 淘汰后的第一次渲染回源、之后完全命中本地。
	before := f.cdn.count()
	body := f.getBody(t, f.dramaPath()+"/poster.jpg", true)
	if !bytes.Equal(body, coverBytes) {
		t.Fatalf("淘汰后 poster.jpg 字节 = %q, 期望 %q", body, coverBytes)
	}
	if got := f.cdn.count(); got != before {
		t.Fatalf("PROPFIND 已回源并写回本地，GET 却再回源：%d → %d", before, got)
	}
	if entry.size != int64(len(body)) {
		t.Fatalf("getcontentlength = %d, GET 字节数 = %d", entry.size, len(body))
	}

	// 回源成功后写回了真实行：再请求一次完全命中本地。
	recount := f.cdn.count()
	if body := f.getBody(t, f.dramaPath()+"/poster.jpg", true); !bytes.Equal(body, coverBytes) {
		t.Fatalf("第二次读取的字节 = %q, 期望 %q", body, coverBytes)
	}
	if got := f.cdn.count(); got != recount {
		t.Fatalf("回源成功后仍再回源：%d → %d", recount, got)
	}
}

// MKACG-650 AC（single-flight）：并发 3 个请求同一个已淘汰的封面 → 回源计数 = 1。
func TestPosterConcurrentRequestsFetchOnce(t *testing.T) {
	f := newFixtureWithCover(t)
	if err := os.Remove(f.coverPath); err != nil {
		t.Fatalf("删本地封面 %s: %v", f.coverPath, err)
	}
	before := f.cdn.count()
	target := f.srv.URL + strings.TrimSuffix(MountPath, "/") + f.dramaPath() + "/poster.jpg"

	const n = 3
	start := make(chan struct{})
	bodies := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
			if err != nil {
				errs[i] = err
				return
			}
			req.SetBasicAuth(f.user, f.pass)
			resp, err := f.client.Do(req)
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				errs[i] = fmt.Errorf("状态码 %d", resp.StatusCode)
				return
			}
			bodies[i], errs[i] = io.ReadAll(resp.Body)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个请求: %v", i, errs[i])
		}
		if !bytes.Equal(bodies[i], coverBytes) {
			t.Fatalf("第 %d 个请求的字节 = %q, 期望 %q", i, bodies[i], coverBytes)
		}
	}
	if got := f.cdn.count(); got != before+1 {
		t.Fatalf("并发 %d 个请求回源 %d 次, 期望 1（single-flight）", n, got-before)
	}
}

// MKACG-650 AC（无封面）：没有 poster.jpg、nfo 没有 thumb，目录列举与 GET 都不报错。
func TestPosterAbsentForDramaWithoutCover(t *testing.T) {
	f := newFixture(t) // 默认没有封面
	posterHref := "/webdav" + f.dramaPath() + "/poster.jpg"

	resp := f.request(t, http.MethodGet, f.dramaPath()+"/poster.jpg", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET 无封面剧的 poster.jpg 状态码 = %d, 期望 404", resp.StatusCode)
	}

	code, ms := f.propfind(t, f.dramaPath())
	if code != http.StatusMultiStatus {
		t.Fatalf("无封面剧的 PROPFIND 状态码 = %d, 期望 207（不因为封面缺失报错）", code)
	}
	if _, ok := entries(t, ms)[posterHref]; ok {
		t.Fatalf("没有封面的剧却列出了 poster.jpg：%v", entries(t, ms))
	}
	nfo := f.getBody(t, f.dramaPath()+"/tvshow.nfo", true)
	if bytes.Contains(nfo, []byte("<thumb")) {
		t.Fatalf("没有封面的剧 nfo 里出现了 thumb：\n%s", nfo)
	}
}

// MKACG-650 AC（回源失败）：封面取不回来 → poster.jpg 404（不是 0 字节、不是半截），
// 该剧目录的列举仍然成功——失败的封面只是被跳过。
func TestPosterFetchFailureIsNotFound(t *testing.T) {
	f := newFixtureWithCover(t)
	if err := os.Remove(f.coverPath); err != nil {
		t.Fatalf("删本地封面 %s: %v", f.coverPath, err)
	}
	f.cdn.set(http.StatusNotFound, []byte("nope"))

	resp := f.request(t, http.MethodGet, f.dramaPath()+"/poster.jpg", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("回源失败时 GET poster.jpg 状态码 = %d, 期望 404", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读 404 body: %v", err)
	}
	if bytes.Contains(body, coverBytes) || bytes.Contains(body, []byte("\xff\xd8")) {
		t.Fatalf("回源失败却回了图片字节：%q", body)
	}

	code, ms := f.propfind(t, f.dramaPath())
	if code != http.StatusMultiStatus {
		t.Fatalf("回源失败时 PROPFIND 状态码 = %d, 期望 207（跳过取不回来的封面）", code)
	}
	if _, ok := entries(t, ms)["/webdav"+f.dramaPath()+"/poster.jpg"]; ok {
		t.Fatal("回源失败的 poster.jpg 仍出现在列举里")
	}
}

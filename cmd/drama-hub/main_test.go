package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/plugin/hongguo"
	"github.com/justforlxz/drama-hub/internal/scheduler"
	"github.com/justforlxz/drama-hub/internal/segment"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
	"github.com/justforlxz/drama-hub/internal/sync"
	"github.com/justforlxz/drama-hub/internal/vfs"
	"github.com/justforlxz/drama-hub/internal/web"
)

// shardOf 独立实现目录分片号 fnv32a(provider_id) % 128（不复用生产代码）：
// 用它算「这部剧的目录应该挂在哪个分片下」。
func shardOf(providerID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(providerID))
	return fmt.Sprintf("%02x", h.Sum32()%128)
}

// newTestHandler 与 main 用同一条装配路径：httptest 起真实 handler，不 mock 自己的
// handler。播放面需要数据库（settings / episode 表），因此开一个临时库装配。
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	db, err := store.Open(context.Background(), pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return newAssembly(slog.New(slog.DiscardHandler), store.NewStore(db.DB), store.NewSettings(db.DB),
		testSyncer(), fakeSchedule{}, testSources(), nil)
}

// newAssembly 按 main 的装配顺序造 handler：缓存实例 + 直链解析器 + 播放面出站身份
// + 路由表。reg 为 nil 时用一个空注册表（任何 Resolve 都会失败，播放面据此回 502）。
//
// 出站身份走与 main 同一个派生函数（playbackHeaders），这里给一份无人使用的 boots：
// 装配测试不真取直链，但注入链路必须与生产一致，否则这条缝只在真机上才被走过。
func newAssembly(logger *slog.Logger, st *store.Store, settings *store.Settings, syncer web.SeriesSyncer, schedule web.Schedule, sources []web.Source, reg *plugin.Registry) http.Handler {
	if reg == nil {
		reg = plugin.NewRegistry()
	}
	return newHandler(logger, st, settings, syncer, schedule, sources,
		cache.New(st, settings, logger), stream.NewResolver(st, reg),
		playbackHeaders(map[string]pluginBoot{}), testAdminToken)
}

// testAdminToken 是装配测试用的管理令牌：生产里来自 DRAMA_HUB_ADMIN_TOKEN。
// 写操作（POST / DELETE / PATCH）不带它一律 401，所以生产路径上的写请求都要带。
const testAdminToken = "test-admin-token"

// stubSource 是装配测试用的最小插件：Resolve 恒给同一个直链。
//
// 它让「播放面装配」不必打真上游：playlist 200 只需要解析出一个合法 URL
// （分片索引在测试里直接写库，不必真 ffprobe）。
type stubSource struct {
	name string
	url  string
}

func (s stubSource) Name() string { return s.name }

func (s stubSource) Enumerate(context.Context, *plugin.Cursor) (plugin.Page, error) {
	return plugin.Page{}, nil
}

func (s stubSource) Work(context.Context, plugin.WorkRef) (plugin.WorkDetail, error) {
	return plugin.WorkDetail{}, nil
}

func (s stubSource) Resolve(context.Context, plugin.WorkRef, int) (plugin.Stream, error) {
	return plugin.Stream{URL: s.url}, nil
}

func (s stubSource) Health(context.Context) error { return nil }

// stubRegistry 造一个只含 stubSource 的注册表（源名与库里的 provider 对齐）。
func stubRegistry(t *testing.T, name, directURL string) *plugin.Registry {
	t.Helper()
	reg := plugin.NewRegistry()
	if err := reg.Register(stubSource{name: name, url: directURL}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// testSyncer 是管理 API 的数据源依赖。cmd 的测试不触发同步，只为满足装配参数；
// BaseURL 指向本机 1 端口，即使误触发同步也只会立刻连接失败，不会打到真站。
func testSyncer() web.SeriesSyncer {
	logger := slog.New(slog.DiscardHandler)
	return sync.New(hongguo.New(hongguo.Config{BaseURL: "http://127.0.0.1:1", Logger: logger}), nil, logger)
}

// fakeSchedule 是管理面的调度依赖替身：cmd 的测试不跑调度器循环，只验证接线。
type fakeSchedule struct{}

func (fakeSchedule) Trigger(context.Context, string) error { return nil }

func (fakeSchedule) Status(context.Context) ([]scheduler.SourceStatus, error) { return nil, nil }

// testSources 是状态页的播放源清单，形状与生产一致（名字来自插件）。
func testSources() []web.Source {
	return []web.Source{{Name: hongguo.Name, Label: "红果官网 SSR"}}
}

func serve(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// TestHandler_Healthz 覆盖 /healthz 的响应契约（AC1）。
func TestHandler_Healthz(t *testing.T) {
	rec := serve(t, newTestHandler(t), http.MethodGet, "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, 期望 %q", body.Status, "ok")
	}
}

// TestHandler_RetiredPlaybackPaths 覆盖「/dav/ 与 /s/{token}/* 已退役、不留兼容层」：
// 两条老路径都落到管理面的 404 兜底，不再是 501、也不再有任何 handler。
func TestHandler_RetiredPlaybackPaths(t *testing.T) {
	h := newTestHandler(t)
	for _, path := range []string{
		"/s/x", "/s/abc/e/e1", "/s/abc/seg/e1/0.ts", "/s/abc/e/m1/master.m3u8",
		"/dav/", "/dav/短剧",
	} {
		if rec := serve(t, h, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 404（退役路径没有 handler）", path, rec.Code)
		}
	}
}

// TestHandler_ManagementSurface 覆盖 R1 的装配面：管理面的三页与 `/api/` 都挂在
// 装配根的 `/` 上（不是 404、不是 501），未登记路径仍由 web 的 mux 兜底 404。
func TestHandler_ManagementSurface(t *testing.T) {
	h := newTestHandler(t)
	for _, path := range []string{"/", "/merges", "/status"} {
		rec := serve(t, h, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s 状态码 = %d, 期望 200（管理面没挂上）", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("GET %s Content-Type = %q", path, ct)
		}
	}
	// 管理 API 走真实端点：空库的剧目列表是 200 + 空数组（501 说明还是占位）。
	rec := serve(t, h, http.MethodGet, "/api/dramas")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/dramas 状态码 = %d, 期望 200（501 说明管理 API 还是占位）", rec.Code)
	}
	var list struct {
		Dramas []any `json:"dramas"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Errorf("GET /api/dramas 不是合法 JSON: %v", err)
	}
	// 未登记路径仍是 404。
	for _, path := range []string{"/nope", "/api/nope"} {
		if rec := serve(t, h, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 404", path, rec.Code)
		}
	}
}

// TestHandler_LogsRequests（F5 AC7）证明装配根每请求恰一条访问日志：对
// /healthz、/webdav/（无认证 → 401）、/nope 各发一次，每次 1 条含 method/path/status/duration_ms
// 四字段的 JSON 日志，路径与状态和响应一致。logger 级别设为 Debug，避免级别
// 过滤影响条数。
func TestHandler_LogsRequests(t *testing.T) {
	cases := []struct {
		path     string
		wantPath string
		wantCode int
	}{
		{"/healthz", "/healthz", http.StatusOK},
		{"/webdav/", "/webdav/", http.StatusUnauthorized},
		{"/nope", "/nope", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			buf := &bytes.Buffer{}
			db, err := store.Open(context.Background(), pgtest.DSN(t))
			if err != nil {
				t.Fatalf("store.Open: %v", err)
			}
			defer func() { _ = db.Close() }()
			h := newAssembly(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
				store.NewStore(db.DB), store.NewSettings(db.DB), testSyncer(), fakeSchedule{}, testSources(), nil)

			rec := serve(t, h, http.MethodGet, c.path)
			if rec.Code != c.wantCode {
				t.Errorf("GET %s 状态码 = %d, 期望 %d", c.path, rec.Code, c.wantCode)
			}

			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("GET %s 日志条数 = %d, 期望 1（buf=%q）", c.path, len(lines), buf.String())
			}
			var entry struct {
				Method     string `json:"method"`
				Path       string `json:"path"`
				Status     int    `json:"status"`
				DurationMS *int64 `json:"duration_ms"`
			}
			if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
				t.Fatalf("日志不是合法 JSON: %v（line=%q）", err, lines[0])
			}
			if entry.Method != http.MethodGet {
				t.Errorf("method = %q, 期望 GET", entry.Method)
			}
			if entry.Path != c.wantPath {
				t.Errorf("path = %q, 期望 %q", entry.Path, c.wantPath)
			}
			if entry.Status != c.wantCode {
				t.Errorf("status = %d, 期望 %d", entry.Status, c.wantCode)
			}
			if entry.DurationMS == nil || *entry.DurationMS < 0 {
				t.Errorf("duration_ms = %v, 期望 ≥ 0", entry.DurationMS)
			}
		})
	}
}

// TestHandler_WebDAV 覆盖元数据面：`/webdav/` 是只读虚拟 WebDAV（无认证 401、
// 带认证 Depth:1 207）；`.strm` 的内容是 `/video/{id}.m3u8`，id 由装配根的 signID
// 生成、与 stream.ID 一致 —— 这条断言能挡住 IDFunc 接错。
func TestHandler_WebDAV(t *testing.T) {
	ctx := context.Background()
	const (
		user   = "davuser"
		pass   = "davpass"
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
	dramaID, err := st.UpsertDrama(ctx, store.Drama{Provider: "web", ProviderID: "p1", Title: "测试剧", Year: 2024, Genre: "爱情"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	epID, err := st.UpsertEpisode(ctx, store.Episode{DramaID: dramaID, EpNo: 1, Title: "开局", Source: "web", Vid: "v1"})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}
	// vfs 树只认 emby_item 派生表（硬约束 6），等价于生产里同步完成后的重建。
	if _, err := st.RebuildEmbyItems(ctx, dramaID); err != nil {
		t.Fatalf("RebuildEmbyItems: %v", err)
	}
	h := newAssembly(slog.New(slog.DiscardHandler), st, settings, testSyncer(), fakeSchedule{}, testSources(), nil)

	// 无认证 → 401（不是 501：说明 /webdav/ 已接上真实 handler）。
	if rec := serve(t, h, http.MethodGet, "/webdav/"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /webdav/（无认证）状态码 = %d, 期望 401", rec.Code)
	}

	// 带认证 → 207。
	req := httptest.NewRequest("PROPFIND", "/webdav/", nil)
	req.SetBasicAuth(user, pass)
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /webdav/ 状态码 = %d, 期望 207", rec.Code)
	}

	// 文件名含空格与中文，请求目标必须是转义后的形式（真实客户端也这样发）。
	path := "/webdav/" + url.PathEscape("爱情") + "/" + shardOf("p1") + "/" +
		url.PathEscape("测试剧 (2024)") + "/Season%2001/" +
		url.PathEscape("测试剧 - S01E01 - 开局") + ".strm"
	req = httptest.NewRequest(http.MethodGet, path, nil)
	req.SetBasicAuth(user, pass)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d, 期望 200", path, rec.Code)
	}
	uid := "e" + strconv.FormatInt(epID, 10)
	body := rec.Body.String()
	// B1：strm_prefix 为空时按请求 Host 推导绝对地址（httptest 的 Host = example.com），
	// 默认配置下 .strm 就是可直接播放的绝对 URL。
	if want := "http://example.com/video/" + stream.ID(secret, uid) + ".m3u8\n"; body != want {
		t.Errorf("strm 内容 = %q, 期望 %q", body, want)
	}
	// 契约的 .strm 形态：`^.*/video/[em][0-9]+-[0-9a-f]{12}.m3u8$`（单行 + 尾换行）。
	strmRE := regexp.MustCompile(`^.*/video/[em][0-9]+-[0-9a-f]{12}\.m3u8\n$`)
	if !strmRE.MatchString(body) {
		t.Errorf("strm 内容不符合契约正则 %s：%q", strmRE, body)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %q, body 长度 = %d", cl, rec.Body.Len())
	}
}

// TestHandler_SettingsWrite 覆盖「新部署靠接口写 WebDAV 凭据」这条唯一入口：
// 写操作要管理令牌（没配/带错都 401），写进去的账号密码让 `/webdav/` 下一次请求
// 立刻用新凭据（不重启），非法值与未知键 400 且库里不动。
func TestHandler_SettingsWrite(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	h := newAssembly(slog.New(slog.DiscardHandler), st, settings, testSyncer(), fakeSchedule{}, testSources(), nil)

	patch := func(token, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set(web.AdminTokenHeader, token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	propfind := func(user, pass string) int {
		t.Helper()
		req := httptest.NewRequest("PROPFIND", "/webdav/", nil)
		req.SetBasicAuth(user, pass)
		req.Header.Set("Depth", "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// 没带令牌 / 带错令牌 → 401，并且什么都没写。
	if rec := patch("", `{"settings":{"webdav_user":"alice"}}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌 PATCH 状态码 = %d, 期望 401（body=%s）", rec.Code, rec.Body.String())
	}
	if rec := patch("wrong-token", `{"settings":{"webdav_user":"alice"}}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错令牌 PATCH 状态码 = %d, 期望 401", rec.Code)
	}
	if _, found, err := settings.OptionalInt(ctx, store.KeyRefreshBatch); err != nil || found {
		t.Fatalf("401 之后库被写了：refresh_batch found=%v err=%v", found, err)
	}

	// 造一部剧 + 一集，供 `.strm` 断言用（vfs 只认 emby_item 派生表）。
	dramaID, err := st.UpsertDrama(ctx, store.Drama{Provider: "web", ProviderID: "p1", Title: "测试剧", Year: 2024, Genre: "爱情"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	epID, err := st.UpsertEpisode(ctx, store.Episode{DramaID: dramaID, EpNo: 1, Title: "开局", Source: "web", Vid: "v1"})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}
	if _, err := st.RebuildEmbyItems(ctx, dramaID); err != nil {
		t.Fatalf("RebuildEmbyItems: %v", err)
	}
	const secret = "0123456789abcdef0123456789abcdef"

	// 带对令牌 → 200，凭据立刻生效：新凭据 207，旧凭据 401（不需要重启）。
	rec := patch(testAdminToken, `{"settings":{"webdav_user":"alice","webdav_pass":"s3cret",`+
		`"strm_prefix":"http://media.example:9000/","stream_secret":"`+secret+`"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("带令牌 PATCH 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	// 归一化后的值回在响应里：尾 `/` 被去掉，密码不回显。
	var body struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got := body.Settings["strm_prefix"]; got != "http://media.example:9000" {
		t.Errorf("strm_prefix 归一化结果 = %v, 期望去尾 /", got)
	}
	if _, ok := body.Settings["webdav_pass"]; !ok || body.Settings["webdav_pass"] != nil {
		t.Errorf("webdav_pass 应回 null（不回显），实际 = %v（存在=%v）", body.Settings["webdav_pass"], ok)
	}
	if code := propfind("alice", "s3cret"); code != http.StatusMultiStatus {
		t.Errorf("新凭据 PROPFIND 状态码 = %d, 期望 207", code)
	}
	if code := propfind("bob", "s3cret"); code != http.StatusUnauthorized {
		t.Errorf("旧凭据 PROPFIND 状态码 = %d, 期望 401", code)
	}

	// B2 的归一化要落到渲染：写进去的是带尾 `/` 的地址，`.strm` 里不能出现双斜杠。
	strmPath := "/webdav/" + url.PathEscape("爱情") + "/" + shardOf("p1") + "/" +
		url.PathEscape("测试剧 (2024)") + "/Season%2001/" +
		url.PathEscape("测试剧 - S01E01 - 开局") + ".strm"
	strmReq := httptest.NewRequest(http.MethodGet, strmPath, nil)
	strmReq.SetBasicAuth("alice", "s3cret")
	strmRec := httptest.NewRecorder()
	h.ServeHTTP(strmRec, strmReq)
	if strmRec.Code != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d, 期望 200", strmPath, strmRec.Code)
	}
	uid := "e" + strconv.FormatInt(epID, 10)
	if want := "http://media.example:9000/video/" + stream.ID(secret, uid) + ".m3u8\n"; strmRec.Body.String() != want {
		t.Errorf(".strm 内容 = %q, 期望 %q（无双斜杠）", strmRec.Body.String(), want)
	}

	// 未知键 / 越界值 → 400，且整批不落库。
	if rec := patch(testAdminToken, `{"settings":{"webdav_user":"bob","nope":"1"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("未知键 PATCH 状态码 = %d, 期望 400（body=%s）", rec.Code, rec.Body.String())
	}
	if got, err := settings.String(ctx, store.KeyWebDAVUser); err != nil || got != "alice" {
		t.Fatalf("未知键的批次写了一半：webdav_user = %q err=%v", got, err)
	}
	for _, bad := range []string{"-1", "1048577", "abc"} {
		rec := patch(testAdminToken, `{"settings":{"cache_max_mb":"`+bad+`"}}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("cache_max_mb=%s 状态码 = %d, 期望 400（body=%s）", bad, rec.Code, rec.Body.String())
		}
	}
	// 0 在注册表边界内（0 = 不按大小淘汰），因此是 200 —— 见 docs/DEV-STANDARDS.md 的配置表。
	if rec := patch(testAdminToken, `{"settings":{"cache_max_mb":"0"}}`); rec.Code != http.StatusOK {
		t.Errorf("cache_max_mb=0 状态码 = %d, 期望 200（注册表下界就是 0）", rec.Code)
	}

	// 读操作不需要令牌：状态页与 API 照旧可读，并报出「有没有配令牌」。
	statusRec := serve(t, h, http.MethodGet, "/api/status")
	if statusRec.Code != http.StatusOK {
		t.Fatalf("GET /api/status 状态码 = %d, 期望 200（读操作不受令牌影响）", statusRec.Code)
	}
	var status struct {
		AdminTokenSet bool `json:"admin_token_set"`
		Config        struct {
			CacheMaxMB int    `json:"cache_max_mb"`
			StrmPrefix string `json:"strm_prefix"`
		} `json:"config"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("解析 /api/status: %v", err)
	}
	if !status.AdminTokenSet {
		t.Error("admin_token_set = false, 期望 true（装配根注入了令牌）")
	}
	if status.Config.CacheMaxMB != 0 || status.Config.StrmPrefix != "http://media.example:9000" {
		t.Errorf("config = %+v, 期望 0 与归一化后的 strm_prefix", status.Config)
	}
}

// TestHandler_PlaybackSurface 覆盖 2.0 播放面的装配：两个口子挂在 `/video`，
// 单集与合并集同一路径形态；签名错 401、uid 不存在 404；每请求恰一条访问日志。
func TestHandler_PlaybackSurface(t *testing.T) {
	ctx := context.Background()
	const secret = "0123456789abcdef0123456789abcdef"
	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	for k, v := range map[store.SettingKey]string{
		store.KeyStreamSecret: secret,
		store.KeyCacheDir:     filepath.Join(t.TempDir(), "cache"),
	} {
		if err := settings.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	dramaID, err := st.UpsertDrama(ctx, store.Drama{Provider: "web", ProviderID: "p1", Title: "测试剧", Year: 2026})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	var ids []int64
	for i := 1; i <= 3; i++ {
		id, err := st.UpsertEpisode(ctx, store.Episode{DramaID: dramaID, EpNo: i, Source: "web", Vid: "v"})
		if err != nil {
			t.Fatalf("UpsertEpisode: %v", err)
		}
		ids = append(ids, id)
	}
	// 索引由 S8 按需建立；装配面测试不跑 ffprobe，直接写每集各 2 片。
	// codec_sig 带通道段（`web/`）：没有通道段的签名会被当成老索引重建（MKACG-629）。
	for _, id := range ids {
		if err := st.ReplaceSegments(ctx, id, []store.Segment{
			{EpisodeID: id, StartSec: 0, Dur: 4, Keyframe: true, Mode: "copy", CodecSig: "web/h264/High/40/yuv420p/[0][0][0][0]/1/12800"},
			{EpisodeID: id, StartSec: 4, Dur: 2, Keyframe: true, Mode: "copy", CodecSig: "web/h264/High/40/yuv420p/[0][0][0][0]/1/12800"},
		}); err != nil {
			t.Fatalf("ReplaceSegments: %v", err)
		}
	}
	// 播放单元只认派生表；CreateMergeGroup 会重建它，这里先把单集形态建出来。
	if _, err := st.RebuildEmbyItems(ctx, dramaID); err != nil {
		t.Fatalf("RebuildEmbyItems: %v", err)
	}
	gid, err := st.CreateMergeGroup(ctx, dramaID, "第2-3集", []int64{ids[1], ids[2]})
	if err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := newAssembly(logger, st, settings, testSyncer(), fakeSchedule{}, testSources(),
		stubRegistry(t, "web", "http://127.0.0.1:1/direct.mp4"))

	singleUID := "e" + strconv.FormatInt(ids[0], 10)
	mergedUID := "m" + strconv.FormatInt(gid, 10)

	// 单集：合法 VOD playlist，分片 URI 指回 /video/{id}/{seq}.ts，没有 DISCONTINUITY。
	singleID := stream.ID(secret, singleUID)
	rec := serve(t, h, http.MethodGet, "/video/"+singleID+".m3u8")
	if rec.Code != http.StatusOK {
		t.Fatalf("单集 playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("Content-Type = %q, 期望 application/vnd.apple.mpegurl", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "#EXTM3U\n") || !strings.HasSuffix(body, "#EXT-X-ENDLIST\n") {
		t.Errorf("单集 playlist 不是完整 VOD：\n%s", body)
	}
	if !strings.Contains(body, "/video/"+singleID+"/0.ts") {
		t.Errorf("playlist 分片 URI 没指回 /video/{id}/{seq}.ts：\n%s", body)
	}
	if strings.Contains(body, "#EXT-X-DISCONTINUITY") {
		t.Errorf("单集 playlist 不该有 DISCONTINUITY：\n%s", body)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %q, body = %d 字节", cl, rec.Body.Len())
	}

	// 合并集：同一路径形态，集边界带 DISCONTINUITY。
	mergedID := stream.ID(secret, mergedUID)
	rec = serve(t, h, http.MethodGet, "/video/"+mergedID+".m3u8")
	if rec.Code != http.StatusOK {
		t.Fatalf("合并集 playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "#EXT-X-DISCONTINUITY") {
		t.Errorf("合并集 playlist 缺集边界 DISCONTINUITY：\n%s", rec.Body.String())
	}

	// 签名错 → 401；uid 在库里不存在 → 404；没有 .m3u8 后缀 → 404。
	if rec := serve(t, h, http.MethodGet, "/video/"+singleUID+"-000000000000.m3u8"); rec.Code != http.StatusUnauthorized {
		t.Errorf("坏签名状态码 = %d, 期望 401", rec.Code)
	}
	if rec := serve(t, h, http.MethodGet, "/video/"+stream.ID(secret, "e9999")+".m3u8"); rec.Code != http.StatusNotFound {
		t.Errorf("未知剧集状态码 = %d, 期望 404", rec.Code)
	}
	if rec := serve(t, h, http.MethodGet, "/video/"+singleID); rec.Code != http.StatusNotFound {
		t.Errorf("缺 .m3u8 后缀状态码 = %d, 期望 404", rec.Code)
	}

	// 每请求恰一条访问日志，path 就是 /video 的真实路径（id 里没有 secret）。
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	requestLogs := 0
	for _, line := range lines {
		var entry struct {
			Msg  string `json:"msg"`
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("日志不是合法 JSON: %v（line=%q）", err, line)
		}
		if entry.Msg == "http request" && entry.Path == "/video/"+singleID+".m3u8" {
			requestLogs++
		}
	}
	if requestLogs != 1 {
		t.Errorf("/video playlist 的 http request 日志条数 = %d, 期望 1（buf=%q）", requestLogs, buf.String())
	}
}

// TestIDStability 覆盖契约的「{id} 不飘移」四条：
// 1. 重复取一致；2. 全量刷新（UpsertEpisode + RebuildEmbyItems）后一致；
// 3. 建/删合并组只让受影响那一集的 id 变化（旧 id 404、其它集 id 一个都不变）；
// 4. 重启进程（settings 不变）后一致。
func TestIDStability(t *testing.T) {
	ctx := context.Background()
	const secret = "0123456789abcdef0123456789abcdef"
	dsn := pgtest.DSN(t)
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	for k, v := range map[store.SettingKey]string{
		store.KeyStreamSecret: secret,
		store.KeyCacheDir:     filepath.Join(t.TempDir(), "cache"),
	} {
		if err := settings.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	dramaID, err := st.UpsertDrama(ctx, store.Drama{Provider: "web", ProviderID: "p1", Title: "稳定剧", Year: 2026})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	var ids []int64
	for i := 1; i <= 3; i++ {
		id, err := st.UpsertEpisode(ctx, store.Episode{DramaID: dramaID, EpNo: i, Source: "web", Vid: "v"})
		if err != nil {
			t.Fatalf("UpsertEpisode: %v", err)
		}
		ids = append(ids, id)
	}
	if _, err := st.RebuildEmbyItems(ctx, dramaID); err != nil {
		t.Fatalf("RebuildEmbyItems: %v", err)
	}

	// idsOf 把派生表的 uid 翻成对外 id（生产方与校验方共用 stream.ID）。
	idsOf := func(st *store.Store) map[string]string {
		t.Helper()
		items, err := st.ListEmbyItems(ctx, dramaID)
		if err != nil {
			t.Fatalf("ListEmbyItems: %v", err)
		}
		out := make(map[string]string, len(items))
		for _, item := range items {
			out[item.UID] = stream.ID(secret, item.UID)
		}
		return out
	}
	assertSame := func(name string, got, want map[string]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s：条目数从 %d 变为 %d", name, len(want), len(got))
		}
		for uid, id := range want {
			if got[uid] != id {
				t.Errorf("%s：uid %s 的 id 从 %q 变为 %q", name, uid, id, got[uid])
			}
		}
	}

	before := idsOf(st)
	// 1. 重复取一致。
	assertSame("重复取", idsOf(st), before)
	if len(before) != 3 {
		t.Fatalf("对外剧集数 = %d, 期望 3", len(before))
	}

	// 2. 全量刷新（同一批 UpsertEpisode + RebuildEmbyItems）后一致。
	for i, id := range ids {
		if _, err := st.UpsertEpisode(ctx, store.Episode{ID: id, DramaID: dramaID, EpNo: i + 1, Source: "web", Vid: "v"}); err != nil {
			t.Fatalf("再次 UpsertEpisode: %v", err)
		}
	}
	if _, err := st.RebuildEmbyItems(ctx, dramaID); err != nil {
		t.Fatalf("再次 RebuildEmbyItems: %v", err)
	}
	assertSame("全量刷新后", idsOf(st), before)

	// 4. 重启（重开库、重建 Store / Settings，secret 不变）后一致。
	db2, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("重开库: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	assertSame("重启后", idsOf(store.NewStore(db2.DB)), before)

	// 3. 建合并组：只让被合并的两集从单集 uid 空间消失，第 1 集的 id 不变。
	gid, err := st.CreateMergeGroup(ctx, dramaID, "第2-3集", []int64{ids[1], ids[2]})
	if err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}
	merged := idsOf(st)
	firstUID := "e" + strconv.FormatInt(ids[0], 10)
	mergedUID := "m" + strconv.FormatInt(gid, 10)
	if merged[firstUID] != before[firstUID] {
		t.Errorf("未受影响的第 1 集 id 变了：%q → %q", before[firstUID], merged[firstUID])
	}
	if len(merged) != 2 {
		t.Fatalf("建组后对外剧集数 = %d, 期望 2（单集 + 合并集）：%v", len(merged), merged)
	}
	if _, ok := merged[mergedUID]; !ok {
		t.Fatalf("合并集 uid %s 不在派生表里：%v", mergedUID, merged)
	}
	// 旧 id 仍合法签名，但对应 uid 已不在库里 → 404（与 401 区分开）。
	h := newAssembly(slog.New(slog.DiscardHandler), st, settings, testSyncer(), fakeSchedule{}, testSources(), nil)
	if rec := serve(t, h, http.MethodGet, "/video/"+before["e"+strconv.FormatInt(ids[1], 10)]+".m3u8"); rec.Code != http.StatusNotFound {
		t.Errorf("被合并集的旧 id 状态码 = %d, 期望 404", rec.Code)
	}

	// 删组后回到 3 条单集，id 与建组前逐个相同（旧 id 复活而不是换新值）。
	if err := st.DeleteMergeGroup(ctx, gid); err != nil {
		t.Fatalf("DeleteMergeGroup: %v", err)
	}
	assertSame("删组后", idsOf(st), before)
}

// 装配根显式注册插件（不用 init()），并且 plugin.<源>.* 配置真的进到插件：
// 把 base_url 指向假官网后，管理 API 的同步请求就打在这个假官网上。
func TestPluginConfigReachesPlugin(t *testing.T) {
	ctx := context.Background()
	// handler 在自己的 goroutine 里跑，用 channel 收请求路径：既不用锁也不用 sleep。
	paths := make(chan string, 4)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(site.Close)

	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	if err := settings.Set(ctx, store.PluginSettingKey(hongguo.Name, store.PluginItemBaseURL), site.URL); err != nil {
		t.Fatalf("Set base_url: %v", err)
	}
	if err := settings.Set(ctx, store.PluginSettingKey(hongguo.Name, store.PluginItemRequestTimeout), "5"); err != nil {
		t.Fatalf("Set request_timeout_sec: %v", err)
	}

	logger := slog.New(slog.DiscardHandler)
	reg, boots, err := newPlugins(ctx, settings, logger)
	if err != nil {
		t.Fatalf("newPlugins: %v", err)
	}
	p, ok := reg.Get(hongguo.Name)
	if !ok {
		t.Fatalf("注册表里没有 %q：装配根必须显式注册内置插件", hongguo.Name)
	}
	// 播放面的出站身份取的是同一份 boots：MKACG-616 把 base_url 改成经 pluginBoot
	// 注入后，这条缝最容易退化成「页面用了配置、ffmpeg 用了默认」。
	// 两个通道各一份身份（MKACG-638）：网页通道 = UA + 站点根 Referer；App 通道 =
	// 只有 UA —— 它的媒体 CDN 对带站内 Referer 的请求回 403，多带一行就是整集 502。
	hdr := playbackHeaders(boots)
	if len(hdr.Web) != 2 || hdr.Web[1] != "Referer: "+site.URL+"/" {
		t.Fatalf("网页通道身份 = %q, 期望 UA + Referer %q（跟随 pluginBoot.BaseURL）",
			hdr.Web, "Referer: "+site.URL+"/")
	}
	if len(hdr.App) != 1 || !strings.HasPrefix(hdr.App[0], "User-Agent: ") {
		t.Fatalf("App 通道身份 = %q, 期望只有一份浏览器 UA（不带 Referer）", hdr.App)
	}
	h := newAssembly(logger, st, settings, sync.New(p, nil, logger), fakeSchedule{}, sourceList(reg, boots), reg)

	req := httptest.NewRequest(http.MethodPost, "/api/dramas", strings.NewReader(`{"series_id":"7503745441910017049"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(web.AdminTokenHeader, testAdminToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("POST /api/dramas 状态码 = %d, 期望 502（上游 500）", rec.Code)
	}
	select {
	case got := <-paths:
		if got != "/player/7503745441910017049" {
			t.Fatalf("上游请求路径 = %q, 期望 /player/<id>", got)
		}
	default:
		t.Fatal("同步没有打到配置的 base_url")
	}

	// 状态页读的是同一组配置键，值的来源只有 settings。
	statusRec := serve(t, h, http.MethodGet, "/api/status")
	var status struct {
		Sources []struct {
			Name              string `json:"name"`
			Enabled           bool   `json:"enabled"`
			BaseURL           string `json:"base_url"`
			RequestTimeoutSec int    `json:"request_timeout_sec"`
			Reason            string `json:"reason"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("解析 /api/status: %v", err)
	}
	if len(status.Sources) != 1 {
		t.Fatalf("sources = %+v, 期望 1 个源", status.Sources)
	}
	src := status.Sources[0]
	if src.Name != hongguo.Name || !src.Enabled || src.BaseURL != site.URL || src.RequestTimeoutSec != 5 || src.Reason != "" {
		t.Fatalf("状态页的源 = %+v, 期望 web 启用 + 配置的 base_url + 5 秒超时", src)
	}
}

// 插件被停用（没有任何配置行时默认启用）时服务照常启动，状态页给出原因。
func TestPluginDisabledStillStarts(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)

	// 先验「配置缺失照常启动」：没有任何 plugin.* 行，注册仍然成功。
	logger := slog.New(slog.DiscardHandler)
	// 第一次只验「注册成功」；第二次才用得到 boots（状态页的生效值）。
	firstReg, _, err := newPlugins(ctx, settings, logger)
	if err != nil {
		t.Fatalf("配置缺失时 newPlugins 报错：%v", err)
	}
	if _, ok := firstReg.Get(hongguo.Name); !ok {
		t.Fatalf("配置缺失时 %q 未注册", hongguo.Name)
	}

	if err := settings.Set(ctx, store.PluginSettingKey(hongguo.Name, store.PluginItemEnabled), "false"); err != nil {
		t.Fatalf("Set enabled: %v", err)
	}
	reg, boots, err := newPlugins(ctx, settings, logger)
	if err != nil {
		t.Fatalf("停用时 newPlugins 报错：%v", err)
	}
	h := newAssembly(logger, st, settings, sync.New(mustPlugin(reg, hongguo.Name), nil, logger), fakeSchedule{}, sourceList(reg, boots), reg)

	rec := serve(t, h, http.MethodGet, "/api/status")
	var status struct {
		Sources []struct {
			Enabled bool   `json:"enabled"`
			Reason  string `json:"reason"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("解析 /api/status: %v", err)
	}
	if len(status.Sources) != 1 {
		t.Fatalf("sources = %+v, 期望 1 个源", status.Sources)
	}
	if status.Sources[0].Enabled || !strings.Contains(status.Sources[0].Reason, "enabled=false") {
		t.Fatalf("停用的源 = %+v, 期望 enabled=false 且原因里带配置键", status.Sources[0])
	}
}

// g4Source 现场造一段 seconds 秒的真实视频并用假 CDN 提供（ffprobe 按 Range 读源），
// 返回（直链, ffprobe 独立测得的时长）。时长真值来自 ffprobe，不取 fixture 字面值。
func g4Source(t *testing.T, seconds int) (string, float64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("ep%d.mp4", seconds))
	out, err := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=160x120:rate=10:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-g", "10", "-keyint_min", "10", "-sc_threshold", "0", path,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("生成测试视频失败: %v（%s）", err, out)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 fixture: %v", err)
	}
	raw, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	dur, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	if err != nil {
		t.Fatalf("解析 ffprobe 时长 %q: %v", raw, err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "ep.mp4", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/ep.mp4", dur
}

// TestFirstPlay_BackfillsDurationIntoNFO 覆盖 G4（MKACG-621）：首播触发建索引时把
// ffprobe 实测时长回填 episode.duration_sec，同事务重建派生表后重新取该集 .nfo，
// <runtime>（分钟）与实测时长一致；没播过的集仍是 0，它的 .nfo 照常出、不阻断。
func TestFirstPlay_BackfillsDurationIntoNFO(t *testing.T) {
	ctx := context.Background()
	const secret = "g4-backfill-duration-test-secret"
	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	for k, v := range map[store.SettingKey]string{
		store.KeyStreamSecret: secret,
		store.KeySegTargetSec: "10",
		// 缓存目录必须在临时目录里：默认的 ./data/cache 会写进源码树。
		store.KeyCacheDir: filepath.Join(t.TempDir(), "cache"),
	} {
		if err := settings.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	dramaID, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "web", ProviderID: "g1", Title: "回填剧", Year: 2026, Genre: "爱情",
	})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	urls := map[int64]string{}
	// 65 秒（播过 → 回填 65 秒 = 1 分钟）与 12 秒（没播过 → 仍为 0）。
	addEpisode := func(epNo, seconds int) (int64, float64) {
		u, dur := g4Source(t, seconds)
		id, err := st.UpsertEpisode(ctx, store.Episode{
			DramaID: dramaID, EpNo: epNo, Source: "web", Vid: fmt.Sprintf("v%d", epNo),
		})
		if err != nil {
			t.Fatalf("UpsertEpisode %d: %v", epNo, err)
		}
		urls[id] = u
		return id, dur
	}
	playedID, playedDur := addEpisode(1, 65)
	unplayedID, _ := addEpisode(2, 12)
	// vfs 树与播放面都只认 emby_item 派生表（硬约束 6），等价于同步完成后的重建。
	if _, err := st.RebuildEmbyItems(ctx, dramaID); err != nil {
		t.Fatalf("RebuildEmbyItems: %v", err)
	}

	logger := slog.New(slog.DiscardHandler)
	cutter := segment.NewCutter(st, settings, logger, cache.New(st, settings, logger),
		func(_ context.Context, ep store.Episode) (stream.Direct, error) {
			return stream.Direct{URL: urls[ep.ID]}, nil
		},
		playbackHeaders(nil))
	mux := http.NewServeMux()
	mux.HandleFunc(segment.PlaylistRoute, cutter.ServePlaylist)
	tree := vfs.NewTree(st, settings, stream.ID, logger)

	season := "短剧/爱情/" + shardOf("g1") + "/回填剧 (2026)/Season 01"
	nfoOf := func(epNo int) string {
		return fmt.Sprintf("%s/回填剧 - S01E%02d - 第%d集.nfo", season, epNo, epNo)
	}

	// 播放前：两集都是 0，.nfo 的 <runtime> 是 0 —— G4 要修的现状。
	for _, epNo := range []int{1, 2} {
		body, err := tree.Render(ctx, nfoOf(epNo))
		if err != nil {
			t.Fatalf("播放前取第 %d 集 .nfo: %v", epNo, err)
		}
		if want := "<runtime>0</runtime>"; !strings.Contains(string(body), want) {
			t.Errorf("播放前第 %d 集 .nfo 缺 %s：%s", epNo, want, body)
		}
	}
	before, err := st.EpisodeByID(ctx, playedID)
	if err != nil {
		t.Fatalf("EpisodeByID 播放前: %v", err)
	}
	t.Logf("播放前：episode.duration_sec=%d（ffprobe 实测 %.3f 秒）", before.DurationSec, playedDur)

	// 播第 1 集 → 建索引 + 回填。
	playID := "/video/" + stream.ID(secret, "e"+strconv.FormatInt(playedID, 10)) + ".m3u8"
	if rec := serve(t, mux, http.MethodGet, playID); rec.Code != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d, 期望 200（body: %s）", playID, rec.Code, rec.Body)
	}

	got, err := st.EpisodeByID(ctx, playedID)
	if err != nil {
		t.Fatalf("EpisodeByID: %v", err)
	}
	if want := int(math.Round(playedDur)); got.DurationSec != want {
		t.Errorf("回填 duration_sec = %d, 期望 ffprobe 实测 %d（%.3f 秒）", got.DurationSec, want, playedDur)
	}

	// 重新取该集 .nfo：<runtime> 单位是分钟（Emby 用 TimeSpan.FromMinutes）。
	body, err := tree.Render(ctx, nfoOf(1))
	if err != nil {
		t.Fatalf("回填后取第 1 集 .nfo: %v", err)
	}
	if want := fmt.Sprintf("<runtime>%d</runtime>", got.DurationSec/60); !strings.Contains(string(body), want) {
		t.Errorf("回填后第 1 集 .nfo 缺 %s：%s", want, body)
	}
	items, err := st.ListEmbyItems(ctx, dramaID)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	t.Logf("回填后：episode.duration_sec=%d, emby_item.runtime_sec=%d, .nfo=%s",
		got.DurationSec, items[0].RuntimeSec, strings.TrimSpace(string(body)))

	// 没播过的集：库里仍是 0，.nfo 照常出、仍是 0（不报错、不阻断）。
	ep, err := st.EpisodeByID(ctx, unplayedID)
	if err != nil {
		t.Fatalf("EpisodeByID 未播放集: %v", err)
	}
	if ep.DurationSec != 0 {
		t.Errorf("未播放集 duration_sec = %d, 期望 0", ep.DurationSec)
	}
	body, err = tree.Render(ctx, nfoOf(2))
	if err != nil {
		t.Fatalf("未播放集取 .nfo: %v", err)
	}
	if want := "<runtime>0</runtime>"; !strings.Contains(string(body), want) {
		t.Errorf("未播放集 .nfo 缺 %s：%s", want, body)
	}
}

// AC：数据库不可用时启动路径 fail fast —— openDB 返回错误、main 记日志后 os.Exit(1)。
// 这里钉住错误返回（退出码由 main 的那一行 os.Exit(1) 保证）：
//  1. 没设 DRAMA_HUB_DB_URL；
//  2. 连不上；
//  3. 正常时把 schema 版本报给日志（启动日志的唯一来源）。
func TestOpenDBFailsFast(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	ctx := context.Background()

	t.Setenv(store.EnvDBURL, "")
	if _, err := openDB(ctx, logger); err == nil {
		t.Fatal("未设 DRAMA_HUB_DB_URL 时 openDB 未报错")
	}

	t.Setenv(store.EnvDBURL, "postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if _, err := openDB(ctx, logger); err == nil {
		t.Fatal("连不上数据库时 openDB 未报错")
	}

	t.Setenv(store.EnvDBURL, pgtest.DSN(t))
	db, err := openDB(ctx, logger)
	if err != nil {
		t.Fatalf("正常配置 openDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if db.SchemaVersion == 0 {
		t.Fatal("openDB 未把 schema 版本报出来（启动日志没有版本可打）")
	}
}

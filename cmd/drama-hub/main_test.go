package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/plugin/hongguo"
	"github.com/justforlxz/drama-hub/internal/scheduler"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
	"github.com/justforlxz/drama-hub/internal/sync"
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
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cmd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return newAssembly(slog.New(slog.DiscardHandler), store.NewStore(db.DB), store.NewSettings(db.DB),
		testSyncer(), fakeSchedule{}, testSources(), nil)
}

// newAssembly 按 main 的装配顺序造 handler：缓存实例 + 直链解析器 + 路由表。
// reg 为 nil 时用一个空注册表（任何 Resolve 都会失败，播放面据此回 502）。
func newAssembly(logger *slog.Logger, st *store.Store, settings *store.Settings, syncer web.SeriesSyncer, schedule web.Schedule, sources []web.Source, reg *plugin.Registry) http.Handler {
	if reg == nil {
		reg = plugin.NewRegistry()
	}
	return newHandler(logger, st, settings, syncer, schedule, sources,
		cache.New(st, settings, logger), stream.NewResolver(st, reg))
}

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
	return sync.New(hongguo.New(hongguo.Config{BaseURL: "http://127.0.0.1:1", Logger: logger}), nil)
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
			db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "log.db"))
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
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "dav.db"))
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
	if want := "/video/" + stream.ID(secret, uid) + ".m3u8\n"; body != want {
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

// TestHandler_PlaybackSurface 覆盖 2.0 播放面的装配：两个口子挂在 `/video`，
// 单集与合并集同一路径形态；签名错 401、uid 不存在 404；每请求恰一条访问日志。
func TestHandler_PlaybackSurface(t *testing.T) {
	ctx := context.Background()
	const secret = "0123456789abcdef0123456789abcdef"
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "video.db"))
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
	for _, id := range ids {
		if err := st.ReplaceSegments(ctx, id, []store.Segment{
			{EpisodeID: id, StartSec: 0, Dur: 4, Keyframe: true, Mode: "copy", CodecSig: "h264/High/40/yuv420p/[0][0][0][0]/1/12800"},
			{EpisodeID: id, StartSec: 4, Dur: 2, Keyframe: true, Mode: "copy", CodecSig: "h264/High/40/yuv420p/[0][0][0][0]/1/12800"},
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
	path := filepath.Join(t.TempDir(), "stable.db")
	db, err := store.Open(ctx, path)
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
	db2, err := store.Open(ctx, path)
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

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "plugin.db"))
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
	reg, err := newPlugins(ctx, settings, logger)
	if err != nil {
		t.Fatalf("newPlugins: %v", err)
	}
	p, ok := reg.Get(hongguo.Name)
	if !ok {
		t.Fatalf("注册表里没有 %q：装配根必须显式注册内置插件", hongguo.Name)
	}
	h := newAssembly(logger, st, settings, sync.New(p, nil), fakeSchedule{}, sourceList(reg), reg)

	req := httptest.NewRequest(http.MethodPost, "/api/dramas", strings.NewReader(`{"series_id":"7503745441910017049"}`))
	req.Header.Set("Content-Type", "application/json")
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
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "disabled.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)

	// 先验「配置缺失照常启动」：没有任何 plugin.* 行，注册仍然成功。
	logger := slog.New(slog.DiscardHandler)
	reg, err := newPlugins(ctx, settings, logger)
	if err != nil {
		t.Fatalf("配置缺失时 newPlugins 报错：%v", err)
	}
	if _, ok := reg.Get(hongguo.Name); !ok {
		t.Fatalf("配置缺失时 %q 未注册", hongguo.Name)
	}

	if err := settings.Set(ctx, store.PluginSettingKey(hongguo.Name, store.PluginItemEnabled), "false"); err != nil {
		t.Fatalf("Set enabled: %v", err)
	}
	reg, err = newPlugins(ctx, settings, logger)
	if err != nil {
		t.Fatalf("停用时 newPlugins 报错：%v", err)
	}
	h := newAssembly(logger, st, settings, sync.New(mustPlugin(reg, hongguo.Name), nil), fakeSchedule{}, sourceList(reg), reg)

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

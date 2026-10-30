package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/justforlxz/drama-hub/internal/stream"
)

// newTestHandler 与 main 用同一条装配路径：httptest 起真实 handler，不 mock 自己的
// handler。出流端点需要数据库（settings / episode 表），因此开一个临时库装配。
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cmd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return newHandler(slog.New(slog.DiscardHandler), store.NewStore(db.DB), store.NewSettings(db.DB))
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

// TestHandler_UnimplementedMounts 覆盖仍是占位的挂载点语义：
// 任意方法、任意子路径都是 501（AC2/AC3/AC4/AC7）。
// /dav/ 已在 M4 接上真实 WebDAV，其契约归 internal/webdav（含「无认证 → 401」）。
func TestHandler_UnimplementedMounts(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodGet, "/s/x"},
		{http.MethodGet, "/api/"},
		{http.MethodGet, "/api/x"},
		{http.MethodPost, "/api/x/y"},
	}
	h := newTestHandler(t)
	for _, c := range cases {
		rec := serve(t, h, c.method, c.path)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s 状态码 = %d, 期望 %d", c.method, c.path, rec.Code, http.StatusNotImplemented)
		}
	}
}

// TestHandler_UnknownPathNotFound 锁定「未装配路径 → 404」（AC5/AC6）。
// 一旦有人加了 "/" catch-all，这条会立刻变红。
func TestHandler_UnknownPathNotFound(t *testing.T) {
	for _, path := range []string{"/nope", "/"} {
		rec := serve(t, newTestHandler(t), http.MethodGet, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 %d", path, rec.Code, http.StatusNotFound)
		}
	}
}

// TestHandler_LogsRequests（F5 AC7）证明装配根每请求恰一条访问日志：对
// /healthz、/dav/（无认证 → 401）、/nope 各发一次，每次 1 条含 method/path/status/duration_ms
// 四字段的 JSON 日志，路径与状态和响应一致。logger 级别设为 Debug，避免级别
// 过滤影响条数。
func TestHandler_LogsRequests(t *testing.T) {
	cases := []struct {
		path     string
		wantPath string
		wantCode int
	}{
		{"/healthz", "/healthz", http.StatusOK},
		{"/dav/", "/dav/", http.StatusUnauthorized},
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
			h := newHandler(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
				store.NewStore(db.DB), store.NewSettings(db.DB))

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

// TestHandler_StreamEpisode 覆盖 AC13 与 AC10：装配后的 `/s/{token}/e/{uid}`
// 真的走到出流（不是只剩 501）；每请求恰一条 `http request` 日志，token 段被
// 掩码为 {token}，全部日志里都不出现 token 明文。
func TestHandler_StreamEpisode(t *testing.T) {
	body := bytes.Repeat([]byte("drama"), 1024) // 5120 字节
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(cdn.Close)

	const secret = "0123456789abcdef0123456789abcdef"
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "stream.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	if err := settings.Set(context.Background(), store.KeyStreamSecret, secret); err != nil {
		t.Fatalf("Set stream_secret: %v", err)
	}
	dramaID, err := st.UpsertDrama(context.Background(), store.Drama{Provider: "web", ProviderID: "p1", Title: "测试剧"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	epID, err := st.UpsertEpisode(context.Background(), store.Episode{
		DramaID: dramaID, EpNo: 1, Source: "web", Vid: "v1", PlayURL: cdn.URL + "/video.mp4",
	})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}
	uid := "e" + strconv.FormatInt(epID, 10)
	token := stream.Sign([]byte(secret), uid)
	h := newHandler(logger, st, settings)

	rec := serve(t, h, http.MethodGet, "/s/"+token+"/e/"+uid)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（/s/ 仍返回 501 说明装配没接上）", rec.Code)
	}
	if rec.Body.Len() != len(body) {
		t.Errorf("body 长度 = %d, 期望 %d", rec.Body.Len(), len(body))
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, 期望 %d", cl, len(body))
	}

	// AC10：`http request` 日志恰一条，且路径掩码。
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
		if entry.Msg == "http request" {
			requestLogs++
			if want := "/s/{token}/e/" + uid; entry.Path != want {
				t.Errorf("日志 path = %q, 期望 %q", entry.Path, want)
			}
		}
	}
	if requestLogs != 1 {
		t.Errorf("http request 日志条数 = %d, 期望 1（buf=%q）", requestLogs, buf.String())
	}
	if strings.Contains(buf.String(), token) {
		t.Errorf("日志里出现 token 明文")
	}

	// /s/x 仍 501（既有语义不变）。
	if rec := serve(t, h, http.MethodGet, "/s/x"); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /s/x 状态码 = %d, 期望 %d", rec.Code, http.StatusNotImplemented)
	}
}

// TestHandler_SegmentRoute 覆盖 M6 的装配：分片端点挂在 `/s/` 子树下、比
// stream.Server 的 `/s/` 更具体，因此命中切片 handler（不是 501）；`/s/` 其余
// 路径仍是 501。签名校验用 cmd 注入的 stream.Verify，所以错 token → 401。
func TestHandler_SegmentRoute(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "seg.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	if err := settings.Set(context.Background(), store.KeyStreamSecret, secret); err != nil {
		t.Fatalf("Set stream_secret: %v", err)
	}
	buf := &bytes.Buffer{}
	h := newHandler(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), st, settings)

	// 正确签名 + 库中不存在的集：走到 handler 后是 404，而不是 501 兜底。
	uid := "e999"
	token := stream.Sign([]byte(secret), uid)
	rec := serve(t, h, http.MethodGet, "/s/"+token+"/seg/"+uid+"/0.ts")
	if rec.Code != http.StatusNotFound {
		t.Errorf("分片端点状态码 = %d, 期望 404（501 说明装配没接上）", rec.Code)
	}

	// 错签名 → 401（证明校验用的是注入的 stream.Verify）。
	rec = serve(t, h, http.MethodGet, "/s/bogus/seg/"+uid+"/0.ts")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("错 token 状态码 = %d, 期望 401", rec.Code)
	}

	// `/s/` 其余子路径仍 501（M5 语义不回归）。
	if rec = serve(t, h, http.MethodGet, "/s/x"); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /s/x 状态码 = %d, 期望 501", rec.Code)
	}

	// 访问日志：分片路径的 token 段被掩码，且日志里不出现 token 明文。
	var sawSeg bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var entry struct {
			Msg  string `json:"msg"`
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("日志不是合法 JSON: %v（line=%q）", err, line)
		}
		if entry.Msg == "http request" && strings.Contains(entry.Path, "/seg/") {
			sawSeg = true
			if want := "/s/{token}/seg/" + uid + "/0.ts"; entry.Path != want {
				t.Errorf("分片日志 path = %q, 期望 %q", entry.Path, want)
			}
		}
	}
	if !sawSeg {
		t.Error("没有看到分片请求的访问日志")
	}
	if strings.Contains(buf.String(), token) {
		t.Error("日志里出现 token 明文")
	}
}

// TestHandler_WebDAV 覆盖 M4 的装配面：`/dav/` 已从占位 501 换成真实 WebDAV
// （无认证 401；带认证 PROPFIND 207；strm 里的 token 由装配根的 signToken 生成，
// 与 internal/stream 的签名一致——这条断言能挡住 TokenFunc 接错）。
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
	dramaID, err := st.UpsertDrama(ctx, store.Drama{Provider: "web", ProviderID: "p1", Title: "测试剧", Year: 2024})
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
	h := newHandler(slog.New(slog.DiscardHandler), st, settings)

	// 无认证 → 401（不是 501：说明 /dav/ 已接上真实 handler）。
	if rec := serve(t, h, "PROPFIND", "/dav/"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("PROPFIND /dav/（无认证）状态码 = %d, 期望 401", rec.Code)
	}

	// 带认证 PROPFIND → 207。
	req := httptest.NewRequest("PROPFIND", "/dav/", nil)
	req.SetBasicAuth(user, pass)
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /dav/ 状态码 = %d, 期望 207", rec.Code)
	}

	// strm 里的 token/uid 与出流端点一致：Sign(secret, episode.id) + `/e/<id>`。
	// 文件名含空格与中文，请求目标必须是转义后的形式（真实客户端也这样发）。
	path := "/dav/" + url.PathEscape("测试剧 (2024)") + "/Season%2001/" +
		url.PathEscape("测试剧 - S01E01 - 开局") + ".strm"
	req = httptest.NewRequest(http.MethodGet, path, nil)
	req.SetBasicAuth(user, pass)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d, 期望 200", path, rec.Code)
	}
	uid := "e" + strconv.FormatInt(epID, 10)
	want := "/s/" + stream.Sign([]byte(secret), uid) + "/e/" + uid + "\n"
	if rec.Body.String() != want {
		t.Errorf("strm 内容 = %q, 期望 %q", rec.Body.String(), want)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %q, body 长度 = %d", cl, rec.Body.Len())
	}
}

// TestHandler_MergedPlaylist 覆盖 M7 的装配面：合并集 uid 走自造 m3u8（不是单集
// MP4 透传、也不是 501），playlist 里带 DISCONTINUITY，且分片 URL 指回分片端点。
func TestHandler_MergedPlaylist(t *testing.T) {
	ctx := context.Background()
	const secret = "0123456789abcdef0123456789abcdef"
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "m7.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	if err := settings.Set(ctx, store.KeyStreamSecret, secret); err != nil {
		t.Fatalf("Set stream_secret: %v", err)
	}
	dramaID, err := st.UpsertDrama(ctx, store.Drama{Provider: "web", ProviderID: "m7", Title: "合并剧", Year: 2026})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	var ids []int64
	for i := 1; i <= 2; i++ {
		id, err := st.UpsertEpisode(ctx, store.Episode{DramaID: dramaID, EpNo: i, Source: "web", Vid: "v"})
		if err != nil {
			t.Fatalf("UpsertEpisode: %v", err)
		}
		ids = append(ids, id)
	}
	// 装配根不预置索引（那是索引任务的事），这里直接写两集各 2 片，让时间轴可用。
	for _, id := range ids {
		if err := st.ReplaceSegments(ctx, id, []store.Segment{
			{EpisodeID: id, StartSec: 0, Dur: 4, Keyframe: true, Mode: "copy"},
			{EpisodeID: id, StartSec: 4, Dur: 2, Keyframe: true, Mode: "copy"},
		}); err != nil {
			t.Fatalf("ReplaceSegments: %v", err)
		}
	}
	gid, err := st.CreateMergeGroup(ctx, dramaID, "第1-2集", ids)
	if err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}
	h := newHandler(slog.New(slog.DiscardHandler), st, settings)

	uid := "m" + strconv.FormatInt(gid, 10)
	token := stream.Sign([]byte(secret), uid)

	// 合并集 uid 在 `/s/{token}/e/{uid}` 上返回 playlist，不是 501、不是 MP4。
	rec := serve(t, h, http.MethodGet, "/s/"+token+"/e/"+uid)
	if rec.Code != http.StatusOK {
		t.Fatalf("合并集出流状态码 = %d, 期望 200（501 说明装配没接上）", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("Content-Type = %q, 期望 application/vnd.apple.mpegurl", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "#EXT-X-DISCONTINUITY") {
		t.Errorf("合并集 playlist 缺集边界 DISCONTINUITY：\n%s", body)
	}
	if !strings.Contains(body, "/s/"+token+"/seg/"+uid+"/0.ts") {
		t.Errorf("playlist 分片 URL 未指回分片端点：\n%s", body)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %q, body = %d 字节", cl, rec.Body.Len())
	}

	// /master.m3u8 别名同样返回 playlist；单集 uid 走别名是 404。
	rec = serve(t, h, http.MethodGet, "/s/"+token+"/e/"+uid+"/master.m3u8")
	if rec.Code != http.StatusOK {
		t.Errorf("master.m3u8 别名状态码 = %d, 期望 200", rec.Code)
	}
	singleUID := "e" + strconv.FormatInt(ids[0], 10)
	singleTok := stream.Sign([]byte(secret), singleUID)
	if rec = serve(t, h, http.MethodGet, "/s/"+singleTok+"/e/"+singleUID+"/master.m3u8"); rec.Code != http.StatusNotFound {
		t.Errorf("单集 uid 请求 master.m3u8 状态码 = %d, 期望 404", rec.Code)
	}

	// 单集 uid 仍走 MP4 透传（不回归成 playlist）：play_url 为空 → 上游失败类别 502。
	if rec = serve(t, h, http.MethodGet, "/s/"+singleTok+"/e/"+singleUID); rec.Code != http.StatusBadGateway {
		t.Errorf("单集 uid 状态码 = %d, 期望 502（play_url 为空，说明仍走单集出流）", rec.Code)
	}

	// 坏签名的合并集请求 → 401（装配根注入的 stream.Verify 生效）。
	if rec = serve(t, h, http.MethodGet, "/s/bogus/e/"+uid); rec.Code != http.StatusUnauthorized {
		t.Errorf("坏签名合并集状态码 = %d, 期望 401", rec.Code)
	}
}

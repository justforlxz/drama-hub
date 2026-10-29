package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// TestHandler_UnimplementedMounts 覆盖三个挂载点的占位语义：
// 任意方法、任意子路径都是 501（AC2/AC3/AC4/AC7）。
func TestHandler_UnimplementedMounts(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodGet, "/dav/"},
		{http.MethodPost, "/dav/"},
		{http.MethodPut, "/dav/x/y"},
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
// /healthz、/dav/、/nope 各发一次，每次 1 条含 method/path/status/duration_ms
// 四字段的 JSON 日志，路径与状态和响应一致。logger 级别设为 Debug，避免级别
// 过滤影响条数。
func TestHandler_LogsRequests(t *testing.T) {
	cases := []struct {
		path     string
		wantPath string
		wantCode int
	}{
		{"/healthz", "/healthz", http.StatusOK},
		{"/dav/", "/dav/", http.StatusNotImplemented},
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

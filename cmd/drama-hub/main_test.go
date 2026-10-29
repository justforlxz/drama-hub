package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestHandler 与 main 用同一条装配路径：httptest 起真实 handler，不 mock 自己的 handler。
func newTestHandler() http.Handler {
	return newHandler(slog.New(slog.DiscardHandler))
}

func serve(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// TestHandler_Healthz 覆盖 /healthz 的响应契约（AC1）。
func TestHandler_Healthz(t *testing.T) {
	rec := serve(t, newTestHandler(), http.MethodGet, "/healthz")

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
	h := newTestHandler()
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
		rec := serve(t, newTestHandler(), http.MethodGet, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 %d", path, rec.Code, http.StatusNotFound)
		}
	}
}

package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestServer_Healthz_ReturnsOK 覆盖健康检查端点的响应契约。
func TestServer_Healthz_ReturnsOK(t *testing.T) {
	srv := NewServer(slog.New(slog.DiscardHandler))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

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

package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStatusCode 覆盖 AC1：四哨兵（裸与 %w 包装）→ 404/400/502/503，无关错误 → 500。
func TestStatusCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"裸 ErrNotFound", ErrNotFound, http.StatusNotFound},
		{"包装 ErrNotFound", fmt.Errorf("加载剧集 %s: %w", "42", ErrNotFound), http.StatusNotFound},
		{"裸 ErrInvalid", ErrInvalid, http.StatusBadRequest},
		{"包装 ErrInvalid", fmt.Errorf("范围 %d 越界: %w", 7, ErrInvalid), http.StatusBadRequest},
		{"裸 ErrUpstream", ErrUpstream, http.StatusBadGateway},
		{"包装 ErrUpstream", fmt.Errorf("源站返回 403: %w", ErrUpstream), http.StatusBadGateway},
		{"裸 ErrUnavailable", ErrUnavailable, http.StatusServiceUnavailable},
		{"包装 ErrUnavailable", fmt.Errorf("分片索引未完成: %w", ErrUnavailable), http.StatusServiceUnavailable},
		{"无关错误", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := StatusCode(c.err); got != c.want {
			t.Errorf("%s: StatusCode = %d, 期望 %d", c.name, got, c.want)
		}
	}
}

// TestWriteError 覆盖 AC2：每类写出正确 status、JSON Content-Type、合法 body，
// code ∈ not_found|invalid|upstream|unavailable|internal，message == err.Error()。
func TestWriteError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantKind string
	}{
		{"ErrNotFound", ErrNotFound, http.StatusNotFound, "not_found"},
		{"包装 ErrInvalid", fmt.Errorf("第 %s 集不存在: %w", "3", ErrInvalid), http.StatusBadRequest, "invalid"},
		{"ErrUpstream", ErrUpstream, http.StatusBadGateway, "upstream"},
		{"ErrUnavailable", ErrUnavailable, http.StatusServiceUnavailable, "unavailable"},
		{"无关错误", errors.New("内部错误"), http.StatusInternalServerError, "internal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteError(rec, c.err)

			if rec.Code != c.wantCode {
				t.Errorf("状态码 = %d, 期望 %d", rec.Code, c.wantCode)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q, 期望 %q", got, "application/json; charset=utf-8")
			}
			var body struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, rec.Body.String())
			}
			if body.Code != c.wantKind {
				t.Errorf("code = %q, 期望 %q", body.Code, c.wantKind)
			}
			if body.Message != c.err.Error() {
				t.Errorf("message = %q, 期望 %q", body.Message, c.err.Error())
			}
		})
	}
}

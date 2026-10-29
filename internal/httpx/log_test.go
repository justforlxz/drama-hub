package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureLogger 返回级别为 Debug 的 JSON logger 与其输出缓冲：Debug 避免级别
// 过滤影响日志条数，JSON 让测试走真实 slog 序列化路径。
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// serveLogged 经 RequestLogger 发一次请求，返回响应记录器。
func serveLogged(t *testing.T, logger *slog.Logger, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	RequestLogger(logger, h).ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// loggedRecords 解析缓冲里的全部日志行（每行一条 JSON 记录）。
func loggedRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	out := buf.String()
	if strings.TrimSpace(out) == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("日志行不是合法 JSON: %v（line=%q）", err, line)
		}
		records = append(records, rec)
	}
	return records
}

// TestRequestLogger_OneRecordWithFields 覆盖 AC3：一次请求恰一条日志，含
// method / path / status / duration_ms 四字段且值正确。
func TestRequestLogger_OneRecordWithFields(t *testing.T) {
	logger, buf := captureLogger()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	serveLogged(t, logger, handler, http.MethodGet, "/healthz")

	records := loggedRecords(t, buf)
	if len(records) != 1 {
		t.Fatalf("日志条数 = %d, 期望 1（buf=%q）", len(records), buf.String())
	}
	rec := records[0]
	if rec["method"] != "GET" {
		t.Errorf("method = %v, 期望 GET", rec["method"])
	}
	if rec["path"] != "/healthz" {
		t.Errorf("path = %v, 期望 /healthz", rec["path"])
	}
	if status, ok := rec["status"].(float64); !ok || int(status) != http.StatusOK {
		t.Errorf("status = %v, 期望 %d", rec["status"], http.StatusOK)
	}
	if ms, ok := rec["duration_ms"].(float64); !ok || ms < 0 {
		t.Errorf("duration_ms = %v, 期望 ≥ 0 的数字", rec["duration_ms"])
	}
}

// TestRequestLogger_StatusCapture 覆盖 AC4：显式 WriteHeader(501) → 501；
// 未写 WriteHeader → 200。
func TestRequestLogger_StatusCapture(t *testing.T) {
	cases := []struct {
		name string
		h    http.Handler
		want int
	}{
		{"显式写 501", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotImplemented)
		}), http.StatusNotImplemented},
		{"未写 WriteHeader", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logger, buf := captureLogger()
			serveLogged(t, logger, c.h, http.MethodGet, "/x")

			records := loggedRecords(t, buf)
			if len(records) != 1 {
				t.Fatalf("日志条数 = %d, 期望 1", len(records))
			}
			if status, ok := records[0]["status"].(float64); !ok || int(status) != c.want {
				t.Errorf("status = %v, 期望 %d", records[0]["status"], c.want)
			}
		})
	}
}

// TestRequestLogger_MasksStreamToken 覆盖 AC5：/s/{token}/… 的 token 段掩码，
// 日志全文不含明文 token。
func TestRequestLogger_MasksStreamToken(t *testing.T) {
	logger, buf := captureLogger()
	serveLogged(t, logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		http.MethodGet, "/s/SUPERSECRET/e/42")

	if out := buf.String(); strings.Contains(out, "SUPERSECRET") {
		t.Errorf("日志泄露 token 明文: %s", out)
	}
	records := loggedRecords(t, buf)
	if len(records) != 1 {
		t.Fatalf("日志条数 = %d, 期望 1", len(records))
	}
	if records[0]["path"] != "/s/{token}/e/42" {
		t.Errorf("path = %v, 期望 /s/{token}/e/42", records[0]["path"])
	}
}

// TestRequestLogger_DropsQuery 覆盖 AC6：query 不进日志。
func TestRequestLogger_DropsQuery(t *testing.T) {
	logger, buf := captureLogger()
	serveLogged(t, logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		http.MethodGet, "/api/x?token=SECRET")

	if out := buf.String(); strings.Contains(out, "SECRET") {
		t.Errorf("日志泄露 query 内容: %s", out)
	}
	records := loggedRecords(t, buf)
	if len(records) != 1 {
		t.Fatalf("日志条数 = %d, 期望 1", len(records))
	}
	if records[0]["path"] != "/api/x" {
		t.Errorf("path = %v, 期望 /api/x", records[0]["path"])
	}
}

// TestRedactPath 表驱动覆盖掩码规则（AC5）：仅 /s/ 的首段被替换。
func TestRedactPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/s/SECRET/e/42", "/s/{token}/e/42"},
		{"/s/SECRET/seg/7/12.ts", "/s/{token}/seg/7/12.ts"},
		{"/s/SECRET", "/s/{token}"},
		{"/s/", "/s/"},
		{"/healthz", "/healthz"},
		{"/dav/x", "/dav/x"},
		{"/", "/"},
	}
	for _, c := range cases {
		if got := redactPath(c.in); got != c.want {
			t.Errorf("redactPath(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

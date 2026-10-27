package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestTestdataRelativePathWorks 校验测试运行器把工作目录设回了包目录。
//
// 容器内的 runner 需要自行模拟 `go test` 的 cwd 行为；它一旦坏了，
// testdata 的相对路径会莫名其妙读不到，所以这里用一条最小用例把责任钉在 runner 上。
func TestTestdataRelativePathWorks(t *testing.T) {
	got, err := os.ReadFile(filepath.Join("testdata", "marker.txt"))
	if err != nil {
		t.Fatalf("相对路径读 testdata 失败（测试运行器未把 cwd 设回包目录）：%v", err)
	}
	if string(got) != "marker\n" {
		t.Errorf("marker 内容 = %q, 期望 %q", string(got), "marker\n")
	}
}

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

package web

import (
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

// TestServer_UnimplementedSubtree 锁定占位语义：管理 API 整棵子树（含更深子路径）
// 一律 501。healthz 属装配面已迁到 cmd，本包不重复覆盖。
func TestServer_UnimplementedSubtree(t *testing.T) {
	srv := NewServer(slog.New(slog.DiscardHandler))

	for _, path := range []string{"/api/", "/api/x", "/api/x/y"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("GET %s 状态码 = %d, 期望 %d", path, rec.Code, http.StatusNotImplemented)
		}
	}
}

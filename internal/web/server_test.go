package web

import (
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

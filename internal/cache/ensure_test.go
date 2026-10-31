package cache

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

// EnsureLayout 在启动时建出四个目录；重复调用幂等，已有的缓存内容不受影响。
func TestEnsureLayout(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()

	if err := tc.c.EnsureLayout(ctx); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	// findTree 给的是相对 root 的路径，目录带尾 `/`。
	want := []string{dirCover + "/", dirIndex + "/", dirSegment + "/", dirTmp + "/"}
	got := findTree(t, tc.root)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("布局 = %v, 期望 %v", got, want)
	}

	// 幂等：再建一次不多目录、不报错。
	if err := tc.c.EnsureLayout(ctx); err != nil {
		t.Fatalf("第二次 EnsureLayout: %v", err)
	}
	if got := findTree(t, tc.root); len(got) != len(want) {
		t.Fatalf("第二次调用后布局 = %v", got)
	}
}

// cache_dir 指向无法创建的路径时返回错误（装配根只记 warn，不退出）。
func TestEnsureLayoutUnwritable(t *testing.T) {
	tc := newTestCache(t)
	// 父路径是个普通文件：MkdirAll 必然失败（不依赖运行用户的权限位）。
	blocker := filepath.Join(tc.root, "blocked")
	if err := writeFileAtomic(tc.root, blocker, []byte("x")); err != nil {
		t.Fatalf("造阻塞文件: %v", err)
	}
	if err := tc.settings.Set(context.Background(), "cache_dir", filepath.Join(blocker, "cache")); err != nil {
		t.Fatalf("设置 cache_dir: %v", err)
	}
	if err := tc.c.EnsureLayout(context.Background()); err == nil {
		t.Fatal("不可写的 cache_dir 未报错")
	}
}

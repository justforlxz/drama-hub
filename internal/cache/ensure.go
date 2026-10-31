package cache

import (
	"context"
	"fmt"
	"os"

	"github.com/justforlxz/drama-hub/internal/store"
)

// EnsureLayout 把 <cache_dir> 的四个目录（cover / index / seg / tmp）建出来。
//
// 缓存本体是「用到才建」，这里只是让布局在启动时就落盘：空卷首次启动后
// `find <cache_dir>` 能看见四个目录，cache_dir 配错（不可写、指到只读卷）在启动
// 日志里就有一条 warn，而不是等到第一次播放才暴露。失败不影响启动：缓存坏了
// 不该挡住只读的 WebDAV 面。
func (c *Cache) EnsureLayout(ctx context.Context) error {
	root, err := cacheRoot(ctx, c.settings)
	if err != nil {
		return err
	}
	l := layoutOf(root)
	dirs := make([]string, 0, 4)
	for _, kind := range []string{store.ArtifactKindCover, store.ArtifactKindIndex, store.ArtifactKindSegment} {
		dir, err := l.dir(kind)
		if err != nil {
			return err
		}
		dirs = append(dirs, dir)
	}
	dirs = append(dirs, l.tmpDir())
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("建缓存目录 %s: %w", dir, err)
		}
	}
	return nil
}

package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/justforlxz/drama-hub/internal/store"
)

// 内容目录名。artifact 的 kind 是 'cover'/'index'/'segment'，文档里的目录名是
// cover / index / seg（docs/CACHE-MODEL.md「落盘布局与键」）。
const (
	dirCover   = "cover"
	dirIndex   = "index"
	dirSegment = "seg"
	// dirTmp 是写入中的临时文件目录。rename 只在同一文件系统内原子，所以临时文件
	// 必须落在 <cache_dir> 下；与三个内容目录分开，进程被 kill 打断时留下的是 tmp
	// 里的垃圾（淘汰时清扫），内容目录里只会出现完整文件。
	dirTmp = "tmp"
)

// cacheRoot 读 cache_dir。每次现读、不缓存（硬约束 5：改完即时生效）。
func cacheRoot(ctx context.Context, settings *store.Settings) (string, error) {
	root, err := settings.String(ctx, store.KeyCacheDir)
	if err != nil {
		return "", err
	}
	return root, nil
}

// layout 是 <cache_dir> 下的固定布局。root 来自 settings，只在一次读写内有效。
type layout struct{ root string }

func layoutOf(root string) layout { return layout{root: root} }

// dir 返回某个 kind 的内容目录：<cache_dir>/{cover,index,seg}。
func (l layout) dir(kind string) (string, error) {
	switch kind {
	case store.ArtifactKindCover:
		return filepath.Join(l.root, dirCover), nil
	case store.ArtifactKindIndex:
		return filepath.Join(l.root, dirIndex), nil
	case store.ArtifactKindSegment:
		return filepath.Join(l.root, dirSegment), nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnsupportedKind, kind)
}

// file 返回内容文件路径：<dir>/<hash 前 2 位>/<hash><ext>。前 2 位做一级分桶，
// 避免单目录十万文件。
func (l layout) file(kind, hash, ext string) (string, error) {
	dir, err := l.dir(kind)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hash[:2], hash+ext), nil
}

// tmpDir 返回写入临时目录：<cache_dir>/tmp。
func (l layout) tmpDir() string { return filepath.Join(l.root, dirTmp) }

// writeFileAtomic 把 body 写成 path：在 tmpDir 里写临时文件，再 rename 到 path。
//
// 文件路径与内容都只由键决定，因此：
//   - 读到的 path 永远是完整内容（rename 是原子的，不存在半截）；
//   - 同名并发写同一份内容无害（后一次 rename 覆盖，内容相同）；
//   - 进程在写入中被 kill，半截文件留在 tmp/ 里、path 根本不会出现（淘汰会清扫 tmp）。
func writeFileAtomic(tmpDir, path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

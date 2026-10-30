package webdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	xwebdav "golang.org/x/net/webdav"

	"github.com/justforlxz/drama-hub/internal/vfs"
)

// Tree 是本包消费的虚拟文件树能力：List 枚举节点、Render 渲染文件字节。
//
// 接口定义在消费方（DEV-STANDARDS「代码组织」），只声明这里用到的两个方法。
// 生产实现是 *vfs.Tree；单测用 fakeTree，不需要 SQLite 与真实渲染。
type Tree interface {
	List(ctx context.Context) ([]vfs.Node, error)
	Render(ctx context.Context, path string) ([]byte, error)
}

// FS 把 Tree 适配为 x/net/webdav.FileSystem。只读：任何写操作都失败。
//
// 它不持有缓存：一次请求树快照挂在请求 ctx 上（见 snapshot），跨请求不共享。
type FS struct {
	tree Tree
}

// NewFS 构造只读 FileSystem。tree 不可为 nil。
func NewFS(tree Tree) *FS { return &FS{tree: tree} }

// errReadOnly 是所有写操作的统一原因：挂载点只读（AGENTS.md 硬约束 1，虚拟不落盘）。
var errReadOnly = errors.New("只读文件系统")

// Mkdir 恒失败：树是派生的，不接受目录创建。
func (f *FS) Mkdir(context.Context, string, fs.FileMode) error { return errReadOnly }

// RemoveAll 恒失败：树是派生的，不接受删除。
func (f *FS) RemoveAll(context.Context, string) error { return errReadOnly }

// Rename 恒失败：树是派生的，不接受重命名。
func (f *FS) Rename(context.Context, string, string) error { return errReadOnly }

// Stat 返回 WebDAV 路径对应资源的元信息。
//
// 文件的大小只有一条来源：渲染一次、取 len(bytes)（硬约束 3）。不存在的路径返回
// 包装了 fs.ErrNotExist 的错误，x/net/webdav 据此映射 404。
func (f *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	nodes, err := f.nodes(ctx)
	if err != nil {
		return nil, err
	}
	node, ok := f.nodeAt(nodes, name)
	if !ok {
		return nil, notExist(name)
	}
	return f.stat(ctx, node)
}

// OpenFile 打开一个目录（供 Readdir）或一个文件（供 Read/Seek）。
//
// 带任何写意图的 flag 直接失败：只读挂载点不给 x/net/webdav 的 PUT/MKCOL 落到
// 数据面。读打开与 Stat 共用同一份渲染结果（请求快照）。
func (f *FS) OpenFile(ctx context.Context, name string, flag int, _ fs.FileMode) (xwebdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, errReadOnly
	}
	nodes, err := f.nodes(ctx)
	if err != nil {
		return nil, err
	}
	node, ok := f.nodeAt(nodes, name)
	if !ok {
		return nil, notExist(name)
	}
	info, err := f.stat(ctx, node)
	if err != nil {
		return nil, err
	}
	if !node.IsDir {
		body, err := f.render(ctx, node.Path)
		if err != nil {
			return nil, err
		}
		return &file{entry: info, body: body}, nil
	}
	children, err := f.children(ctx, nodes, node.Path)
	if err != nil {
		return nil, err
	}
	return &file{entry: info, children: children}, nil
}

// nodeAt 在树快照里查 WebDAV 路径对应的节点。
//
// DAV 根就是 vfs 的根目录：DESIGN §5.1 / README 的库路径是 `/短剧`，挂载点根直接
// 映射到它（`rclone lsf --max-depth 3 dramahub:/` 必须直接给出剧目录，见
// docs/ACCEPTANCE.md P2，多套一层 `短剧/` 会让 Emby 库路径多一级）。所以 DAV
// 路径 `/` → `短剧`，`/剧名 (2024)/…` → `短剧/剧名 (2024)/…`，根目录名从
// vfs.List 现取（唯一不带 `/` 的目录节点），不在本包复制字面量。
func (f *FS) nodeAt(nodes []vfs.Node, name string) (vfs.Node, bool) {
	p, ok := resolve(nodes, name)
	if !ok {
		return vfs.Node{}, false
	}
	for _, n := range nodes {
		if n.Path == p {
			return n, true
		}
	}
	return vfs.Node{}, false
}

// resolve 把 DAV 路径映射为 vfs 相对路径：DAV 根 = vfs 根目录。
func resolve(nodes []vfs.Node, name string) (string, bool) {
	root, ok := rootDir(nodes)
	if !ok {
		return "", false
	}
	rel := strings.Trim(name, "/")
	if rel == "" {
		return root, true
	}
	return root + "/" + rel, true
}

// rootDir 返回 vfs 的根目录：List 里唯一不带 `/` 的目录节点。
func rootDir(nodes []vfs.Node) (string, bool) {
	for _, n := range nodes {
		if n.IsDir && !strings.Contains(n.Path, "/") {
			return n.Path, true
		}
	}
	return "", false
}

// stat 取节点的 FileInfo；文件会渲染一次拿长度，目录长度为 0。
func (f *FS) stat(ctx context.Context, node vfs.Node) (entry, error) {
	if node.IsDir {
		return entry{node: node}, nil
	}
	body, err := f.render(ctx, node.Path)
	if err != nil {
		return entry{}, err
	}
	return entry{node: node, size: int64(len(body))}, nil
}

// children 取目录的直接子项，顺序与 vfs.List 一致。
func (f *FS) children(ctx context.Context, nodes []vfs.Node, dir string) ([]os.FileInfo, error) {
	out := make([]os.FileInfo, 0, 4)
	for _, n := range nodes {
		if parentOf(n.Path) != dir {
			continue
		}
		info, err := f.stat(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// nodes 取树节点：同一请求内复用快照，无快照（单测直连 FS）时现算。
func (f *FS) nodes(ctx context.Context) ([]vfs.Node, error) {
	if s := snapshotFrom(ctx); s != nil {
		return s.nodes(ctx, f.tree)
	}
	return f.tree.List(ctx)
}

// render 取文件字节；同一请求内同一路径只渲染一次（长度与内容必须来自同一次渲染）。
func (f *FS) render(ctx context.Context, p string) ([]byte, error) {
	if s := snapshotFrom(ctx); s != nil {
		return s.render(ctx, f.tree, p)
	}
	return f.tree.Render(ctx, p)
}

// parentOf 返回 vfs 路径的父目录；顶层节点（`短剧`）的父目录是空串，也就是 DAV 根。
func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// notExist 包装 fs.ErrNotExist，让 x/net/webdav 的错误映射落到 404。
func notExist(name string) error {
	return fmt.Errorf("%s: %w", name, fs.ErrNotExist)
}

// entry 是 os.FileInfo 实现：一个目录或一个已渲染的文件。
//
// ModTime 是零值：库里没有可靠的修改时间（`sync_at` / `play_url_at` 是采集时间），
// 而零值正是「未知」——http.ServeContent 对零值不写 Last-Modified，比伪造一个时间
// 诚实。长度只有 size 一个字段，来自 len(渲染字节)。
type entry struct {
	node vfs.Node
	size int64
}

// Name 是路径最后一段（WebDAV 的 displayname）。
func (e entry) Name() string {
	if i := strings.LastIndexByte(e.node.Path, '/'); i >= 0 {
		return e.node.Path[i+1:]
	}
	return e.node.Path
}

// Size 是文件字节数；目录为 0。
func (e entry) Size() int64 { return e.size }

// Mode 只读：目录 0555、文件 0444。
func (e entry) Mode() fs.FileMode {
	if e.node.IsDir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

// ModTime 恒为零值（见 entry 注释）。
func (e entry) ModTime() time.Time { return time.Time{} }

// IsDir 报告是否为目录。
func (e entry) IsDir() bool { return e.node.IsDir }

// Sys 无底层数据。
func (e entry) Sys() any { return nil }

// file 是 x/net/webdav.File：目录给出子项，文件给出已渲染的字节。
//
// 只读：Write 恒失败（写路径在 OpenFile 就被挡掉，这里只是接口要求的兜底）。
type file struct {
	entry
	body     []byte
	off      int64
	children []os.FileInfo
	pos      int
}

// Stat 实现 http.File。
func (f *file) Stat() (os.FileInfo, error) { return f.entry, nil }

// Close 无资源可释放（内容在内存里）。
func (f *file) Close() error { return nil }

// Read 从渲染字节里读；越界即 io.EOF。
func (f *file) Read(p []byte) (int, error) {
	if f.off >= int64(len(f.body)) {
		return 0, io.EOF
	}
	n := copy(p, f.body[f.off:])
	f.off += int64(n)
	return n, nil
}

// Seek 支持 http.ServeContent 需要的三个 whence（它靠 SeekEnd 定长度）。
func (f *file) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.off + offset
	case io.SeekEnd:
		abs = int64(len(f.body)) + offset
	default:
		return 0, fmt.Errorf("非法的 whence: %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("负偏移: %d", abs)
	}
	f.off = abs
	return abs, nil
}

// Readdir 列出目录子项；n<=0 返回剩余全部，n>0 分页并在耗尽时返回 io.EOF。
func (f *file) Readdir(n int) ([]os.FileInfo, error) {
	if !f.entry.IsDir() {
		return nil, fmt.Errorf("%s 不是目录", f.entry.Name())
	}
	if n <= 0 {
		out := f.children[f.pos:]
		f.pos = len(f.children)
		return out, nil
	}
	if f.pos >= len(f.children) {
		return nil, io.EOF
	}
	end := min(f.pos+n, len(f.children))
	out := f.children[f.pos:end]
	f.pos = end
	return out, nil
}

// Write 恒失败：只读文件系统。
func (f *file) Write([]byte) (int, error) { return 0, errReadOnly }

// rendered 是一次渲染的结果（含失败），用于请求内复用。
type rendered struct {
	body []byte
	err  error
}

// snapshot 是一次请求内的树快照。
//
// x/net/webdav 处理一个 PROPFIND 会为同一份数据反复调用 Stat / OpenFile（父目录
// 一次、每个子项再各一次），逐次 List 会让一次目录列举变成 O(目录项数) 次全树
// 查询（每次 1+N 条 SQL）。快照把 List 与渲染结果挂到请求 ctx 上，一次请求各算一遍。
//
// 一个请求内所有调用都在同一 goroutine 且串行，因此不加锁。
type snapshot struct {
	once    sync.Once
	list    []vfs.Node
	listErr error
	bodies  map[string]rendered
}

// nodes 返回树节点，首次调用现算。
func (s *snapshot) nodes(ctx context.Context, t Tree) ([]vfs.Node, error) {
	s.once.Do(func() { s.list, s.listErr = t.List(ctx) })
	return s.list, s.listErr
}

// render 返回某一路径的字节，同路径只渲染一次（长度与内容必须同源）。
func (s *snapshot) render(ctx context.Context, t Tree, p string) ([]byte, error) {
	if r, ok := s.bodies[p]; ok {
		return r.body, r.err
	}
	body, err := t.Render(ctx, p)
	s.bodies[p] = rendered{body: body, err: err}
	return body, err
}

// snapshotKey 是请求 ctx 里树快照的键（本包私有，外部无法伪造）。
type snapshotKey struct{}

// withSnapshot 把一份新的树快照挂到 ctx 上，只在 Handler.ServeHTTP 里调用一次。
func withSnapshot(ctx context.Context) context.Context {
	return context.WithValue(ctx, snapshotKey{}, &snapshot{bodies: map[string]rendered{}})
}

// snapshotFrom 取请求 ctx 上的树快照；没有则返回 nil（调用方现算）。
func snapshotFrom(ctx context.Context) *snapshot {
	s, _ := ctx.Value(snapshotKey{}).(*snapshot)
	return s
}

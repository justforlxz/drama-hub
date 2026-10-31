package webdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	xwebdav "golang.org/x/net/webdav"

	"github.com/justforlxz/drama-hub/internal/vfs"
)

// Tree 是本包消费的虚拟文件树能力：List 列出一个目录的直接子项、Render 渲染文件字节。
//
// 接口定义在消费方（DEV-STANDARDS「代码组织」），只声明这里用到的两个方法。
// 生产实现是 *vfs.Tree；单测用 fakeTree，不需要 SQLite 与真实渲染。
type Tree interface {
	List(ctx context.Context, dir string) ([]vfs.Node, error)
	Render(ctx context.Context, path string) ([]byte, error)
}

// FS 把 Tree 适配为 x/net/webdav.FileSystem。只读：任何写操作都失败。
//
// 它不持有缓存：一次请求内的目录列举与渲染结果挂在请求 ctx 上（见 snapshot），
// 跨请求不共享。每个目录在一次请求里只查一次库（docs/DIRECTORY-CONTRACT.md
// 「MUST：目录列举必须是「一个目录一次查询」」）。
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
	node, err := f.nodeAt(ctx, name)
	if err != nil {
		return nil, err
	}
	return f.stat(ctx, node)
}

// OpenFile 打开一个目录（供 Readdir）或一个文件（供 Read/Seek）。
//
// 带任何写意图的 flag 直接失败：只读挂载点不给 x/net/webdav 的 PUT/MKCOL 落到
// 数据面。读打开与 Stat 共用同一份渲染结果（请求内快照）。
func (f *FS) OpenFile(ctx context.Context, name string, flag int, _ fs.FileMode) (xwebdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, errReadOnly
	}
	node, err := f.nodeAt(ctx, name)
	if err != nil {
		return nil, err
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
	// 目录的子项**不在这里查**：x/net/webdav 的 PROPFIND 会对每个子项调一次
	// OpenFile（prop.go 的 propnames），立即列子项等于把「列举一层」变成
	// 「列举 N 层 + 渲染孙级文件」。真正的列子项推迟到 Readdir。
	return &file{entry: info, fsys: f, ctx: ctx, dir: node}, nil
}

// nodeAt 找到 DAV 路径对应的节点：列一次父目录，按最后一段匹配。
//
// DAV 根（`/`）就是 vfs 的根目录 `短剧`（协议里没有这一段），其余路径逐段对应
// `短剧` 之下的目录。一次 Stat 因此只查「父目录一层」，不枚举全树。
func (f *FS) nodeAt(ctx context.Context, name string) (vfs.Node, error) {
	rel := strings.Trim(name, "/")
	if rel == "" {
		return vfs.Node{Path: vfs.Root, IsDir: true}, nil
	}
	parent, base := "", rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		parent, base = rel[:i], rel[i+1:]
	}
	l, err := f.listing(ctx, vfsDir(parent))
	if err != nil {
		return vfs.Node{}, err
	}
	if n, ok := l.byName[base]; ok {
		return n, nil
	}
	return vfs.Node{}, notExist(name)
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

// children 取目录的直接子项（一次 vfs.List），顺序与 vfs.List 一致。
func (f *FS) children(ctx context.Context, dir vfs.Node) ([]os.FileInfo, error) {
	l, err := f.listing(ctx, dirKey(dir.Path))
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, 0, len(l.nodes))
	for _, n := range l.nodes {
		info, err := f.stat(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// listing 列一个 vfs 目录的子项：同一请求内复用快照，无快照（单测直连 FS）时现查。
//
// 返回的 listing 带「末段 → 节点」索引：一次 PROPFIND 会对每个子项按名字回查父目录，
// 线性扫一遍列表会让一层列举变成 O(子项数²)。vfs 的 ErrNotFound 在这里翻译成
// fs.ErrNotExist（协议层据此映射 404，不把「目录不存在」当 500 报出去）。
func (f *FS) listing(ctx context.Context, dir string) (*listing, error) {
	var (
		l   *listing
		err error
	)
	if s := snapshotFrom(ctx); s != nil {
		l, err = s.listing(ctx, f.tree, dir)
	} else {
		l, err = loadListing(ctx, f.tree, dir)
	}
	if errors.Is(err, vfs.ErrNotFound) {
		return nil, notExist(dir)
	}
	return l, err
}

// render 取文件字节；同一请求内同一路径只渲染一次（长度与内容必须来自同一次渲染）。
func (f *FS) render(ctx context.Context, p string) ([]byte, error) {
	if s := snapshotFrom(ctx); s != nil {
		return s.render(ctx, f.tree, p)
	}
	return f.tree.Render(ctx, p)
}

// dirKey 把 vfs 目录路径归一成快照键：根目录的两种写法（`""` 与 `短剧`）是同一个
// 目录，必须共用一个键，否则一次 PROPFIND 会对根目录查两遍。
func dirKey(dir string) string {
	if dir == vfs.Root {
		return ""
	}
	return dir
}

// vfsDir 把「相对挂载点根的目录」换成 vfs 目录路径：DAV 根（`""`）对应 vfs 根，
// 其余补上根目录那一段（vfs.List 只认 `短剧/…` 形态）。
func vfsDir(rel string) string {
	if rel == "" {
		return ""
	}
	return vfs.Root + "/" + rel
}

// baseOf 返回 vfs 路径的最后一段（WebDAV 的 displayname）。
func baseOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// notExist 用 *fs.PathError 包 fs.ErrNotExist，让 x/net/webdav 的错误映射落到 404。
//
// 必须是 *fs.PathError：x/net 的 PROPFIND 用 os.IsNotExist 分流，而它只认
// *fs.PathError（`fmt.Errorf("%w")` 普通包装返回 false），否则不存在的路径会被
// 报成 405 Method Not Allowed。
func notExist(name string) error {
	return &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

// entry 是 os.FileInfo 实现：一个目录或一个已渲染的文件。
//
// ModTime 是零值：库里没有可靠的修改时间（`sync_at` 是采集时间），
// 而零值正是「未知」——http.ServeContent 对零值不写 Last-Modified，比伪造一个时间
// 诚实。长度只有 size 一个字段，来自 len(渲染字节)。
type entry struct {
	node vfs.Node
	size int64
}

// Name 是路径最后一段（WebDAV 的 displayname）。
func (e entry) Name() string { return baseOf(e.node.Path) }

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
	body []byte
	off  int64

	// 目录：子项推迟到 Readdir 才查（见 OpenFile 的注释）。ctx 是打开时的请求 ctx，
	// 请求内快照挂在它上面，所以惰性查询仍然「每目录一次」；file 本身不跨请求存活。
	fsys     *FS
	ctx      context.Context
	dir      vfs.Node
	children []os.FileInfo
	loadErr  error
	loaded   bool
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
//
// 子项在第一次调用时才查（惰性）：x/net/webdav 对每个子项都会 OpenFile 一次，
// 打开即列子项会让一次目录列举把孙级目录与文件全查一遍。
func (f *file) Readdir(n int) ([]os.FileInfo, error) {
	if !f.entry.IsDir() {
		return nil, fmt.Errorf("%s 不是目录", f.entry.Name())
	}
	if err := f.load(); err != nil {
		return nil, err
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

// load 首次调用时取目录子项；失败结果也缓存，避免重复查库。
func (f *file) load() error {
	if f.loaded {
		return f.loadErr
	}
	f.loaded = true
	f.children, f.loadErr = f.fsys.children(f.ctx, f.dir)
	return f.loadErr
}

// Write 恒失败：只读文件系统。
func (f *file) Write([]byte) (int, error) { return 0, errReadOnly }

// rendered 是一次渲染的结果（含失败），用于请求内复用。
type rendered struct {
	body []byte
	err  error
}

// listing 是一次目录列举的结果（含失败），用于请求内复用。
//
// byName 是「末段 → 节点」索引：PROPFIND 的每个子项都要按名字回查父目录，
// 没有索引就是 O(子项数²) 次字符串比较。
type listing struct {
	nodes  []vfs.Node
	byName map[string]vfs.Node
	err    error
}

// loadListing 现查一个目录，一次遍历同时建好 byName 索引。
func loadListing(ctx context.Context, t Tree, dir string) (*listing, error) {
	nodes, err := t.List(ctx, dir)
	l := &listing{nodes: nodes, byName: make(map[string]vfs.Node, len(nodes)), err: err}
	for _, n := range nodes {
		l.byName[baseOf(n.Path)] = n
	}
	return l, err
}

// snapshot 是一次请求内的树快照，键是「列过的目录」。
//
// x/net/webdav 处理一个 PROPFIND 会为同一份数据反复调用 Stat / OpenFile（父目录
// 一次、每个子项再各一次），每次都现查会让一次目录列举变成 O(目录项数) 次 SQL。
// 快照把「每个目录一次的 List」与渲染结果挂到请求 ctx 上，一次请求每层各算一遍。
//
// 一个请求内所有调用都在同一 goroutine 且串行，因此不加锁。
type snapshot struct {
	dirs   map[string]*listing
	bodies map[string]rendered
}

// listing 返回某个 vfs 目录的子项，同一目录只查一次库。
func (s *snapshot) listing(ctx context.Context, t Tree, dir string) (*listing, error) {
	if l, ok := s.dirs[dir]; ok {
		return l, l.err
	}
	l, err := loadListing(ctx, t, dir)
	s.dirs[dir] = l
	return l, err
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
	return context.WithValue(ctx, snapshotKey{},
		&snapshot{dirs: map[string]*listing{}, bodies: map[string]rendered{}})
}

// snapshotFrom 取请求 ctx 上的树快照；没有则返回 nil（调用方现查）。
func snapshotFrom(ctx context.Context) *snapshot {
	s, _ := ctx.Value(snapshotKey{}).(*snapshot)
	return s
}

package webdav

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/vfs"
)

// 本文件是 FS 的单元测试：用 fakeTree 提供树与渲染字节，**不碰 SQLite、不起 HTTP**。
// 集成测试（webdav_test.go）验证协议契约，这里验证适配逻辑本身——路径映射、
// 长度来源、目录成员、只读语义、请求内快照。全部带超时，不允许挂住。

// fakeTree 是 Tree 的替身：节点与渲染字节都是静态表，渲染失败可注入。
type fakeTree struct {
	nodes  []vfs.Node
	bodies map[string][]byte
	// renderErr 命中时让对应路径的 Render 失败（键为 vfs 路径）。
	renderErr map[string]error
	// listErr 非 nil 时 List 失败。
	listErr error

	renders int // Render 调用次数，用于证明「同请求同路径只渲染一次」
	lists   int // List 调用次数，用于证明「同请求只枚举一次」
}

func (f *fakeTree) List(context.Context) ([]vfs.Node, error) {
	f.lists++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.nodes, nil
}

func (f *fakeTree) Render(_ context.Context, path string) ([]byte, error) {
	f.renders++
	if err := f.renderErr[path]; err != nil {
		return nil, err
	}
	body, ok := f.bodies[path]
	if !ok {
		return nil, vfs.ErrNotFound
	}
	return body, nil
}

// newFakeTree 造一棵最小树：根 `短剧`、一部剧、一季、一集的两个文件。
func newFakeTree() *fakeTree {
	const (
		root     = "短剧"
		drama    = root + "/测试剧 (2024)"
		season   = drama + "/Season 01"
		strmPath = season + "/测试剧 - S01E01 - 开局.strm"
		nfoPath  = season + "/测试剧 - S01E01 - 开局.nfo"
	)
	return &fakeTree{
		nodes: []vfs.Node{
			{Path: root, IsDir: true},
			{Path: drama, IsDir: true},
			{Path: drama + "/tvshow.nfo"},
			{Path: season, IsDir: true},
			{Path: strmPath},
			{Path: nfoPath},
		},
		bodies: map[string][]byte{
			drama + "/tvshow.nfo": []byte("<tvshow/>\n"),
			strmPath:              []byte("http://example/s/tok/e/1\n"),
			nfoPath:               []byte("<episodedetails/>\n"),
		},
		renderErr: map[string]error{},
	}
}

// testCtx 返回带超时的 ctx：单元测试也不允许无限等待。
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	t.Cleanup(cancel)
	return ctx
}

// 根路径映射到 vfs 根目录（`短剧`），且只有一条路径能打开。
func TestFS_MapsDAVRootToTreeRoot(t *testing.T) {
	ft := newFakeTree()
	info, err := NewFS(ft).Stat(testCtx(t), "/")
	if err != nil {
		t.Fatalf("Stat(/): %v", err)
	}
	if !info.IsDir() {
		t.Errorf("Stat(/) 应是目录")
	}
	if got, want := info.Name(), "短剧"; got != want && got != "" {
		t.Errorf("根 Name() = %q, 期望 %q（findDisplayName 对根隐藏名字）", got, want)
	}
}

// 文件长度唯一来源是渲染字节数：改 fake 的字节，Stat 的长度随之变。
func TestFS_FileSizeComesFromRender(t *testing.T) {
	ft := newFakeTree()
	const path = "短剧/测试剧 (2024)/Season 01/测试剧 - S01E01 - 开局.strm"
	fsys := NewFS(ft)

	info, err := fsys.Stat(testCtx(t), "/"+strings.TrimPrefix(path, "短剧/"))
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if got, want := info.Size(), int64(len(ft.bodies[path])); got != want {
		t.Errorf("Size() = %d, 期望 %d（= len(渲染字节)）", got, want)
	}
	if info.IsDir() {
		t.Errorf("%s 应是文件", path)
	}

	// 换一份更长的字节：长度必须跟着变，证明没有第二条长度来源。
	longer := []byte(strings.Repeat("x", 100) + "\n")
	ft.bodies[path] = longer
	info, err = fsys.Stat(testCtx(t), "/"+strings.TrimPrefix(path, "短剧/"))
	if err != nil {
		t.Fatalf("Stat(%s) 第二次: %v", path, err)
	}
	if got, want := info.Size(), int64(len(longer)); got != want {
		t.Errorf("Size() = %d, 期望 %d", got, want)
	}
}

// 渲染失败必须原样上报，不能被当成「不存在」或「长度 0」。
func TestFS_RenderErrorPropagates(t *testing.T) {
	ft := newFakeTree()
	const path = "短剧/测试剧 (2024)/Season 01/测试剧 - S01E01 - 开局.strm"
	sentinel := errors.New("渲染炸了")
	ft.renderErr[path] = sentinel

	_, err := NewFS(ft).Stat(testCtx(t), "/"+strings.TrimPrefix(path, "短剧/"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("Stat 错误 = %v, 期望 %v", err, sentinel)
	}
}

// 未知路径 → 包装 fs.ErrNotExist（x/net/webdav 据此映射 404）。
func TestFS_NotFoundWrapsFSNotExist(t *testing.T) {
	ft := newFakeTree()
	for _, name := range []string{"/不存在", "/测试剧 (2024)/nope.strm", "/测试剧 (2024)/Season 01/nope.nfo"} {
		_, err := NewFS(ft).Stat(testCtx(t), name)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Stat(%s) 错误 = %v, 期望 errors.Is(err, fs.ErrNotExist)", name, err)
		}
	}
}

// 目录的 Readdir 给出直接子项（名称与 IsDir），不夹带别的层。
func TestFS_ReaddirDirectChildren(t *testing.T) {
	ft := newFakeTree()
	fsys := NewFS(ft)
	ctx := testCtx(t)

	f, err := fsys.OpenFile(ctx, "/测试剧 (2024)", 0, 0)
	if err != nil {
		t.Fatalf("OpenFile(剧目录): %v", err)
	}
	children, err := f.Readdir(0)
	if err != nil {
		t.Fatalf("Readdir: %v", err)
	}
	got := map[string]bool{}
	for _, c := range children {
		got[c.Name()] = c.IsDir()
	}
	want := map[string]bool{"tvshow.nfo": false, "Season 01": true}
	if len(got) != len(want) {
		t.Fatalf("子项 = %v, 期望 %v", got, want)
	}
	for name, isDir := range want {
		if got[name] != isDir {
			t.Errorf("子项 %s IsDir = %v, 期望 %v（实际 %v）", name, got[name], isDir, got)
		}
	}
}

// 只读：三个写操作与写打开全部失败。
func TestFS_ReadOnly(t *testing.T) {
	ft := newFakeTree()
	fsys := NewFS(ft)
	ctx := testCtx(t)

	if err := fsys.Mkdir(ctx, "/新目录", 0o755); !errors.Is(err, errReadOnly) {
		t.Errorf("Mkdir 错误 = %v, 期望 errReadOnly", err)
	}
	if err := fsys.RemoveAll(ctx, "/"); !errors.Is(err, errReadOnly) {
		t.Errorf("RemoveAll 错误 = %v, 期望 errReadOnly", err)
	}
	if err := fsys.Rename(ctx, "/a", "/b"); !errors.Is(err, errReadOnly) {
		t.Errorf("Rename 错误 = %v, 期望 errReadOnly", err)
	}
	if _, err := fsys.OpenFile(ctx, "/", 0x2 /* O_RDWR */, 0); !errors.Is(err, errReadOnly) {
		t.Errorf("写打开错误 = %v, 期望 errReadOnly", err)
	}
	if ft.renders != 0 {
		t.Errorf("只读操作不应触发渲染，实际 %d 次", ft.renders)
	}
}

// 请求内快照：同一请求里 Stat 两次同一文件，只 List 一次、只渲染一次。
func TestFS_SnapshotRendersOncePerRequest(t *testing.T) {
	ft := newFakeTree()
	fsys := NewFS(ft)
	ctx := withSnapshot(testCtx(t))

	for i := 0; i < 3; i++ {
		if _, err := fsys.Stat(ctx, "/测试剧 (2024)/Season 01/测试剧 - S01E01 - 开局.strm"); err != nil {
			t.Fatalf("第 %d 次 Stat: %v", i+1, err)
		}
	}
	if ft.lists != 1 {
		t.Errorf("List 调用 %d 次, 期望 1（请求内快照）", ft.lists)
	}
	if ft.renders != 1 {
		t.Errorf("Render 调用 %d 次, 期望 1（同请求同路径复用）", ft.renders)
	}

	// 新请求（新快照）必须重新枚举：没有跨请求缓存（硬约束 5 即时生效）。
	ctx2 := withSnapshot(testCtx(t))
	if _, err := fsys.Stat(ctx2, "/"); err != nil {
		t.Fatalf("新请求 Stat: %v", err)
	}
	if ft.lists != 2 {
		t.Errorf("新请求后 List 调用 %d 次, 期望 2（不跨请求复用）", ft.lists)
	}
}

// List 失败必须上报（不能把整棵树伪装成只有根）。
func TestFS_ListErrorPropagates(t *testing.T) {
	sentinel := errors.New("查库失败")
	ft := newFakeTree()
	ft.listErr = sentinel

	_, err := NewFS(ft).Stat(testCtx(t), "/")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Stat 错误 = %v, 期望 %v", err, sentinel)
	}
}

// 请求内快照不得跨 goroutine 泄漏：两次独立快照互不影响（-race 下验证）。
func TestFS_SnapshotIsolated(t *testing.T) {
	ft := newFakeTree()
	fsys := NewFS(ft)
	a, b := withSnapshot(testCtx(t)), withSnapshot(testCtx(t))

	if _, err := fsys.Stat(a, "/"); err != nil {
		t.Fatalf("请求 A Stat: %v", err)
	}
	if _, err := fsys.Stat(b, "/"); err != nil {
		t.Fatalf("请求 B Stat: %v", err)
	}
	if ft.lists != 2 {
		t.Errorf("两个请求各应枚举一次，实际 %d 次", ft.lists)
	}
}

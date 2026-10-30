package webdav

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/vfs"
)

// 本文件是 FS 的单元测试：用 fakeTree 提供「目录 → 直接子项」与渲染字节，
// **不碰 SQLite、不起 HTTP**。集成测试（webdav_test.go）验证协议契约，
// 这里验证适配逻辑本身——路径映射、长度来源、目录成员、只读语义、请求内快照。
// 全部带超时，不允许挂住。

// fakeTree 是 Tree 的替身：按目录返回直接子项，回答「一次请求只查一层」。
type fakeTree struct {
	dirs   map[string][]vfs.Node
	bodies map[string][]byte
	// renderErr 命中时让对应路径的 Render 失败（键为 vfs 路径）。
	renderErr map[string]error
	// listErr 命中时让对应目录的 List 失败（键为 vfs 目录）。
	listErr map[string]error

	renders int      // Render 调用次数，用于证明「同请求同路径只渲染一次」
	lists   int      // List 调用次数，用于证明「每个目录每请求只查一次」
	listed  []string // List 收到过的目录（按调用顺序），用于证明「只查一层」
}

// List 只返回 dir 的直接子项；未知目录与 vfs.Tree 一致返回 ErrNotFound。
func (f *fakeTree) List(_ context.Context, dir string) ([]vfs.Node, error) {
	f.lists++
	f.listed = append(f.listed, dir)
	if err := f.listErr[dir]; err != nil {
		return nil, err
	}
	nodes, ok := f.dirs[dir]
	if !ok {
		return nil, vfs.ErrNotFound
	}
	return nodes, nil
}

// Render 返回静态字节；未登记路径与 vfs.Tree 一致返回 ErrNotFound。
func (f *fakeTree) Render(_ context.Context, p string) ([]byte, error) {
	f.renders++
	if err := f.renderErr[p]; err != nil {
		return nil, err
	}
	body, ok := f.bodies[p]
	if !ok {
		return nil, vfs.ErrNotFound
	}
	return body, nil
}

// 假树的固定形状：短剧/<分类>/<分片>/<剧>/Season 01/<文件>。
const (
	fakeRoot     = "短剧"
	fakeGenreDir = fakeRoot + "/爱情"
	fakeShardDir = fakeGenreDir + "/0a"
	fakeDramaDir = fakeShardDir + "/测试剧 (2024)"
	fakeSeason   = fakeDramaDir + "/Season 01"
	fakeStrm     = fakeSeason + "/测试剧 - S01E01 - 开局.strm"
	fakeNFO      = fakeSeason + "/测试剧 - S01E01 - 开局.nfo"
)

// newFakeTree 造一棵最小树：根 → 分类 → 分片 → 一部剧 → 一季 → 一集的两个文件。
func newFakeTree() *fakeTree {
	return &fakeTree{
		dirs: map[string][]vfs.Node{
			"":           {{Path: fakeGenreDir, IsDir: true}},
			fakeRoot:     {{Path: fakeGenreDir, IsDir: true}},
			fakeGenreDir: {{Path: fakeShardDir, IsDir: true}},
			fakeShardDir: {{Path: fakeDramaDir, IsDir: true}},
			fakeDramaDir: {
				{Path: fakeDramaDir + "/tvshow.nfo"},
				{Path: fakeSeason, IsDir: true},
			},
			fakeSeason: {
				{Path: fakeStrm},
				{Path: fakeNFO},
			},
		},
		bodies: map[string][]byte{
			fakeDramaDir + "/tvshow.nfo": []byte("<tvshow/>\n"),
			fakeStrm:                     []byte("http://example/video/e1-abcdef012345.m3u8\n"),
			fakeNFO:                      []byte("<episodedetails/>\n"),
		},
		renderErr: map[string]error{},
		listErr:   map[string]error{},
	}
}

// testCtx 返回带超时的 ctx：单元测试也不允许无限等待。
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	t.Cleanup(cancel)
	return ctx
}

// davPath 把 vfs 路径换成 DAV 路径（去掉根那一段）。
func davPath(vfsPath string) string {
	return "/" + strings.TrimPrefix(strings.TrimPrefix(vfsPath, fakeRoot), "/")
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
	if got := info.Name(); got != fakeRoot {
		t.Errorf("根 Name() = %q, 期望 %q", got, fakeRoot)
	}
}

// 文件长度唯一来源是渲染字节数：改 fake 的字节，Stat 的长度随之变。
func TestFS_FileSizeComesFromRender(t *testing.T) {
	ft := newFakeTree()
	fsys := NewFS(ft)

	info, err := fsys.Stat(testCtx(t), davPath(fakeStrm))
	if err != nil {
		t.Fatalf("Stat(%s): %v", davPath(fakeStrm), err)
	}
	if got, want := info.Size(), int64(len(ft.bodies[fakeStrm])); got != want {
		t.Errorf("Size() = %d, 期望 %d（= len(渲染字节)）", got, want)
	}
	if info.IsDir() {
		t.Errorf("%s 应是文件", fakeStrm)
	}

	// 换一份更长的字节：长度必须跟着变，证明没有第二条长度来源。
	longer := []byte(strings.Repeat("x", 100) + "\n")
	ft.bodies[fakeStrm] = longer
	info, err = fsys.Stat(testCtx(t), davPath(fakeStrm))
	if err != nil {
		t.Fatalf("Stat 第二次: %v", err)
	}
	if got, want := info.Size(), int64(len(longer)); got != want {
		t.Errorf("Size() = %d, 期望 %d", got, want)
	}
}

// 渲染失败必须原样上报，不能被当成「不存在」或「长度 0」。
func TestFS_RenderErrorPropagates(t *testing.T) {
	ft := newFakeTree()
	sentinel := errors.New("渲染炸了")
	ft.renderErr[fakeStrm] = sentinel

	_, err := NewFS(ft).Stat(testCtx(t), davPath(fakeStrm))
	if !errors.Is(err, sentinel) {
		t.Fatalf("Stat 错误 = %v, 期望 %v", err, sentinel)
	}
}

// 未知路径 → 包装 fs.ErrNotExist（x/net/webdav 据此映射 404）。
func TestFS_NotFoundWrapsFSNotExist(t *testing.T) {
	ft := newFakeTree()
	fsys := NewFS(ft)
	for _, name := range []string{
		"/不存在",
		"/爱情/7f",            // 分片目录不存在
		"/爱情/0a/不存在 (2024)", // 剧目录不存在
		"/爱情/0a/测试剧 (2024)/nope.strm",
	} {
		_, err := fsys.Stat(testCtx(t), name)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Stat(%s) 错误 = %v, 期望 errors.Is(err, fs.ErrNotExist)", name, err)
		}
	}
}

// 目录的 Readdir 给出直接子项（名称与 IsDir），不夹带别的层；且只查这一层。
func TestFS_ReaddirDirectChildren(t *testing.T) {
	ft := newFakeTree()
	fsys := NewFS(ft)
	ctx := testCtx(t)

	f, err := fsys.OpenFile(ctx, davPath(fakeDramaDir), 0, 0)
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

	// 列一个剧目录只需要「父目录 + 该目录」两层，不能枚举整棵树。
	seen := map[string]bool{}
	for _, dir := range ft.listed {
		seen[dir] = true
	}
	for dir := range seen {
		if strings.HasPrefix(dir, fakeSeason) {
			t.Errorf("列举剧目录时不该查季目录：查过 %v", ft.listed)
		}
	}
	if !seen[fakeDramaDir] {
		t.Errorf("列举剧目录应查它自己，实际查过 %v", ft.listed)
	}
}

// 目录的 OpenFile **不查子项**：x/net/webdav 对每个子项都 OpenFile 一次，
// 打开即列子项等于把「列举一层」放大成「列举 N 层 + 渲染孙级文件」。
func TestFS_OpenDirIsLazy(t *testing.T) {
	ft := newFakeTree()
	fsys := NewFS(ft)
	ctx := withSnapshot(testCtx(t))

	f, err := fsys.OpenFile(ctx, davPath(fakeShardDir), 0, 0)
	if err != nil {
		t.Fatalf("OpenFile(分片目录): %v", err)
	}
	// 只查了父目录（用来定位分片目录自身），分片目录还没查、孙级文件一个都没渲染。
	if ft.lists != 1 || ft.listed[0] != fakeGenreDir {
		t.Errorf("打开目录后 List 调用 = %d 次（%v）, 期望只查父目录 %q", ft.lists, ft.listed, fakeGenreDir)
	}
	if ft.renders != 0 {
		t.Errorf("打开目录就渲染了 %d 个文件, 期望 0", ft.renders)
	}

	// Readdir 才真正列子项：查分片目录一次，仍然不渲染孙级文件。
	children, err := f.Readdir(0)
	if err != nil {
		t.Fatalf("Readdir: %v", err)
	}
	if len(children) != 1 || children[0].Name() != "测试剧 (2024)" {
		t.Fatalf("子项 = %v, 期望只有剧目录", children)
	}
	if ft.lists != 2 || ft.listed[1] != fakeShardDir {
		t.Errorf("Readdir 后 List 调用 = %d 次（%v）, 期望追加查 %q", ft.lists, ft.listed, fakeShardDir)
	}
	if ft.renders != 0 {
		t.Errorf("列举分片目录渲染了 %d 个孙级文件, 期望 0", ft.renders)
	}

	// 再 Readdir 一次不重复查（已加载）。
	if _, err := f.Readdir(0); err != nil {
		t.Fatalf("第二次 Readdir: %v", err)
	}
	if ft.lists != 2 {
		t.Errorf("第二次 Readdir 又查了一次（List = %d）, 期望复用", ft.lists)
	}
}

// 不存在的目录：错误的包装必须是 *fs.PathError —— x/net/webdav 用 os.IsNotExist
// 分流，普通 %w 包装会让它落到 405（协议层），线上表现为「不存在的路径返回 405」。
func TestFS_NotFoundIsPathError(t *testing.T) {
	ft := newFakeTree()
	_, err := NewFS(ft).Stat(testCtx(t), "/爱情/7f")
	if err == nil {
		t.Fatalf("Stat(不存在) 没有报错")
	}
	if !os.IsNotExist(err) {
		t.Errorf("os.IsNotExist(%v) = false, 期望 true（必须用 *fs.PathError 包装）", err)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		t.Errorf("错误类型 = %T, 期望 *fs.PathError", err)
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

// 请求内快照：同一请求里 Stat 三次同一文件，只查一次该目录、只渲染一次。
func TestFS_SnapshotRendersOncePerRequest(t *testing.T) {
	ft := newFakeTree()
	fsys := NewFS(ft)
	ctx := withSnapshot(testCtx(t))

	for i := 0; i < 3; i++ {
		if _, err := fsys.Stat(ctx, davPath(fakeStrm)); err != nil {
			t.Fatalf("第 %d 次 Stat: %v", i+1, err)
		}
	}
	if ft.lists != 1 {
		t.Errorf("List 调用 %d 次, 期望 1（请求内快照，只查季目录一层）：%v", ft.lists, ft.listed)
	}
	if ft.listed[0] != fakeSeason {
		t.Errorf("List 查了 %q, 期望只查 %q", ft.listed[0], fakeSeason)
	}
	if ft.renders != 1 {
		t.Errorf("Render 调用 %d 次, 期望 1（同请求同路径复用）", ft.renders)
	}

	// 新请求（新快照）必须重新查：没有跨请求缓存（硬约束 5 即时生效）。
	ctx2 := withSnapshot(testCtx(t))
	if _, err := fsys.Stat(ctx2, "/爱情"); err != nil {
		t.Fatalf("新请求 Stat: %v", err)
	}
	if ft.lists != 2 {
		t.Errorf("新请求后 List 调用 %d 次, 期望 2（不跨请求复用）", ft.lists)
	}
}

// 列一个分片目录（PROPFIND Depth:1 的形态）时，每个子项只 Stat，不再逐项查库：
// 快照让「一次目录列举」= 每层一次 List + 每个文件一次 Render。
func TestFS_DepthOneListsEachDirOnce(t *testing.T) {
	ft := newFakeTree()
	fsys := NewFS(ft)
	ctx := withSnapshot(testCtx(t))

	f, err := fsys.OpenFile(ctx, davPath(fakeSeason), 0, 0)
	if err != nil {
		t.Fatalf("OpenFile(季目录): %v", err)
	}
	children, err := f.Readdir(0)
	if err != nil {
		t.Fatalf("Readdir: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("子项数 = %d, 期望 2", len(children))
	}
	// x/net/webdav 会对每个子项再 Stat 一次：同一路径必须复用快照。
	for _, c := range children {
		if _, err := fsys.Stat(ctx, davPath(fakeSeason)+"/"+c.Name()); err != nil {
			t.Fatalf("Stat(%s): %v", c.Name(), err)
		}
	}
	// 两个目录各查一次：父目录（定位季目录自身）+ 季目录（列子项）；
	// 之后每个子项的 Stat 复用快照，不再加查询。
	if ft.lists != 2 {
		t.Errorf("List 调用 %d 次, 期望 2（父目录 + 季目录各一次）：%v", ft.lists, ft.listed)
	}
	if ft.renders != 2 {
		t.Errorf("Render 调用 %d 次, 期望 2（两个文件各一次）", ft.renders)
	}
}

// List 失败必须上报（不能把树伪装成只有根）。
func TestFS_ListErrorPropagates(t *testing.T) {
	sentinel := errors.New("查库失败")
	ft := newFakeTree()
	ft.listErr[""] = sentinel

	_, err := NewFS(ft).Stat(testCtx(t), "/爱情")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Stat 错误 = %v, 期望 %v", err, sentinel)
	}
}

// 请求内快照不得跨 goroutine 泄漏：两次独立快照互不影响（-race 下验证）。
func TestFS_SnapshotIsolated(t *testing.T) {
	ft := newFakeTree()
	fsys := NewFS(ft)
	a, b := withSnapshot(testCtx(t)), withSnapshot(testCtx(t))

	if _, err := fsys.Stat(a, "/爱情"); err != nil {
		t.Fatalf("请求 A Stat: %v", err)
	}
	if _, err := fsys.Stat(b, "/爱情"); err != nil {
		t.Fatalf("请求 B Stat: %v", err)
	}
	if ft.lists != 2 {
		t.Errorf("两个请求各应查一次，实际 %d 次", ft.lists)
	}
}

// baseOf 与 notExist 是两个小工具：路径末段与 404 包装。
func TestBaseOfAndNotExist(t *testing.T) {
	if got := baseOf(path.Join(fakeRoot, "爱情", "0a", "剧 (2024)")); got != "剧 (2024)" {
		t.Errorf("baseOf = %q, 期望 剧 (2024)", got)
	}
	if got := baseOf("短剧"); got != "短剧" {
		t.Errorf("baseOf(顶层) = %q, 期望 短剧", got)
	}
	if !errors.Is(notExist("/x"), fs.ErrNotExist) {
		t.Errorf("notExist 未包装 fs.ErrNotExist")
	}
}

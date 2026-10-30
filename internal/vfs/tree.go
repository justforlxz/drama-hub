package vfs

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/justforlxz/drama-hub/internal/store"
)

// Tree 是虚拟文件树的中枢：List 枚举节点，Render 渲染文件字节。
//
// 无状态、无缓存：每次请求现算，一次请求内完成（AGENTS.md 硬约束 3）。
// 由装配根（cmd）构造并单例持有；M4 的 webdav 通过 List/Render 消费本树。
type Tree struct {
	st       *store.Store
	settings *store.Settings
	token    TokenFunc
}

// TokenFunc 由装配根注入，计算 ADR-0003 的出流 token（secret + uid → 签名）。
//
// vfs（rank 30）不 import internal/stream（rank 40），HMAC 算法在 M5 的
// stream.Token 里实现；M3 单测注入 stub。
type TokenFunc func(secret, uid string) string

// NewTree 构造 Tree。token 不可为 nil（渲染 strm 必需）。
func NewTree(st *store.Store, settings *store.Settings, token TokenFunc) *Tree {
	return &Tree{st: st, settings: settings, token: token}
}

// Node 是 List 输出的一个条目：相对 vfs 根（无前导 /）的路径与其类型。
type Node struct {
	Path  string
	IsDir bool
}

// root 是树的固定一级目录。
const root = "短剧"

// seasonDir 是季目录名。1.0 只有单季，emby_item.season 恒为 1；目录名与派生表
// 的 season 字段在这里必须一致。
const seasonDir = "Season 01"

// List 枚举整棵树的目录与文件节点，路径无前导 /。
//
// 每次请求现算：1 次 ListDramas + 每剧 1 次 ListEmbyItems（派生表）。目录（根、
// 剧目录、Season 01）IsDir=true，strm/nfo 文件 IsDir=false。M4 用它回答 PROPFIND
// 的目录成员与类型。
//
// 对外一集（单集或合并集）由 emby_item 决定：一个合并组在这里只产出 1 条文件对，
// Emby 因此把它显示为 1 集（硬约束 6）。
func (t *Tree) List(ctx context.Context) ([]Node, error) {
	dramas, err := t.dramaEntries(ctx)
	if err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(dramas)*4+1)
	nodes = append(nodes, Node{Path: root, IsDir: true})
	for _, d := range dramas {
		dir := root + "/" + dramaDir(d.drama)
		season := dir + "/" + seasonDir
		nodes = append(nodes,
			Node{Path: dir, IsDir: true},
			Node{Path: dir + "/tvshow.nfo", IsDir: false},
			Node{Path: season, IsDir: true},
		)
		for _, item := range d.items {
			base := embyBase(d.drama, item)
			nodes = append(nodes,
				Node{Path: season + "/" + base + ".strm", IsDir: false},
				Node{Path: season + "/" + base + ".nfo", IsDir: false},
			)
		}
	}
	return nodes, nil
}

// dramaEntry 是 List 的数据视图：一部剧与其全部对外剧集。
type dramaEntry struct {
	drama store.Drama
	items []store.EmbyItem
}

// dramaEntries 读回全部剧目与其派生剧集，供 List 枚举整棵树（1+N 次查询）。
//
// Render 不走这里：它按目录名只取一部剧再取派生剧集（见 dramaByDir），
// 单文件渲染不随全库规模放大。
func (t *Tree) dramaEntries(ctx context.Context) ([]dramaEntry, error) {
	dramas, err := t.st.ListDramas(ctx)
	if err != nil {
		return nil, fmt.Errorf("列剧目: %w", err)
	}
	out := make([]dramaEntry, 0, len(dramas))
	for _, d := range dramas {
		items, err := t.st.ListEmbyItems(ctx, d.ID)
		if err != nil {
			return nil, fmt.Errorf("列对外剧集 drama_id=%d: %w", d.ID, err)
		}
		out = append(out, dramaEntry{drama: d, items: items})
	}
	return out, nil
}

// dramaDir 是剧目录名：<剧名> (<年份>)。年份是命名的一部分，Year==0 显式
// 写成 (<0>)，不省略括号——命名是纯函数，空态显式化。
func dramaDir(d store.Drama) string {
	return sanitizeName(d.Title) + " (" + strconv.Itoa(d.Year) + ")"
}

// embyBase 是对外剧集的文件名主干（不含扩展名）：<剧名> - S01E01 - <标签>。
// NFO 的 <title> 与 .strm/.nfo 的文件名共用它，杜绝两处漂移。
//
// 集号取 emby_item.ep_no、标签见 embyLabel，两者都只来自派生表（硬约束 6）：
// 合并调整重算派生表后文件名随之变化。
func embyBase(d store.Drama, item store.EmbyItem) string {
	return sanitizeName(d.Title) + " - " + embyNumber(item) + " - " + embyLabel(item)
}

// embyNumber 是集号文本 S%02dE%02d（1.0 恒为 Season 01）。
func embyNumber(item store.EmbyItem) string {
	return "S" + twoDigit(item.Season) + "E" + twoDigit(item.EpNo)
}

// twoDigit 把集号格式化为两位，超过两位时按原宽度展开。
func twoDigit(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// embyLabel 是对外剧集的标签：
//
//   - 合并集：`第 A-B 集`（A/B 是组内最小/最大源集号；A==B 时退化为 `第 A 集`）
//   - 单集：源集标题；空则回退 `第 N 集`（N 是对外集号）
//
// 文件名主干与 NFO <title> 共用本函数，避免两处漂移。
func embyLabel(item store.EmbyItem) string {
	if item.Kind == "merged" {
		if item.EpFrom == item.EpTo {
			return "第" + strconv.Itoa(item.EpFrom) + "集"
		}
		return "第" + strconv.Itoa(item.EpFrom) + "-" + strconv.Itoa(item.EpTo) + "集"
	}
	label := sanitizeName(item.Title)
	if label == "" {
		label = "第" + strconv.Itoa(item.EpNo) + "集"
	}
	return label
}

// sanitizeName 把 Windows / WebDAV 非法字符替换为 _。目录名与文件名共用。
func sanitizeName(s string) string {
	return strings.NewReplacer(
		"/", "_", `\`, "_", ":", "_", "*", "_",
		"?", "_", `"`, "_", "<", "_", ">", "_", "|", "_",
	).Replace(s)
}

// Render 渲染 path 处的文件字节：解析路径 → 查库 → 一次返回完整字节。
//
// 路径以 / 分隔、相对 vfs 根、无前导 /（WebDAV 的绝对路径由 M4 映射）。
// 合法形状只有剧级 tvshow.nfo 与集级 .strm/.nfo 两种；目录、未知路径、
// 段数不符、不存在的剧/集一律 ErrNotFound。返回字节的长度就是 len(body)
// （硬约束 3），本包没有任何独立的长度字段或方法。
func (t *Tree) Render(ctx context.Context, p string) ([]byte, error) {
	req, ok := parsePath(p)
	if !ok {
		return nil, ErrNotFound
	}
	d, err := t.dramaByDir(ctx, req.dramaDir)
	if err != nil {
		return nil, err
	}
	if req.isShow {
		return renderShowNFO(d)
	}
	items, err := t.st.ListEmbyItems(ctx, d.ID)
	if err != nil {
		return nil, fmt.Errorf("列对外剧集 drama_id=%d: %w", d.ID, err)
	}
	for _, item := range items {
		if embyBase(d, item) != req.itemBase {
			continue
		}
		if req.ext == ".strm" {
			return t.renderStrm(ctx, item)
		}
		return renderItemNFO(item)
	}
	return nil, ErrNotFound
}

// dramaByDir 按目录名精确匹配一部剧：1 次 ListDramas，不枚举其余剧的剧集。
//
// Render 的成本因此与全库规模无关：剧级文件 1 次查询，集级文件 2 次
// （ListDramas + 该剧的 ListEmbyItems）。全树遍历只属于 List。
func (t *Tree) dramaByDir(ctx context.Context, dir string) (store.Drama, error) {
	dramas, err := t.st.ListDramas(ctx)
	if err != nil {
		return store.Drama{}, fmt.Errorf("列剧目: %w", err)
	}
	for _, d := range dramas {
		if dramaDir(d) == dir {
			return d, nil
		}
	}
	return store.Drama{}, ErrNotFound
}

// renderRequest 是 parsePath 的解构结果。
type renderRequest struct {
	dramaDir string // 剧目录名
	isShow   bool   // true = 剧级 tvshow.nfo；false = 集级文件
	itemBase string // 文件名主干（isShow 时为空）
	ext      string // ".strm" / ".nfo"（isShow 时为空）
}

// parsePath 解析相对路径；ok=false 表示不是本树里可渲染的文件形状。
//
// 合法：短剧/<剧目录>/tvshow.nfo（3 段）
//
//	短剧/<剧目录>/Season 01/<文件名>.strm|.nfo（4 段）
func parsePath(p string) (renderRequest, bool) {
	p = strings.TrimPrefix(p, "/")
	segs := strings.Split(p, "/")
	switch {
	case len(segs) == 3 && segs[0] == root && segs[2] == "tvshow.nfo":
		return renderRequest{dramaDir: segs[1], isShow: true}, true
	case len(segs) == 4 && segs[0] == root && segs[2] == seasonDir:
		ext := extension(segs[3])
		if ext != ".strm" && ext != ".nfo" {
			return renderRequest{}, false
		}
		name := strings.TrimSuffix(segs[3], ext)
		if name == "" {
			return renderRequest{}, false
		}
		return renderRequest{dramaDir: segs[1], itemBase: name, ext: ext}, true
	}
	return renderRequest{}, false
}

// extension 返回路径最后一段的扩展名（含点），没有则空串。
//
// 手写而不 import path：只认最后一个点，与 path.Ext 语义一致但少一个依赖。
func extension(base string) string {
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		return base[i:]
	}
	return ""
}

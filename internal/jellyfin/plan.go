package jellyfin

import (
	"context"
	"path"
	"sort"
	"strings"
	"time"
)

// Config 是五个配置项在本包里的形态（键与校验的真值在 internal/store）。
type Config struct {
	// URL 是 Jellyfin 基址，如 http://jellyfin:8096；空 = 未配置。
	URL string
	// APIKey 只用于出站请求头，不回显、不写日志（AC-8）。
	APIKey string
	// LibraryRoot 是 **Jellyfin 容器内**看到的挂载根，如 /data/video/dramahub。
	// 填成宿主路径或本服务的挂载点都会建出空库。
	LibraryRoot string
	// Prefix 是库名前缀，同时是「哪些库归本服务管」的第一条判定。
	Prefix string
	// Timeout 是单次出站超时。
	Timeout time.Duration
}

// Missing 返回启用还缺哪几个键，顺序固定（状态页与错误文本都用它，顺序稳定才可逐字比较）。
//
// 三项都非空即启用：不设 `jellyfin_enabled`，少一个状态就少一种「显示已配置但不跑」。
func (c Config) Missing() []string {
	var out []string
	for _, item := range []struct {
		key   string
		empty bool
	}{
		{"jellyfin_url", c.URL == ""},
		{"jellyfin_api_key", c.APIKey == ""},
		{"jellyfin_library_root", c.LibraryRoot == ""},
	} {
		if item.empty {
			out = append(out, item.key)
		}
	}
	return out
}

// Enabled 报告三项启用条件是否齐了。
func (c Config) Enabled() bool { return len(c.Missing()) == 0 }

// Library 是 GET /Library/VirtualFolders 的一个元素（只保留本包用到的字段）。
type Library struct {
	Name           string   `json:"Name"`
	ItemID         string   `json:"ItemId"`
	Locations      []string `json:"Locations"`
	CollectionType string   `json:"CollectionType"`
}

// Action 是一次建库：库名 = 前缀 + 分类目录名，路径 = 库根/<分类目录名>。
type Action struct {
	Name string
	Path string
}

// Plan 是一次 reconcile 的差异。
type Plan struct {
	// Create 是启用且还没有对应自管库的分类。
	Create []Action
	// Delete 是分类被关掉的自管库名：这一轮就要删（MKACG-668 的「关闭即删库」）。
	// 只出现在下一轮同步里，拨开关的瞬间不动手。
	Delete []string
	// Orphans 是自管库里有、分类里已经没有的库名（只报告，不删）。
	Orphans []string
	// Conflicts 是「名字被一个非自管库占着」的库名：不建、不删、不接管，只报告。
	Conflicts []string
}

// Diff 算出一次 reconcile 该做什么。纯函数：不碰网络、不碰库，可直接单测（AC-9）。
//
// 函数名不叫 Plan：Plan 是返回值的类型（Go 同包内类型与函数共用一个命名空间）。
//
// 自管库由三条同时成立决定（缺一条都不算，任一条单独成立都可能是别人的库）：
//
//  1. 名字带 prefix；
//  2. 只有一个 Locations；
//  3. 那个位置正好是 root 的直接子目录（path.Clean 后比较，更深或更浅都不算）。
//
// 已匹配的自管库一律不动：不重命名、不改路径、不同步 libraryOptions —— 那些都会
// 制造「第二次触发又改了东西」，与幂等直接冲突。
//
// disabled 是分类开关（MKACG-668）：被关掉的分类不建库，已有的自管库进 Delete。
// 分类整个消失（不在 categories 里）仍然只算孤儿 —— 删除只由显式关闭的开关触发，
// 树里一次读不到某个分类不会顺手把库删掉。
func Diff(existing []Library, categories, disabled []string, root, prefix string) Plan {
	var out Plan
	// managed 记「自管库名 → 名字去掉前缀后的分类」。
	managed := make(map[string]string, len(existing))
	byName := make(map[string]Library, len(existing))
	for _, lib := range existing {
		byName[lib.Name] = lib
		if category, ok := Managed(lib, root, prefix); ok {
			managed[lib.Name] = category
		}
	}

	off := make(map[string]struct{}, len(disabled))
	for _, category := range disabled {
		if category != "" {
			off[category] = struct{}{}
		}
	}

	present := make(map[string]struct{}, len(categories))
	for _, category := range categories {
		present[category] = struct{}{}
		if _, isOff := off[category]; isOff {
			continue
		}
		name := prefix + category
		if _, ok := managed[name]; ok {
			continue
		}
		if _, taken := byName[name]; taken {
			out.Conflicts = append(out.Conflicts, name)
			continue
		}
		out.Create = append(out.Create, Action{Name: name, Path: path.Join(path.Clean(root), category)})
	}

	for name, category := range managed {
		if _, isOff := off[category]; isOff {
			out.Delete = append(out.Delete, name)
			continue
		}
		if _, ok := present[category]; !ok {
			out.Orphans = append(out.Orphans, name)
		}
	}
	sort.Strings(out.Delete)
	sort.Strings(out.Orphans)
	return out
}

// Managed 判断一个库是否归本服务管，是则给出它对应的分类（= 名字去掉前缀）。
//
// 只在名字带前缀时才有意义：不带前缀的库无论路径如何都不动（别人的库）。
func Managed(lib Library, root, prefix string) (string, bool) {
	if prefix == "" || !strings.HasPrefix(lib.Name, prefix) {
		return "", false
	}
	if len(lib.Locations) != 1 {
		return "", false
	}
	category, ok := strings.CutPrefix(lib.Name, prefix)
	if !ok || category == "" {
		return "", false
	}
	if path.Dir(path.Clean(lib.Locations[0])) != path.Clean(root) {
		return "", false
	}
	return category, true
}

// LoadConfig 现读五个配置项（装配根接到 store.Settings）。
//
// 一次性给整份 Config 而不是五个 getter：状态页要的「缺哪几个键」是 Config 自己的
// 判断（Missing），调用方不需要知道有哪几个键。
type LoadConfig func(ctx context.Context) (Config, error)

// Categories 列库根下的分类目录名（装配根接到 vfs 的根目录）。
//
// 口径必须与虚拟文件树的根目录完全一致（转义规则 + 「其他」兜底），否则库指向一个
// 不存在的目录；所以这条能力只能从 vfs 来，本包不自己读 drama.genre。
type Categories func(ctx context.Context) ([]string, error)

// Disabled 列被关掉的分类（装配根接到 store 的分类开关表，MKACG-668）。
//
// 行不存在 = 启用，所以这里给出的就是「关掉的那些」。每轮开始时现读：拨开关不触发
// 同步，下一轮（周期或手动）从库里读到新状态后才执行删除。
type Disabled func(ctx context.Context) ([]string, error)

// CategoryState 是分类开关列表的一行：分类名 + 当前是否同步到 Jellyfin。
type CategoryState struct {
	Name    string
	Enabled bool
}

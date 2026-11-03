package jellyfin

import (
	"slices"
	"testing"
)

// TestDiff 是差异算法的唯一守门测试（AC-9）：自管库的三条限定同时成立才算自管，
// 别人的库一个都不进 create / orphan / conflicts。
func TestDiff(t *testing.T) {
	const (
		root   = "/data/video/dramahub"
		prefix = "短剧 - "
	)

	managed := func(name, location string) Library {
		return Library{Name: prefix + name, Locations: []string{location}, CollectionType: "tvshows"}
	}

	cases := []struct {
		name       string
		existing   []Library
		categories []string
		disabled   []string
		wantCreate []Action
		wantDelete []string
		wantOrphan []string
		wantConf   []string
	}{
		{
			name:       "空库按分类全建",
			categories: []string{"都市", "爱情"},
			wantCreate: []Action{
				{Name: "短剧 - 都市", Path: "/data/video/dramahub/都市"},
				{Name: "短剧 - 爱情", Path: "/data/video/dramahub/爱情"},
			},
			wantOrphan: []string{},
			wantConf:   []string{},
		},
		{
			name: "已存在的自管库一个都不动（幂等）",
			existing: []Library{
				managed("都市", "/data/video/dramahub/都市"),
				{Name: "Movies", Locations: []string{"/data/video/movies"}},
			},
			categories: []string{"都市"},
			wantCreate: []Action{},
			wantOrphan: []string{},
			wantConf:   []string{},
		},
		{
			name: "分类新增时只补差集",
			existing: []Library{
				managed("都市", "/data/video/dramahub/都市"),
			},
			categories: []string{"都市", "科幻"},
			wantCreate: []Action{{Name: "短剧 - 科幻", Path: "/data/video/dramahub/科幻"}},
			wantOrphan: []string{},
			wantConf:   []string{},
		},
		{
			name: "分类消失是孤儿：默认只报告",
			existing: []Library{
				managed("都市", "/data/video/dramahub/都市"),
				managed("已下架", "/data/video/dramahub/已下架"),
			},
			categories: []string{"都市"},
			wantCreate: []Action{},
			wantOrphan: []string{"短剧 - 已下架"},
			wantConf:   []string{},
		},
		{
			// 名字带前缀但位置不在 root 下一层（更深）：不是自管库，既不建也不删。
			// 分类仍缺 → 会尝试建同名库吗？不会：名字已被占用 → conflict。
			name: "带前缀但位置更深：不算自管",
			existing: []Library{
				{Name: prefix + "都市", Locations: []string{"/data/video/dramahub/都市/Season 01"}},
			},
			categories: []string{"都市"},
			wantCreate: []Action{},
			wantOrphan: []string{},
			wantConf:   []string{"短剧 - 都市"},
		},
		{
			name: "带前缀但位置在 root 之外：不算自管",
			existing: []Library{
				{Name: prefix + "动画", Locations: []string{"/data/video/animations"}},
			},
			categories: []string{"动画"},
			wantCreate: []Action{},
			wantOrphan: []string{},
			wantConf:   []string{"短剧 - 动画"},
		},
		{
			name: "带前缀但位置在 root 之上（root 本身）：不算自管",
			existing: []Library{
				{Name: prefix + "全部", Locations: []string{root}},
			},
			categories: []string{},
			wantCreate: []Action{},
			wantOrphan: []string{},
			wantConf:   []string{},
		},
		{
			// 名字不带前缀的库永远不进自管集合：它既不是孤儿（不删），也不构成同名冲突
			// （要占的是 `短剧 - 都市` 这个名字，而它叫 `都市`）。所以照常建自己的库 ——
			// 两条库指向同一路径是 Jellyfin 允许的，前缀才是归属的判据。
			name: "没有前缀的库：路径在 root 下也不算自管",
			existing: []Library{
				{Name: "都市", Locations: []string{"/data/video/dramahub/都市"}},
				{Name: "Movies", Locations: []string{"/data/video/movies"}},
			},
			categories: []string{"都市"},
			wantCreate: []Action{{Name: "短剧 - 都市", Path: "/data/video/dramahub/都市"}},
			wantOrphan: []string{},
			wantConf:   []string{},
		},
		{
			name: "多个 Locations：不算自管（路径不唯一，无法判定归属）",
			existing: []Library{
				{Name: prefix + "都市", Locations: []string{root + "/都市", "/data/video/other"}},
			},
			categories: []string{"都市"},
			wantCreate: []Action{},
			wantOrphan: []string{},
			wantConf:   []string{"短剧 - 都市"},
		},
		{
			name: "路径带多余斜杠与 . 段：Clean 后仍算同一位置",
			existing: []Library{
				managed("都市", root+"/./都市/"),
			},
			categories: []string{"都市"},
			wantCreate: []Action{},
			wantOrphan: []string{},
			wantConf:   []string{},
		},
		{
			name: "孤儿按名字排序（状态页与日志的稳定顺序）",
			existing: []Library{
				managed("b", root+"/b"),
				managed("c", root+"/c"),
				managed("a", root+"/a"),
			},
			categories: []string{},
			wantCreate: []Action{},
			wantOrphan: []string{prefix + "a", prefix + "b", prefix + "c"},
			wantConf:   []string{},
		},
		{
			// 开关就是删除开关（MKACG-668）：关掉的分类不建库，已有的自管库进 Delete。
			name: "关掉的分类：不建库、已有自管库进 Delete",
			existing: []Library{
				managed("都市", root+"/都市"),
				managed("爱情", root+"/爱情"),
			},
			categories: []string{"都市", "爱情", "科幻"},
			disabled:   []string{"爱情", "科幻"},
			wantCreate: []Action{},
			wantDelete: []string{prefix + "爱情"},
			wantOrphan: []string{},
			wantConf:   []string{},
		},
		{
			// 删除集合只含自管库：别人的库、带前缀但路径不对的库一个都不进。
			name: "Delete 只含自管库：别人的库与错位库不动",
			existing: []Library{
				{Name: "Movies", Locations: []string{"/data/video/movies"}},
				{Name: prefix + "都市", Locations: []string{"/data/video/elsewhere"}},
				{Name: prefix + "都市", Locations: []string{root + "/都市", "/data/video/other"}},
				managed("爱情", root+"/爱情"),
			},
			categories: []string{"都市", "爱情"},
			disabled:   []string{"都市", "爱情"},
			wantCreate: []Action{},
			wantDelete: []string{prefix + "爱情"},
			wantOrphan: []string{},
			wantConf:   []string{},
		},
		{
			// 开关行还在、分类已经从树里消失：仍然按关闭处理（库删掉）；没有库就是 0 删除。
			name: "关闭的分类已消失：有库就删，没库就什么都不做",
			existing: []Library{
				managed("已下架", root+"/已下架"),
			},
			categories: []string{"都市"},
			disabled:   []string{"已下架"},
			wantCreate: []Action{{Name: prefix + "都市", Path: root + "/都市"}},
			wantDelete: []string{prefix + "已下架"},
			wantOrphan: []string{},
			wantConf:   []string{},
		},
		{
			// 关掉的分类即使名字被别人的库占着也不算冲突：这一轮根本不会建这个库。
			name: "关掉的分类不报同名冲突",
			existing: []Library{
				{Name: prefix + "都市", Locations: []string{"/data/video/other/都市"}},
			},
			categories: []string{"都市"},
			disabled:   []string{"都市"},
			wantCreate: []Action{},
			wantDelete: []string{},
			wantOrphan: []string{},
			wantConf:   []string{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Diff(c.existing, c.categories, c.disabled, root, prefix)
			if !slices.Equal(got.Create, c.wantCreate) {
				t.Errorf("Create = %v, 期望 %v", got.Create, c.wantCreate)
			}
			if !slices.Equal(got.Delete, c.wantDelete) {
				t.Errorf("Delete = %v, 期望 %v", got.Delete, c.wantDelete)
			}
			if !slices.Equal(got.Orphans, c.wantOrphan) {
				t.Errorf("Orphans = %v, 期望 %v", got.Orphans, c.wantOrphan)
			}
			if !slices.Equal(got.Conflicts, c.wantConf) {
				t.Errorf("Conflicts = %v, 期望 %v", got.Conflicts, c.wantConf)
			}
		})
	}
}

// TestDiffPrefixEmpty 锁定空前缀的兜底：名字带空前缀等于「每个库都带前缀」，
// 若还判定位置，任何落在 root 下的别人的库都会变成自管、进而可能被删除。
func TestDiffPrefixEmpty(t *testing.T) {
	got := Diff([]Library{{Name: "都市", Locations: []string{"/data/video/dramahub/都市"}}},
		[]string{"都市"}, []string{"都市"}, "/data/video/dramahub", "")
	if len(got.Orphans) != 0 || len(got.Delete) != 0 {
		t.Errorf("空前缀下 Orphans = %v / Delete = %v, 期望都为空（不含任何库）", got.Orphans, got.Delete)
	}
}

// TestConfigMissing 锁定启用条件：三项都非空才启用，缺哪个键按固定顺序报出来。
func TestConfigMissing(t *testing.T) {
	full := Config{URL: "http://jellyfin:8096", APIKey: "k", LibraryRoot: "/data/video/dramahub", Prefix: "短剧 - "}
	if got := full.Missing(); len(got) != 0 || !full.Enabled() {
		t.Fatalf("齐全配置 Missing = %v, Enabled = %v, 期望都为空/true", got, full.Enabled())
	}
	cases := []struct {
		name string
		cfg  Config
		want []string
	}{
		{"全缺", Config{Prefix: "短剧 - "}, []string{"jellyfin_url", "jellyfin_api_key", "jellyfin_library_root"}},
		{"缺 key", Config{URL: "http://j:1", LibraryRoot: "/r", Prefix: "p"}, []string{"jellyfin_api_key"}},
		{"缺 root", Config{URL: "http://j:1", APIKey: "k", Prefix: "p"}, []string{"jellyfin_library_root"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.Missing(); !slices.Equal(got, c.want) {
				t.Errorf("Missing = %v, 期望 %v", got, c.want)
			}
			if c.cfg.Enabled() {
				t.Error("Enabled 应为 false")
			}
		})
	}
}

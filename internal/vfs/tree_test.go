package vfs

import (
	"context"
	"encoding/xml"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/store"
)

// testSecret 是 fixture 的 stream_secret，满足 store 的 ≥32 字节校验。
const testSecret = "0123456789abcdef0123456789abcdef"

// stubToken 是 M3 的 TokenFunc 替身：把入参编码进结果，证明 token 来自注入缝。
func stubToken(secret, uid string) string { return "tok(" + secret + ":" + uid + ")" }

// 固定 fixture 的剧名。titleB 带 Windows 非法字符 ':'，用于验证 sanitize 贯穿全树。
const (
	titleA = "测试剧"
	titleB = "另一部:剧"
)

// fixture 是一套两剧三集的数据：剧 A 有「有标签」与「无标签」两集，剧 B 无集。
type fixture struct {
	tree *Tree
	db   *store.DB
	epA1 store.Episode
	epA2 store.Episode
}

// seed 用真实临时库（WAL / FK / busy_timeout 由 store.Open 施加）建 fixture。
func seed(t *testing.T, strmPrefix string) fixture {
	t.Helper()
	return seedTree(t, strmPrefix, true)
}

// seedNoSecret 同 seed，但库中从未写入 stream_secret：验证缺密钥路径时
// 无需在测试里执行裸 SQL（SQL 只允许出现在 internal/store）。
func seedNoSecret(t *testing.T) fixture {
	t.Helper()
	return seedTree(t, "", false)
}

// seedTree 建 fixture；withSecret=false 时不写 stream_secret。
func seedTree(t *testing.T, strmPrefix string, withSecret bool) fixture {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "vfs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)

	aID, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "web", ProviderID: "a1", Title: titleA, Year: 2024,
		Plot: "第一行 & <第二行>", EpisodeTotal: 2,
	})
	if err != nil {
		t.Fatalf("UpsertDrama A: %v", err)
	}
	if _, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "app", ProviderID: "b1", Title: titleB, Year: 0,
	}); err != nil {
		t.Fatalf("UpsertDrama B: %v", err)
	}

	ep1 := store.Episode{DramaID: aID, EpNo: 1, Title: "开局", DurationSec: 125, Source: "web", Vid: "v1"}
	ep2 := store.Episode{DramaID: aID, EpNo: 2, Title: "", DurationSec: 59, Source: "web", Vid: "v2"}
	ep1.ID, err = st.UpsertEpisode(ctx, ep1)
	if err != nil {
		t.Fatalf("UpsertEpisode 1: %v", err)
	}
	ep2.ID, err = st.UpsertEpisode(ctx, ep2)
	if err != nil {
		t.Fatalf("UpsertEpisode 2: %v", err)
	}
	// 派生表是 vfs 的唯一数据源（硬约束 6）：seed 后必须重建一次，等价于
	// 「同步完成」那个生产调用点。
	if _, err := st.RebuildEmbyItems(ctx, aID); err != nil {
		t.Fatalf("RebuildEmbyItems A: %v", err)
	}

	if withSecret {
		if err := settings.Set(ctx, store.KeyStreamSecret, testSecret); err != nil {
			t.Fatalf("Set stream_secret: %v", err)
		}
	}
	if strmPrefix != "" {
		if err := settings.Set(ctx, store.KeyStrmPrefix, strmPrefix); err != nil {
			t.Fatalf("Set strm_prefix: %v", err)
		}
	}

	return fixture{tree: NewTree(st, settings, stubToken), db: db, epA1: ep1, epA2: ep2}
}

// AC1：List 返回全部目录与文件节点，路径与 IsDir 逐项相等。
func TestList(t *testing.T) {
	f := seed(t, "")
	nodes, err := f.tree.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	dirA := "短剧/" + titleA + " (2024)"
	dirB := "短剧/另一部_剧 (0)"
	want := []Node{
		{Path: "短剧", IsDir: true},
		{Path: dirA, IsDir: true},
		{Path: dirA + "/tvshow.nfo"},
		{Path: dirA + "/Season 01", IsDir: true},
		{Path: dirA + "/Season 01/" + titleA + " - S01E01 - 开局.strm"},
		{Path: dirA + "/Season 01/" + titleA + " - S01E01 - 开局.nfo"},
		{Path: dirA + "/Season 01/" + titleA + " - S01E02 - 第2集.strm"},
		{Path: dirA + "/Season 01/" + titleA + " - S01E02 - 第2集.nfo"},
		{Path: dirB, IsDir: true},
		{Path: dirB + "/tvshow.nfo"},
		{Path: dirB + "/Season 01", IsDir: true},
	}

	if len(nodes) != len(want) {
		t.Fatalf("节点数 = %d, 期望 %d\n实际 %#v", len(nodes), len(want), nodes)
	}
	for i := range want {
		if nodes[i] != want[i] {
			t.Errorf("节点[%d] = %+v, 期望 %+v", i, nodes[i], want[i])
		}
	}
}

// AC2：命名是纯函数（表驱动）。
// AC2：命名是纯函数（表驱动）。
func TestNames(t *testing.T) {
	drama := func(title string, year int) store.Drama {
		return store.Drama{Title: title, Year: year}
	}
	single := func(epNo int, title string) store.EmbyItem {
		return store.EmbyItem{Kind: "single", EpNo: epNo, Season: 1, Title: title, EpFrom: epNo, EpTo: epNo}
	}
	merged := func(epNo, from, to int) store.EmbyItem {
		return store.EmbyItem{Kind: "merged", EpNo: epNo, Season: 1, EpFrom: from, EpTo: to}
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"剧目录/正常", dramaDir(drama("你好世界", 2024)), "你好世界 (2024)"},
		{"剧目录/空年显式化为0", dramaDir(drama("无年份", 0)), "无年份 (0)"},
		{"剧目录/非法字符替换", dramaDir(drama(`a/b\c:d*e?f"g<h>i|j`, 2021)), "a_b_c_d_e_f_g_h_i_j (2021)"},
		{"集号/一位补零", embyNumber(single(1, "开局")), "S01E01"},
		{"集号/两位原样", embyNumber(single(12, "")), "S01E12"},
		{"集号/超两位展开", embyNumber(single(100, "")), "S01E100"},
		{"标签/有标题", embyLabel(single(1, "开局")), "开局"},
		{"标签/空标题回退", embyLabel(single(7, "")), "第7集"},
		{"标签/标题里的非法字符也替换", embyLabel(single(1, "a/b")), "a_b"},
		{"标签/合并集范围", embyLabel(merged(1, 1, 10)), "第1-10集"},
		{"标签/合并集单集退化为第N集", embyLabel(merged(1, 3, 3)), "第3集"},
		{"文件名主干/有标签", embyBase(drama("测试剧", 2024), single(1, "开局")), "测试剧 - S01E01 - 开局"},
		{"文件名主干/无标签", embyBase(drama("测试剧", 2024), single(2, "")), "测试剧 - S01E02 - 第2集"},
		{"文件名主干/合并集", embyBase(drama("测试剧", 2024), merged(1, 1, 10)), "测试剧 - S01E01 - 第1-10集"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// AC2：sanitizeName 表驱动。
func TestSanitizeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"干净的名字 2024", "干净的名字 2024"},
		{`/`, `_`},
		{`\`, `_`},
		{`:`, `_`},
		{`*`, `_`},
		{`?`, `_`},
		{`"`, `_`},
		{`<`, `_`},
		{`>`, `_`},
		{`|`, `_`},
		{`a/b\c:d*e?f"g<h>i|j`, `a_b_c_d_e_f_g_h_i_j`},
		{"", ""},
	}
	for _, tt := range tests {
		if got := sanitizeName(tt.in); got != tt.want {
			t.Errorf("sanitizeName(%q) = %q, 期望 %q", tt.in, got, tt.want)
		}
	}
}

// AC7：非文件路径一律 ErrNotFound（表驱动）。
func TestRenderNotFound(t *testing.T) {
	f := seed(t, "")
	dir := "短剧/" + titleA + " (2024)"
	season := dir + "/Season 01"

	paths := []string{
		"",
		"/",
		"短剧",
		dir,    // 剧目录当文件
		season, // 季目录当文件
		"其他/" + titleA + " (2024)/tvshow.nfo",                // 不在 短剧/ 下
		"短剧/不存在 (2024)/tvshow.nfo",                           // 剧不存在
		dir + "/tvshow.txt",                                  // 剧级扩展名非法
		dir + "/tvshow.nfo/x",                                // 段数多余
		dir + "/Season 02/" + titleA + " - S01E01 - 开局.strm", // 季目录错
		season + "/" + titleA + " - S01E99 - 第99集.strm",      // 集不存在
		season + "/" + titleA + " - S01E01 - 开局.mp4",         // 集扩展名非法
		season + "/" + titleA + " - S01E01 - 开局",             // 无扩展名
		season + "/tvshow.nfo",                               // 季目录下的 tvshow.nfo 不是合法形状
		season + "/.strm",                                    // 空主干
	}
	for _, p := range paths {
		body, err := f.tree.Render(context.Background(), p)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Render(%q) err = %v, 期望 ErrNotFound", p, err)
		}
		if body != nil {
			t.Errorf("Render(%q) 返回了 %d 字节, 期望 nil", p, len(body))
		}
	}
}

// AC（M7/硬约束 6）：合并组在树里只产出 1 条文件对，标签是「第 A-B 集」，
// 且 strm 的 uid 是合并集句柄 m<group_id>（不是任一单集 uid）。
func TestListMergedGroupCollapsesToOne(t *testing.T) {
	ctx := context.Background()
	f := seed(t, "")
	st := store.NewStore(f.db.DB)

	// 把 fixture 的两集并成 1 集。
	gid, err := st.CreateMergeGroup(ctx, 1, "第1-2集", []int64{f.epA1.ID, f.epA2.ID})
	if err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}
	items, err := st.ListEmbyItems(ctx, 1)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("合并后对外剧集数 = %d, 期望 1：%+v", len(items), items)
	}

	nodes, err := f.tree.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	season := "短剧/" + titleA + " (2024)" + "/Season 01"
	base := titleA + " - S01E01 - 第1-2集"
	wantStrm := season + "/" + base + ".strm"
	wantNFO := season + "/" + base + ".nfo"

	got := map[string]bool{}
	for _, n := range nodes {
		if strings.HasPrefix(n.Path, season+"") && !n.IsDir {
			got[n.Path] = true
		}
	}
	// 合并后季目录下只剩这一条文件对（其余节点不含集文件）。
	if len(got) != 2 || !got[wantStrm] || !got[wantNFO] {
		t.Fatalf("合并后季目录文件 = %v, 期望 %q 与 %q", got, wantStrm, wantNFO)
	}

	// strm 的 uid 是 m<group_id>，token 由注入的 TokenFunc 计算。
	uid := "m" + strconv.FormatInt(gid, 10)
	body, err := f.tree.Render(ctx, wantStrm)
	if err != nil {
		t.Fatalf("Render(%s): %v", wantStrm, err)
	}
	if want := "/s/" + stubToken(testSecret, uid) + "/e/" + uid + "\n"; string(body) != want {
		t.Errorf("合并集 strm = %q, 期望 %q", body, want)
	}

	// NFO 的 episode 号是对外集号 1，标题是「第1-2集」。
	nfo, err := f.tree.Render(ctx, wantNFO)
	if err != nil {
		t.Fatalf("Render(%s): %v", wantNFO, err)
	}
	var parsed episodeNFO
	if err := xml.Unmarshal(nfo, &parsed); err != nil {
		t.Fatalf("解析合并集 NFO: %v\n%s", err, nfo)
	}
	if parsed.Title != "第1-2集" {
		t.Errorf("合并集 NFO title = %q, 期望 第1-2集", parsed.Title)
	}
	if parsed.Episode != 1 {
		t.Errorf("合并集 NFO episode = %d, 期望 1（对外集号）", parsed.Episode)
	}
}

package vfs

import (
	"context"
	"errors"
	"path/filepath"
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
func TestNames(t *testing.T) {
	drama := func(title string, year int) store.Drama {
		return store.Drama{Title: title, Year: year}
	}
	ep := func(no int, title string) store.Episode {
		return store.Episode{EpNo: no, Title: title}
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"剧目录/正常", dramaDir(drama("你好世界", 2024)), "你好世界 (2024)"},
		{"剧目录/空年显式化为0", dramaDir(drama("无年份", 0)), "无年份 (0)"},
		{"剧目录/非法字符替换", dramaDir(drama(`a/b\c:d*e?f"g<h>i|j`, 2021)), "a_b_c_d_e_f_g_h_i_j (2021)"},
		{"集号/一位补零", episodeNumber(ep(1, "开局")), "S01E01"},
		{"集号/两位原样", episodeNumber(ep(12, "")), "S01E12"},
		{"集号/超两位展开", episodeNumber(ep(100, "")), "S01E100"},
		{"标签/有标题", episodeLabel(ep(1, "开局")), "开局"},
		{"标签/空标题回退", episodeLabel(ep(7, "")), "第7集"},
		{"标签/标题里的非法字符也替换", episodeLabel(ep(1, "a/b")), "a_b"},
		{"文件名主干/有标签", episodeBase(drama("测试剧", 2024), ep(1, "开局")), "测试剧 - S01E01 - 开局"},
		{"文件名主干/无标签", episodeBase(drama("测试剧", 2024), ep(2, "")), "测试剧 - S01E02 - 第2集"},
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

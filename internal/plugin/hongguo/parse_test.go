package hongguo

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// 真实页面 fixture 的身份（2026-10-29 抓取的《宴律，你的白月光回国了》）。
const (
	fixtureSID   = "7503745441910017049"
	fixtureVID   = "7503780574541122622"
	fixtureName  = "宴律，你的白月光回国了"
	fixtureIntro = "七年前，顾星晚捅伤亲生父亲入狱；五年前，宴矜撑伞在监狱门口等她，却得知她早已出狱。时隔七年重逢，顾星晚望着律界大佬宴矜怔愣，对方却称不认识她，且早已订有婚约。顾星晚选择缄口不言，无人知晓，她曾在狱中为宴矜生下过一个孩子，过往的纠葛与未说出口的秘密，成了她心底最深的牵挂。"
	fixtureCover = "https://p3-novel.byteimg.com/novel-pic/34ad28f319c9c2b6598792ce2deaaaf8~tplv-shrink:640:0.image"
)

// readFixture 读 testdata/html 下的页面 fixture。
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "html", name))
	if err != nil {
		t.Fatalf("读 fixture %s: %v", name, err)
	}
	return string(b)
}

// fixtureVidList 从 fixture 的原始 JSON 里独立取出 vid_list，作为解析结果的对照。
//
// 刻意不复用 parsePlayerPage：这段用最直白的正则+json.Unmarshal，若被测函数丢项、
// 换序或串了字段，两边就对不上。
func fixtureVidList(t *testing.T, name string) []string {
	t.Helper()
	m := regexp.MustCompile(`"vid_list":\s*(\[[^\]]*\])`).FindStringSubmatch(readFixture(t, name))
	if m == nil {
		t.Fatalf("fixture %s 里找不到 vid_list", name)
	}
	var vids []string
	if err := json.Unmarshal([]byte(m[1]), &vids); err != nil {
		t.Fatalf("解析 fixture %s 的 vid_list: %v", name, err)
	}
	return vids
}

// AC1：76 项 vid_list 逐项相等（下标即集号由 SyncSeries 的 ep_no 派生保证）。
func TestParsePlayerPageFullVidList(t *testing.T) {
	got, err := parsePlayerPage(readFixture(t, "page-series.html"), fixtureSID)
	if err != nil {
		t.Fatalf("parsePlayerPage: %v", err)
	}
	want := fixtureVidList(t, "page-series.html")
	if len(want) != 76 {
		t.Fatalf("fixture 自身 vid_list len = %d, 期望 76", len(want))
	}
	if !reflect.DeepEqual(got.vids, want) {
		t.Fatalf("vid_list 不一致：got %d 项 %v..., want %d 项 %v...",
			len(got.vids), head(got.vids), len(want), head(want))
	}
	if got.vids[0] != fixtureVID {
		t.Fatalf("vids[0] = %q, 期望 %q", got.vids[0], fixtureVID)
	}
	if got.vids[75] != "7503780672947899454" {
		t.Fatalf("vids[75] = %q, 期望第 76 集的 vid", got.vids[75])
	}
}

// AC1：3 项 fixture 逐项可枚举。
func TestParsePlayerPageSmallVidList(t *testing.T) {
	got, err := parsePlayerPage(readFixture(t, "page-series-small.html"), fixtureSID)
	if err != nil {
		t.Fatalf("parsePlayerPage: %v", err)
	}
	want := []string{fixtureVID, "7503780577347128382", "7503780574608231486"}
	if !reflect.DeepEqual(got.vids, want) {
		t.Fatalf("vid_list = %v, 期望 %v", got.vids, want)
	}
	if got.name != "测试剧 三集版" {
		t.Fatalf("name = %q, 期望 fixture 的 series_name", got.name)
	}
}

// AC2：剧目字段映射（title/plot/cover/first_visible_time）。
func TestParsePlayerPageSeriesFields(t *testing.T) {
	got, err := parsePlayerPage(readFixture(t, "page-series.html"), fixtureSID)
	if err != nil {
		t.Fatalf("parsePlayerPage: %v", err)
	}
	if got.sid != fixtureSID || got.name != fixtureName || got.intro != fixtureIntro || got.cover != fixtureCover {
		t.Fatalf("剧目字段不符：sid=%q name=%q intro=%q cover=%q", got.sid, got.name, got.intro, got.cover)
	}
	if got.firstVisibleTime != "1747533495" {
		t.Fatalf("first_visible_time = %q, 期望 \"1747533495\"", got.firstVisibleTime)
	}
	if year := yearFromUnix(got.firstVisibleTime); year != 2025 {
		t.Fatalf("yearFromUnix = %d, 期望 2025", year)
	}
}

// AC5/AC6：身份不符与结构变化都必须报 plugin.ErrSchema，且不与 plugin.ErrUpstream 混淆。
func TestParsePlayerPageRejects(t *testing.T) {
	cases := []struct {
		fixture string
		why     string
	}{
		{"page-wrong-series.html", "series_id 与请求 sid 不符（被锁集 302 到别处）"},
		{"page-missing-router.html", "缺 _ROUTER_DATA"},
		{"page-truncated-json.html", "_ROUTER_DATA JSON 截断"},
		{"page-missing-vidlist.html", "seriesDetail 缺 vid_list"},
		{"page-empty-vidlist.html", "vid_list 为空"},
		{"page-invalid-vids.html", "vid_list 含非数字项"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			_, err := parsePlayerPage(readFixture(t, tc.fixture), fixtureSID)
			if !errors.Is(err, plugin.ErrSchema) {
				t.Fatalf("%s：err = %v, 期望 plugin.ErrSchema", tc.why, err)
			}
			if errors.Is(err, plugin.ErrUpstream) || errors.Is(err, plugin.ErrNoStream) {
				t.Fatalf("%s：错误类别串了：%v", tc.why, err)
			}
		})
	}
}

// AC3：同一页含非 VideoObject 块 + VideoObject 块，恰取后者并绑定本集。
func TestParseDirectURL(t *testing.T) {
	got, err := parseDirectURL(readFixture(t, "page-episode.html"), fixtureSID, fixtureVID)
	if err != nil {
		t.Fatalf("parseDirectURL: %v", err)
	}
	want := DefaultBaseURL + "/video/public/" + fixtureVID + "/360p.mp4"
	if got != want {
		t.Fatalf("直链 = %q, 期望 %q", got, want)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", got, err)
	}
	if u.RawQuery != "" {
		t.Fatalf("直链残留 query：%q", u.RawQuery)
	}
	if u.Path != "/video/public/"+fixtureVID+"/360p.mp4" {
		t.Fatalf("直链 path = %q, 期望本集 vid", u.Path)
	}
}

// AC4：\u002F / \u0026 / &amp; 还原，Range 参数删除。
func TestParseDirectURLNormalize(t *testing.T) {
	got, err := parseDirectURL(readFixture(t, "page-episode-normalize.html"), fixtureSID, fixtureVID)
	if err != nil {
		t.Fatalf("parseDirectURL: %v", err)
	}
	want := DefaultBaseURL + "/video/public/" + fixtureVID + "/360p.mp4?b=2&token=a"
	if got != want {
		t.Fatalf("规整后 = %q, 期望 %q", got, want)
	}
	if strings.Contains(got, `\u002F`) || strings.Contains(got, `\u0026`) || strings.Contains(got, "&amp;") {
		t.Fatalf("转义残留：%q", got)
	}
	if strings.Contains(strings.ToLower(got), "range=") {
		t.Fatalf("Range 参数未删除：%q", got)
	}
}

// normalizeURL 的边界：只有 Range、尾部空 & / ?。
func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"https://h.example/v.mp4?Range=0-1":      "https://h.example/v.mp4",
		"https://h.example/v.mp4?a=1&Range=0-1&": "https://h.example/v.mp4?a=1",
		"https://h.example/v.mp4?":               "https://h.example/v.mp4",
		`https:\u002F\u002Fh.example\u002Fa.mp4`: "https://h.example/a.mp4",
	}
	for in, want := range cases {
		got, err := normalizeURL(in)
		if err != nil {
			t.Fatalf("normalizeURL(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("normalizeURL(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// AC10：VideoObject 在、contentUrl 缺失 → plugin.ErrNoStream（与 plugin.ErrSchema 可区分）。
func TestParseDirectURLNoContent(t *testing.T) {
	_, err := parseDirectURL(readFixture(t, "page-episode-no-content.html"), fixtureSID, fixtureVID)
	if !errors.Is(err, plugin.ErrNoStream) {
		t.Fatalf("err = %v, 期望 plugin.ErrNoStream", err)
	}
	if errors.Is(err, plugin.ErrSchema) {
		t.Fatalf("plugin.ErrNoStream 不应同时是 plugin.ErrSchema: %v", err)
	}
}

// AC3 的反面：没有本集 VideoObject、或块绑定到别的剧/别的集 → plugin.ErrSchema。
func TestParseDirectURLRejects(t *testing.T) {
	cases := []struct{ fixture, why string }{
		{"page-episode-no-videobject.html", "页面只有非 VideoObject 块"},
		{"page-episode-other-series.html", "VideoObject 绑定别的剧/别的集"},
		{"page-episode-wrong-vid.html", "url 绑定本剧但 contentUrl 是别的集（被锁集 302）"},
		{"page-wrong-series.html", "页面 series_id 与请求 sid 不符（缺 JSON-LD）"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			_, err := parseDirectURL(readFixture(t, tc.fixture), fixtureSID, fixtureVID)
			if !errors.Is(err, plugin.ErrSchema) {
				t.Fatalf("%s：err = %v, 期望 plugin.ErrSchema", tc.why, err)
			}
			if errors.Is(err, plugin.ErrNoStream) {
				t.Fatalf("%s：不应报 plugin.ErrNoStream: %v", tc.why, err)
			}
		})
	}
}

func head(vids []string) []string {
	if len(vids) > 3 {
		return vids[:3]
	}
	return vids
}

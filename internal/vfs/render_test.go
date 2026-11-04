package vfs

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"slices"
	"strconv"
	"testing"

	"github.com/justforlxz/drama-hub/internal/store"
)

// singleItem 独立构造单集派生行（与 store 的 uid 规则一致：`e<episode.id>`）。
func singleItem(ep store.Episode) store.EmbyItem {
	return store.EmbyItem{
		Kind: "single", Season: 1, EpNo: ep.EpNo, UID: "e" + strconv.FormatInt(ep.ID, 10),
		EpisodeID: ep.ID, EpFrom: ep.EpNo, EpTo: ep.EpNo, Title: ep.Title, RuntimeSec: ep.DurationSec,
	}
}

// strmPath / nfoPath 是 fixture 里剧 A 某集的两条渲染路径（独立于被测代码拼出）。
func (f fixture) strmPath(ep store.Episode) string {
	return f.dirA + "/" + seasonDir + "/" + embyBase(store.Drama{Title: titleA}, singleItem(ep)) + ".strm"
}

func (f fixture) nfoPath(ep store.Episode) string {
	return f.dirA + "/" + seasonDir + "/" + embyBase(store.Drama{Title: titleA}, singleItem(ep)) + ".nfo"
}

// uidFor 是外部集在 strm 里的 uid 段：e<episode.id>。
func uidFor(ep store.Episode) string { return "e" + strconv.FormatInt(ep.ID, 10) }

// AC3：strm 内容 = {prefix}/video/{id}.m3u8\n，单行、尾换行；两个前缀用例。
func TestRenderStrm(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
	}{
		{name: "带前缀", prefix: "https://proxy.example:8080"},
		{name: "空前缀", prefix: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := seed(t, tt.prefix)
			ctx := context.Background()

			for _, ep := range []store.Episode{f.epA1, f.epA2} {
				path := f.strmPath(ep)
				body, err := f.tree.Render(ctx, path)
				if err != nil {
					t.Fatalf("Render(%s): %v", path, err)
				}
				uid := uidFor(ep)
				want := tt.prefix + "/video/" + stubID(testSecret, uid) + ".m3u8\n"
				if string(body) != want {
					t.Errorf("strm 内容 = %q, 期望 %q", body, want)
				}
				if len(body) != len(want) {
					t.Errorf("len(body) = %d, 期望 %d", len(body), len(want))
				}
				if n := bytes.Count(body, []byte("\n")); n != 1 || body[len(body)-1] != '\n' {
					t.Errorf("strm 应是单行 + 尾换行，实际 %q", body)
				}
			}
		})
	}
}

// AC4：集 NFO 解析后字段齐全（title/season/episode/runtime），runtime 为分钟。
func TestRenderEpisodeNFO(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()

	tests := []struct {
		ep          store.Episode
		wantTitle   string
		wantRuntime int
	}{
		{ep: f.epA1, wantTitle: "开局", wantRuntime: 2},  // 125s → 2min
		{ep: f.epA2, wantTitle: "第2集", wantRuntime: 0}, // 59s → 0min
	}
	for _, tt := range tests {
		path := f.nfoPath(tt.ep)
		body, err := f.tree.Render(ctx, path)
		if err != nil {
			t.Fatalf("Render(%s): %v", path, err)
		}
		if len(body) == 0 {
			t.Fatalf("ep_no=%d 渲染出 0 字节", tt.ep.EpNo)
		}

		var got episodeNFO
		if err := xml.Unmarshal(body, &got); err != nil {
			t.Fatalf("解析集 NFO 失败: %v\n%s", err, body)
		}
		if got.XMLName.Local != "episodedetails" {
			t.Errorf("根元素 = %q, 期望 episodedetails", got.XMLName.Local)
		}
		if got.Title != tt.wantTitle {
			t.Errorf("title = %q, 期望 %q", got.Title, tt.wantTitle)
		}
		if got.Season != 1 {
			t.Errorf("season = %d, 期望 1", got.Season)
		}
		if got.Episode != tt.ep.EpNo {
			t.Errorf("episode = %d, 期望 %d", got.Episode, tt.ep.EpNo)
		}
		if got.Runtime != tt.wantRuntime {
			t.Errorf("runtime = %d, 期望 %d（秒=%d）", got.Runtime, tt.wantRuntime, tt.ep.DurationSec)
		}
		// 集简介没有单集来源，用剧简介兜底；不能是空元素。
		if got.Plot != "第一行 & <第二行>" {
			t.Errorf("plot = %q, 期望剧简介", got.Plot)
		}
	}
}

// nfoShowView 只声明被测元素，不复用生产结构体：生产结构体若把 `xml:"genre"` 写成
// `xml:"genres"`，用它反序列化依然是绿的；独立视图解析不到就是空切片。根元素名也由
// XMLName 标签校验（encoding/xml 在不匹配时报错）。
type nfoShowView struct {
	XMLName xml.Name `xml:"tvshow"`
	Genres  []string `xml:"genre"`
	Tags    []string `xml:"tag"`
}

// AC5：剧 NFO 解析后 title/year/plot 正确（含 XML 转义的 & 与 <>）。
func TestRenderShowNFO(t *testing.T) {
	f := seed(t, "")
	body, err := f.tree.Render(context.Background(), f.dirA+"/tvshow.nfo")
	if err != nil {
		t.Fatalf("Render tvshow.nfo: %v", err)
	}

	var got showNFO
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析剧 NFO 失败: %v\n%s", err, body)
	}
	if got.XMLName.Local != "tvshow" {
		t.Errorf("根元素 = %q, 期望 tvshow", got.XMLName.Local)
	}
	if got.Title != titleA {
		t.Errorf("title = %q, 期望 %q", got.Title, titleA)
	}
	if got.Year != 2024 {
		t.Errorf("year = %d, 期望 2024", got.Year)
	}
	if got.Plot != "第一行 & <第二行>" {
		t.Errorf("plot = %q, 期望 %q", got.Plot, "第一行 & <第二行>")
	}
	// genre = 主类型在前 + tags 其余；tag = tags 全量。
	// 用独立视图解析，元素名写错时这里会红。
	var view nfoShowView
	if err := xml.Unmarshal(body, &view); err != nil {
		t.Fatalf("按独立视图解析剧 NFO 失败（根元素或元素名不符）: %v\n%s", err, body)
	}
	if want := []string{"爱情", "逆袭"}; !slices.Equal(view.Genres, want) {
		t.Errorf("genre = %q, 期望 %q", view.Genres, want)
	}
	if want := []string{"爱情", "逆袭"}; !slices.Equal(view.Tags, want) {
		t.Errorf("tag = %q, 期望 %q", view.Tags, want)
	}
}

// AC5：genre/tag 的四种输入——tags 为空、单标签、多标签含重复、含 XML 特殊字符。
func TestRenderShowNFOGenresAndTags(t *testing.T) {
	tests := []struct {
		name       string
		genre      string
		tags       string
		wantGenres []string
		wantTags   []string
	}{
		{name: "空 tags", genre: "", tags: "", wantGenres: nil, wantTags: nil},
		{name: "空 JSON 数组", genre: "剧情", tags: "[]", wantGenres: []string{"剧情"}, wantTags: nil},
		{name: "单标签", genre: "剧情", tags: `["剧情"]`, wantGenres: []string{"剧情"}, wantTags: []string{"剧情"}},
		{
			name:       "多标签含重复",
			genre:      "都市",
			tags:       `["都市","爱情","都市","逆袭"]`,
			wantGenres: []string{"都市", "爱情", "逆袭"},
			wantTags:   []string{"都市", "爱情", "都市", "逆袭"},
		},
		{
			name:       "XML 特殊字符",
			genre:      "剧情&悬疑",
			tags:       `["A<B","C&D"]`,
			wantGenres: []string{"剧情&悬疑", "A<B", "C&D"},
			wantTags:   []string{"A<B", "C&D"},
		},
		{name: "非法 JSON", genre: "剧情", tags: `{"x":1}`, wantGenres: []string{"剧情"}, wantTags: nil},
		{name: "空串标签", genre: "剧情", tags: `["","爱情",""]`, wantGenres: []string{"剧情", "爱情"}, wantTags: []string{"爱情"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := renderShowNFO(store.Drama{
				Title: "剧", Year: 2026, Plot: "简介", Genre: tt.genre, Tags: tt.tags,
			}, false)
			if err != nil {
				t.Fatalf("renderShowNFO: %v", err)
			}
			// 特殊字符必须被转义，不能产出非法 XML；独立视图保证元素名写错会红。
			var got nfoShowView
			if err := xml.Unmarshal(body, &got); err != nil {
				t.Fatalf("解析剧 NFO 失败（不是合法 XML）: %v\n%s", err, body)
			}
			if !slices.Equal(got.Genres, tt.wantGenres) {
				t.Errorf("genre = %q, 期望 %q\n%s", got.Genres, tt.wantGenres, body)
			}
			if !slices.Equal(got.Tags, tt.wantTags) {
				t.Errorf("tag = %q, 期望 %q\n%s", got.Tags, tt.wantTags, body)
			}
			if len(tt.wantGenres) == 0 && bytes.Contains(body, []byte("<genre")) {
				t.Errorf("空 tags 却写了 <genre>：%s", body)
			}
			if len(tt.wantTags) == 0 && bytes.Contains(body, []byte("<tag")) {
				t.Errorf("空 tags 却写了 <tag>：%s", body)
			}
		})
	}
}

// AC6：List 列出的每个文件都能渲染，且返回字节数 = 独立构造的期望字节数（>0）。
//
// strm 用前缀 + id 独立拼出期望；nfo 用「解析后重新序列化」独立重建整段字节，
// 长度与内容都必须逐字节相等 —— 没有任何「先返回 0 再补」的中间态。
func TestRenderLengthEqualsBytes(t *testing.T) {
	const prefix = "https://proxy.example:8080"
	f := seed(t, prefix)
	ctx := context.Background()

	files := 0
	for _, n := range f.walk(t) {
		if n.IsDir {
			continue
		}
		files++
		body, err := f.tree.Render(ctx, n.Path)
		if err != nil {
			t.Fatalf("Render(%s): %v", n.Path, err)
		}
		if len(body) == 0 {
			t.Errorf("Render(%s) 产出 0 字节", n.Path)
			continue
		}

		var want []byte
		switch {
		case len(n.Path) > len(".strm") && n.Path[len(n.Path)-len(".strm"):] == ".strm":
			uid := uidForPath(t, n.Path, f)
			want = []byte(prefix + "/video/" + stubID(testSecret, uid) + ".m3u8\n")
		default:
			want = rebuildNFO(t, body)
		}
		if !bytes.Equal(body, want) {
			t.Errorf("%s 长度/内容不符：len(body)=%d, len(want)=%d\n实际 %q\n期望 %q",
				n.Path, len(body), len(want), body, want)
		}
	}
	// 2 部剧：A 有 tvshow.nfo + 2 集 ×2 文件 = 5；B 只有 tvshow.nfo = 1。
	if want := 6; files != want {
		t.Errorf("可渲染文件数 = %d, 期望 %d", files, want)
	}
}

// uidForPath 从 strm 路径独立解析出 uid：集文件主干尾部就是 S01E01，映射回 fixture 的集主键。
func uidForPath(t *testing.T, path string, f fixture) string {
	t.Helper()
	for _, ep := range []store.Episode{f.epA1, f.epA2} {
		if path == f.strmPath(ep) {
			return uidFor(ep)
		}
	}
	t.Fatalf("路径 %s 不在 fixture 里", path)
	return ""
}

// rebuildNFO 独立重建 NFO 字节：固定头 + 解析后重新序列化 + 尾换行。
func rebuildNFO(t *testing.T, body []byte) []byte {
	t.Helper()
	var ep episodeNFO
	if err := xml.Unmarshal(body, &ep); err == nil && ep.XMLName.Local == "episodedetails" {
		inner, err := xml.MarshalIndent(ep, "", "  ")
		if err != nil {
			t.Fatalf("重建集 NFO: %v", err)
		}
		return append(append([]byte(xmlHeader), inner...), '\n')
	}
	var show showNFO
	if err := xml.Unmarshal(body, &show); err == nil && show.XMLName.Local == "tvshow" {
		inner, err := xml.MarshalIndent(show, "", "  ")
		if err != nil {
			t.Fatalf("重建剧 NFO: %v", err)
		}
		return append(append([]byte(xmlHeader), inner...), '\n')
	}
	t.Fatalf("无法解析 NFO：%s", body)
	return nil
}

// AC8：uid 确定性——同一集两次渲染字节完全一致；不同集 URL 不同。
func TestStrmDeterministic(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()

	path := f.strmPath(f.epA1)
	first, err := f.tree.Render(ctx, path)
	if err != nil {
		t.Fatalf("第一次 Render: %v", err)
	}
	second, err := f.tree.Render(ctx, path)
	if err != nil {
		t.Fatalf("第二次 Render: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("同一集两次渲染不同：%q vs %q", first, second)
	}

	other, err := f.tree.Render(ctx, f.strmPath(f.epA2))
	if err != nil {
		t.Fatalf("Render 另一集: %v", err)
	}
	if bytes.Equal(first, other) {
		t.Errorf("不同集渲染出同一 URL：%q", first)
	}
}

// AC3 补充：strm_prefix 每次现读——改值后下一次渲染立即生效（硬约束 5）。
func TestRenderStrmReadsSettingsLive(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	path := f.strmPath(f.epA1)

	if _, err := f.tree.Render(ctx, path); err != nil {
		t.Fatalf("首次 Render: %v", err)
	}
	if err := store.NewSettings(f.db.DB).Set(ctx, store.KeyStrmPrefix, "https://changed.example"); err != nil {
		t.Fatalf("改 strm_prefix: %v", err)
	}
	body, err := f.tree.Render(ctx, path)
	if err != nil {
		t.Fatalf("改后 Render: %v", err)
	}
	uid := uidFor(f.epA1)
	if want := "https://changed.example/video/" + stubID(testSecret, uid) + ".m3u8\n"; string(body) != want {
		t.Errorf("改前缀后 strm = %q, 期望 %q", body, want)
	}
}

// strm_prefix 带尾 `/` 时不能拼出 `//video/`（MKACG-617 实测）：
// `http://host/stream/` 与 `http://host/stream` 必须渲染出同一个 URL。
func TestRenderStrmTrimsTrailingSlash(t *testing.T) {
	ctx := context.Background()
	withSlash := seed(t, "http://host.example/stream/")
	withoutSlash := seed(t, "http://host.example/stream")

	a, err := withSlash.tree.Render(ctx, withSlash.strmPath(withSlash.epA1))
	if err != nil {
		t.Fatalf("Render(尾斜杠): %v", err)
	}
	b, err := withoutSlash.tree.Render(ctx, withoutSlash.strmPath(withoutSlash.epA1))
	if err != nil {
		t.Fatalf("Render(无尾斜杠): %v", err)
	}
	uid := uidFor(withSlash.epA1)
	want := "http://host.example/stream/video/" + stubID(testSecret, uid) + ".m3u8\n"
	if string(a) != want {
		t.Errorf("尾斜杠渲染 = %q, 期望 %q", a, want)
	}
	if string(a) != string(b) {
		t.Errorf("两种写法渲染结果不同：\n%s\n%s", a, b)
	}
}

// AC3 边界：缺 stream_secret 时透传 store.ErrSettingMissing，不产出脏字节。
func TestRenderStrmMissingSecret(t *testing.T) {
	f := seedNoSecret(t)
	body, err := f.tree.Render(context.Background(), f.strmPath(f.epA1))
	if !errors.Is(err, store.ErrSettingMissing) {
		t.Errorf("err = %v, 期望 store.ErrSettingMissing", err)
	}
	if body != nil {
		t.Errorf("缺密钥时返回了 %d 字节", len(body))
	}
}

// AC：标题被 sanitize 过的剧（目录名与 title 原文不同）仍能按目录名定位并渲染
// ——点查 idx_drama_title 不中时退到分类内比对，不是 404。
func TestRenderSanitizedDramaDir(t *testing.T) {
	f := seed(t, "")
	body, err := f.tree.Render(context.Background(), f.dirB+"/tvshow.nfo")
	if err != nil {
		t.Fatalf("Render(剧 B tvshow.nfo): %v", err)
	}
	var got showNFO
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析剧 NFO: %v\n%s", err, body)
	}
	if got.Title != titleB {
		t.Errorf("title = %q, 期望 %q（sanitize 只作用于目录名，不改 NFO）", got.Title, titleB)
	}
}

// AC：同名冲突的两部剧各自按带后缀的目录名定位，渲染出各自的 tvshow.nfo。
func TestRenderConflictSuffixDirs(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	ids := []string{"dup-0", "dup-1"}
	dirs := make([]string, 0, 2)
	for i, id := range ids {
		f.addDrama(t, store.Drama{
			Provider: "web", ProviderID: id, Title: "同名剧", Year: 2026,
			Genre: genreA, Plot: "第" + strconv.Itoa(i) + "部",
		})
		dirs = append(dirs, dramaDirPath(genreA, "同名剧 (2026) [web-"+id+"]"))
	}

	for i, dir := range dirs {
		body, err := f.tree.Render(ctx, dir+"/tvshow.nfo")
		if err != nil {
			t.Fatalf("Render(%s/tvshow.nfo): %v", dir, err)
		}
		var got showNFO
		if err := xml.Unmarshal(body, &got); err != nil {
			t.Fatalf("解析剧 NFO: %v\n%s", err, body)
		}
		if want := "第" + strconv.Itoa(i) + "部"; got.Plot != want {
			t.Errorf("%s 渲染到别的剧：plot = %q, 期望 %q", dir, got.Plot, want)
		}
	}
}

// MKACG-650 AC：poster.jpg 的字节就是该剧本地封面缓存的字节（逐字节相等）。
func TestRenderPosterFromCoverCache(t *testing.T) {
	f := seed(t, "")
	f.covers.has[coverKey("web", "a1")] = coverStub

	body, err := f.tree.Render(context.Background(), f.dirA+"/poster.jpg")
	if err != nil {
		t.Fatalf("Render poster.jpg: %v", err)
	}
	if !bytes.Equal(body, coverStub) {
		t.Fatalf("poster 字节 = %q, 期望 %q", body, coverStub)
	}
}

// MKACG-650 AC：没有可用封面时 poster.jpg 是「不存在」（404），不是 0 字节文件——
// 三种情形都按不存在处理：源未声明封面、索引行缺失、回源失败。
func TestRenderPosterUnavailable(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()

	// 剧 A 声明了封面 URL，但替身里没有本地封面 → 未命中。
	if body, err := f.tree.Render(ctx, f.dirA+"/poster.jpg"); !errors.Is(err, ErrNotFound) || body != nil {
		t.Fatalf("无本地封面时 Render = (%q, %v), 期望 (nil, ErrNotFound)", body, err)
	}

	// 本地有行但回源失败 → 仍然是「不存在」，且不返回半截字节。
	f.covers.has[coverKey("web", "a1")] = coverStub
	f.covers.fails[coverKey("web", "a1")] = true
	if body, err := f.tree.Render(ctx, f.dirA+"/poster.jpg"); !errors.Is(err, ErrNotFound) || body != nil {
		t.Fatalf("回源失败时 Render = (%q, %v), 期望 (nil, ErrNotFound)", body, err)
	}
}

// MKACG-650 AC：有封面的剧 tvshow.nfo 带 <thumb aspect="poster">poster.jpg</thumb>
// （相对路径，不写外链）；没有封面的剧一个 <thumb> 都没有。
func TestRenderShowNFOThumb(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()

	without, err := f.tree.Render(ctx, f.dirB+"/tvshow.nfo")
	if err != nil {
		t.Fatalf("Render(剧 B tvshow.nfo): %v", err)
	}
	if bytes.Contains(without, []byte("<thumb")) {
		t.Fatalf("无封面的剧 nfo 里出现了 thumb：%s", without)
	}

	f.covers.has[coverKey("web", "a1")] = coverStub
	with, err := f.tree.Render(ctx, f.dirA+"/tvshow.nfo")
	if err != nil {
		t.Fatalf("Render(剧 A tvshow.nfo): %v", err)
	}
	// 逐字节钉住元素形态：属性在前、值是相对路径、不是外链。
	if want := []byte(`<thumb aspect="poster">poster.jpg</thumb>`); !bytes.Contains(with, want) {
		t.Fatalf("有封面的剧 nfo 缺 %s：\n%s", want, with)
	}

	var got showNFO
	if err := xml.Unmarshal(with, &got); err != nil {
		t.Fatalf("解析带 thumb 的剧 NFO 失败（不是合法 XML）: %v\n%s", err, with)
	}
	if got.Thumb == nil || got.Thumb.Aspect != "poster" || got.Thumb.Value != "poster.jpg" {
		t.Fatalf("thumb = %+v, 期望 aspect=poster、值=poster.jpg", got.Thumb)
	}
	if got.Title != titleA || got.Year != 2024 {
		t.Errorf("带 thumb 后 title/year = %q/%d, 期望 %q/2024", got.Title, got.Year, titleA)
	}
}

// AC-3：同一部剧的 .strm 内容与它的目录路径无关。
//
// 内容只由三样东西决定：settings 的 strm_prefix、对外剧集 uid、注入的 IDFunc。
// 分片层下线后路径全变，这条测试钉住「路径不参与内容」——同一条对外剧集在两条
// 不同目录路径下渲染出来的字节必须逐字节相同。
func TestStrmContentIndependentOfPath(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()

	before, err := f.tree.Render(ctx, f.dirA+"/"+seasonDir+"/"+titleA+" - S01E01 - 开局.strm")
	if err != nil {
		t.Fatalf("Render(改造后路径): %v", err)
	}

	// 换分类（上游 tags[0] 变了）：剧目录整体搬到另一个分类目录下。
	if _, err := f.store().UpsertDrama(ctx, store.Drama{
		Provider: "web", ProviderID: "a1", Title: titleA, Year: 2024,
		Plot: "第一行 & <第二行>", EpisodeTotal: 2, Genre: "都市",
	}); err != nil {
		t.Fatalf("UpsertDrama(换分类): %v", err)
	}
	moved := Root + "/都市/" + titleA + " (2024)"
	after, err := f.tree.Render(ctx, moved+"/"+seasonDir+"/"+titleA+" - S01E01 - 开局.strm")
	if err != nil {
		t.Fatalf("Render(换分类后的路径 %s): %v", moved, err)
	}

	if !bytes.Equal(before, after) {
		t.Errorf("同一集在两条路径下的 strm 不同：\n%s\n%s", before, after)
	}
	// 旧路径随之失效（剧已搬走），不是另一份拷贝。
	if _, err := f.tree.Render(ctx, f.dirA+"/"+seasonDir+"/"+titleA+" - S01E01 - 开局.strm"); !errors.Is(err, ErrNotFound) {
		t.Errorf("旧路径仍可渲染（err = %v），期望 ErrNotFound", err)
	}
}

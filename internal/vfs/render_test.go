package vfs

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"strconv"
	"testing"

	"github.com/justforlxz/drama-hub/internal/store"
)

// dramaDirA 是 fixture 里剧 A 的目录名（独立于被测代码拼出）。
const dramaDirA = "短剧/" + titleA + " (2024)"

// singleItem 独立构造单集派生行（与 store 的 uid 规则一致：`e<episode.id>`）。
func singleItem(ep store.Episode) store.EmbyItem {
	return store.EmbyItem{
		Kind: "single", Season: 1, EpNo: ep.EpNo, UID: "e" + strconv.FormatInt(ep.ID, 10),
		EpisodeID: ep.ID, EpFrom: ep.EpNo, EpTo: ep.EpNo, Title: ep.Title, RuntimeSec: ep.DurationSec,
	}
}

// strmPath / nfoPath 是 fixture 里剧 A 某集的两条渲染路径（独立于被测代码拼出）。
func (f fixture) strmPath(ep store.Episode) string {
	return dramaDirA + "/Season 01/" + embyBase(store.Drama{Title: titleA}, singleItem(ep)) + ".strm"
}

func (f fixture) nfoPath(ep store.Episode) string {
	return dramaDirA + "/Season 01/" + embyBase(store.Drama{Title: titleA}, singleItem(ep)) + ".nfo"
}

// AC3：strm 内容 = {prefix}/s/{token}/e/{uid}\n，单行、尾换行；两个前缀用例。
func TestRenderStrm(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
	}{
		{name: "带前缀", prefix: "https://proxy.example/dav"},
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
				uid := "e" + strconv.FormatInt(ep.ID, 10)
				want := tt.prefix + "/s/" + stubToken(testSecret, uid) + "/e/" + uid + "\n"
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
	}
}

// AC5：剧 NFO 解析后 title/year/plot 正确（含 XML 转义的 & 与 <>）。
func TestRenderShowNFO(t *testing.T) {
	f := seed(t, "")
	body, err := f.tree.Render(context.Background(), dramaDirA+"/tvshow.nfo")
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
}

// AC6：List 列出的每个文件都能渲染，且返回字节数 = 独立构造的期望字节数（>0）。
//
// strm 用前缀 + uid 独立拼出期望；nfo 用「解析后重新序列化」独立重建整段字节，
// 长度与内容都必须逐字节相等 —— 没有任何「先返回 0 再补」的中间态。
func TestRenderLengthEqualsBytes(t *testing.T) {
	const prefix = "https://proxy.example/dav"
	f := seed(t, prefix)
	ctx := context.Background()

	nodes, err := f.tree.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	files := 0
	for _, n := range nodes {
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
			want = []byte(prefix + "/s/" + stubToken(testSecret, uid) + "/e/" + uid + "\n")
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
			return "e" + strconv.FormatInt(ep.ID, 10)
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
	uid := "e" + strconv.FormatInt(f.epA1.ID, 10)
	if want := "https://changed.example/s/" + stubToken(testSecret, uid) + "/e/" + uid + "\n"; string(body) != want {
		t.Errorf("改前缀后 strm = %q, 期望 %q", body, want)
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

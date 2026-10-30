package sync

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/plugin/hongguo"
)

// 本文件是 S5 的「全量」端到端验收：枚举并集 → 逐剧 Work → 入库，最后用库里的行
// 证明「剧目数 = 枚举数、逐剧集数 = 声明集数、ep_no = 下标 + 1」。
//
// 走的是生产同一条链路（hongguo.Enumerate / hongguo.Work + sync.Syncer.SyncSeries），
// 只是上游换成 httptest 假站点：sitemap 与 /category 用插件包里的真实裁剪 fixture，
// 详情页用真实页面改 series_id。

// enumFixtureIDs 是目录 fixture 的并集（与插件包 enumerate_test.go 同一份 fixture）：
// sitemap 段 7 部（7619615344046378009 / 7616334357598309401 / 7683103397246946328 /
// 7701334722449787928 / 7603667977744960574 / 7605500183484320792 / 7615465407347952664）
// ∪ /category 段 3 部（7701334722449787928 / 7694653385668037657 / 7693854781961423897），
// 交集 1 部 → 9 部；其中 2 部只在 /category 里。
var enumFixtureIDs = struct{ union []string }{
	union: []string{
		"7619615344046378009", "7616334357598309401", "7683103397246946328",
		"7701334722449787928", "7603667977744960574", "7605500183484320792",
		"7615465407347952664", "7694653385668037657", "7693854781961423897",
	},
}

// bigWorkID 用 76 集那份 fixture（其余用 3 集那份）：让「逐剧集数 = 声明集数」在一轮里
// 有两种不同的声明值，而不是九个相同数字。
const bigWorkID = "7693854781961423897"

// enumSite 起一个假上游：目录三段 + 每部剧的详情页。
func enumSite(t *testing.T) *httptest.Server {
	t.Helper()
	routes := map[string]string{
		"/sitemap/hongguoduanju/index.xml":  readFixture(t, "sitemap-index.xml"),
		"/sitemap/hongguoduanju/index1.xml": readFixture(t, "sitemap-1.xml"),
		"/sitemap/hongguoduanju/index2.xml": readFixture(t, "sitemap-2.xml"),
		"/seo-important.xml":                readFixture(t, "sitemap-seo.xml"),
		"/category":                         readFixture(t, "category-page1.html"),
		"/category?page=2":                  readFixture(t, "category-page2.html"),
	}
	for _, id := range enumFixtureIDs.union {
		// 详情页：真实页面换 series_id。被换的只有「哪部剧」，SSR 结构与 vid_list
		// 长度仍是真实页面，所以集数声明是真实的 76 / 3。
		routes["/player/"+id] = workPage(t, id)
	}
	// fixture 里的 sitemap loc 是绝对 URL（真实行为），这里把域名换成假站点。
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 按 RequestURI 应答：/category 与 /category?page=2 是两页。
		body, ok := routes[r.URL.RequestURI()]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, strings.ReplaceAll(body, hongguo.DefaultBaseURL, baseURL))
	}))
	t.Cleanup(srv.Close)
	baseURL = srv.URL
	return srv
}

// workPage 用真实详情页 fixture 生成任意 id 的详情页。
func workPage(t *testing.T, id string) string {
	t.Helper()
	fixture := "page-series-small.html"
	if id == bigWorkID {
		fixture = "page-series.html"
	}
	body := readFixture(t, fixture)
	if !strings.Contains(body, fixtureSID) {
		t.Fatalf("fixture %s 里没有 %s，无法改身份", fixture, fixtureSID)
	}
	return strings.ReplaceAll(body, fixtureSID, id)
}

// TestFullSyncEnumeratesUnionAndWritesAll 是全量入库的端到端用例。
func TestFullSyncEnumeratesUnionAndWritesAll(t *testing.T) {
	srv := enumSite(t)
	ctx := context.Background()
	st := newTestStore(t)
	p := hongguo.New(hongguo.Config{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	syncer := New(p, nil)
	syncer.now = (&fakeClock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}).now

	// 段 1：枚举（框架从空游标起逐页推进，正是 internal/scheduler 的 enumerate 逻辑）。
	refs := enumerateAllRefs(t, p)
	if len(refs) != 9 {
		t.Fatalf("枚举去重后 %d 部, 期望 9 部（%v）", len(refs), refs)
	}

	// 段 2：逐部 Work + 入库。
	for _, ref := range refs {
		if err := syncer.SyncSeries(ctx, st, ref.ID); err != nil {
			t.Fatalf("SyncSeries(%s): %v", ref.ID, err)
		}
	}

	dramas, err := st.ListDramas(ctx)
	if err != nil {
		t.Fatalf("ListDramas: %v", err)
	}
	if len(dramas) != len(refs) {
		t.Fatalf("入库剧目 %d 部, 期望 = 枚举数 %d 部", len(dramas), len(refs))
	}

	wantEpisodes := map[string]int{bigWorkID: 76}
	bigVids := fixtureVidList(t, "page-series.html")
	totalEpisodes := 0
	for _, d := range dramas {
		want, ok := wantEpisodes[d.ProviderID]
		if !ok {
			want = 3 // page-series-small.html 的真实声明集数
		}
		eps, err := st.ListEpisodes(ctx, d.ID)
		if err != nil {
			t.Fatalf("ListEpisodes(%s): %v", d.ProviderID, err)
		}
		if d.EpisodeTotal != want {
			t.Errorf("剧 %s 的 episode_total = %d, 期望声明集数 %d", d.ProviderID, d.EpisodeTotal, want)
		}
		if len(eps) != want {
			t.Errorf("剧 %s 入库 %d 集, 期望声明集数 %d", d.ProviderID, len(eps), want)
		}
		for i, ep := range eps {
			if ep.EpNo != i+1 {
				t.Fatalf("剧 %s 的第 %d 行 ep_no = %d, 期望 %d（下标 + 1）", d.ProviderID, i+1, ep.EpNo, i+1)
			}
			if ep.Vid == "" {
				t.Fatalf("剧 %s 第 %d 集没有 vid（源侧集标识必须入库）", d.ProviderID, ep.EpNo)
			}
		}
		if d.ProviderID == bigWorkID && len(eps) == len(bigVids) {
			for i := range eps {
				if eps[i].Vid != bigVids[i] {
					t.Fatalf("剧 %s 第 %d 集 vid = %q, 期望 fixture 的下标 %d = %q",
						d.ProviderID, i+1, eps[i].Vid, i, bigVids[i])
				}
			}
		}
		totalEpisodes += len(eps)
	}
	if want := 76 + 8*3; totalEpisodes != want {
		t.Fatalf("入库集数合计 %d, 期望 %d（1 部 76 集 + 8 部 3 集）", totalEpisodes, want)
	}

	// 并集的落地证明：只在 /category 里出现的那两部也必须进库。
	onlyCategory := []string{"7694653385668037657"}
	for _, id := range onlyCategory {
		if _, err := st.DramaByProviderID(ctx, hongguo.Name, id); err != nil {
			t.Fatalf("只在 /category 里的 %s 没入库: %v", id, err)
		}
	}
	t.Logf("枚举 %d 部（sitemap 7 ∪ /category 3，交集 1）→ 入库 %d 部 / %d 集",
		len(refs), len(dramas), totalEpisodes)
}

// enumerateAllRefs 从空游标起逐页推进枚举，返回去重后的 refs（跨页去重归框架）。
func enumerateAllRefs(t *testing.T, p plugin.Plugin) []plugin.WorkRef {
	t.Helper()
	ctx := context.Background()
	var (
		cur  *plugin.Cursor
		refs []plugin.WorkRef
	)
	seen := make(map[string]struct{})
	for i := 0; ; i++ {
		if i > 50 {
			t.Fatal("枚举没有终止")
		}
		page, err := p.Enumerate(ctx, cur)
		if err != nil {
			t.Fatalf("第 %d 页 Enumerate: %v", i+1, err)
		}
		for _, ref := range page.Refs {
			if ref.Plugin != p.Name() {
				t.Fatalf("WorkRef.Plugin = %q, 期望 %q", ref.Plugin, p.Name())
			}
			if _, dup := seen[ref.ID]; dup {
				continue
			}
			seen[ref.ID] = struct{}{}
			refs = append(refs, ref)
		}
		if page.Next == nil {
			return refs
		}
		cur = page.Next
	}
}

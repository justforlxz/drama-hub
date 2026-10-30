package hongguo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// fixtureLoc 把 fixture 里的真实域名换成假站点地址。
//
// sitemap 索引给的是绝对 URL（真实行为，插件照 loc 取文件），所以 fixture 里必须
// 是真域名；测试把它整体换掉，让 --network=none 下也不会打到真站点。
func fixtureLoc(t *testing.T, baseURL, name string) string {
	t.Helper()
	body := readFixture(t, name)
	if !strings.Contains(body, DefaultBaseURL) {
		t.Fatalf("fixture %s 里没有 %s：loc 不是绝对地址，这条测试就证明不了「照 loc 取文件」",
			name, DefaultBaseURL)
	}
	return strings.ReplaceAll(body, DefaultBaseURL, baseURL)
}

// enumerateRoutes 是目录发现的假上游：索引 + 两份子 sitemap + seo + /category 两页。
func enumerateRoutes(t *testing.T, srv *httptest.Server) map[string]route {
	t.Helper()
	const (
		indexPath  = "/sitemap/hongguoduanju/index.xml"
		index1Path = "/sitemap/hongguoduanju/index1.xml"
		index2Path = "/sitemap/hongguoduanju/index2.xml"
	)
	return map[string]route{
		indexPath:            {http.StatusOK, fixtureLoc(t, srv.URL, "sitemap-index.xml")},
		index1Path:           {http.StatusOK, fixtureLoc(t, srv.URL, "sitemap-1.xml")},
		index2Path:           {http.StatusOK, fixtureLoc(t, srv.URL, "sitemap-2.xml")},
		"/seo-important.xml": {http.StatusOK, fixtureLoc(t, srv.URL, "sitemap-seo.xml")},
		"/category":          {http.StatusOK, readFixture(t, "category-page1.html")},
		"/category?page=2":   {http.StatusOK, readFixture(t, "category-page2.html")},
	}
}

// 逐页走完三段枚举：分别贴 sitemap 命中、/category 命中与去重后的并集。
//
// 这条用例是「枚举必须是并集」的守卫（docs/L1-CORPUS-SCOPE.md 取证 B2）：
// sitemap 段给出 7 部，/category 段给出 3 部，其中 1 部重合、2 部只在 /category
// 里出现——只用 sitemap 就会漏掉这 2 部。
func TestEnumerateUnion(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(enumerateRoutes(t, srv))
	p := newTestPlugin(t, srv)

	pages := enumerateAll(t, p)

	// 页序：子 sitemap 1 → 子 sitemap 2 → seo → /category 第 1 页 → 第 2 页（末页 Next==nil）。
	wantPages := [][]string{
		// 子 sitemap 1：/player 两条 + /detail 一条 + 重复一条 + 交集那条；/about 非剧目 loc 跳过。
		{"7619615344046378009", "7616334357598309401", "7683103397246946328", "7619615344046378009", "7701334722449787928"},
		// 子 sitemap 2：与上一份重复一条 + 两条新的。
		{"7683103397246946328", "7603667977744960574", "7605500183484320792"},
		// seo-important：与子 sitemap 重复一条 + 一条只在它里面。
		{"7605500183484320792", "7615465407347952664"},
		// /category 第 1 页：一条与 sitemap 交集 + 一条只在 /category。
		{"7701334722449787928", "7694653385668037657"},
		// /category 第 2 页：一条新的 + 一条页间重复。
		{"7693854781961423897", "7701334722449787928"},
	}
	if len(pages) != len(wantPages) {
		t.Fatalf("页数 = %d, 期望 %d（%v）", len(pages), len(wantPages), fake.requests())
	}
	for i, want := range wantPages {
		if got := idsOf(pages[i]); !reflect.DeepEqual(got, want) {
			t.Fatalf("第 %d 页 refs = %v, 期望 %v", i+1, got, want)
		}
	}
	for _, page := range pages {
		for _, ref := range page.Refs {
			if ref.Plugin != Name {
				t.Fatalf("WorkRef.Plugin = %q, 期望 %q（契约硬性要求 6）", ref.Plugin, Name)
			}
		}
	}
	if last := pages[len(pages)-1]; last.Next != nil {
		t.Fatalf("末页 Next = %+v, 期望 nil（枚举结束）", last.Next)
	}

	// 上一次游标一律只有 「v1:<段>:<位置>」三个片段，且相邻两页不同（框架的游标推进守卫）。
	for _, page := range pages {
		if page.Next == nil {
			continue
		}
		if got := strings.Count(page.Next.Token, ":"); got != 2 {
			t.Fatalf("游标 %q 片段数 = %d, 期望 2", page.Next.Token, got)
		}
	}

	// 上游请求序列：每份子 sitemap 之前重取一次索引（游标只记「第几份」）。
	wantRequests := []string{
		"/sitemap/hongguoduanju/index.xml",
		"/sitemap/hongguoduanju/index1.xml",
		"/sitemap/hongguoduanju/index.xml",
		"/sitemap/hongguoduanju/index2.xml",
		"/seo-important.xml",
		"/category",
		"/category?page=2",
	}
	if got := fake.requests(); !reflect.DeepEqual(got, wantRequests) {
		t.Fatalf("上游请求 = %v, 期望 %v", got, wantRequests)
	}

	// 并集：9 部 = sitemap 段 7 部 ∪ /category 段 3 部，交集 1 部。
	sitemapIDs := make(map[string]struct{})
	for _, page := range pages[:3] {
		for _, id := range idsOf(page) {
			sitemapIDs[id] = struct{}{}
		}
	}
	categoryIDs := make(map[string]struct{})
	for _, page := range pages[3:] {
		for _, id := range idsOf(page) {
			categoryIDs[id] = struct{}{}
		}
	}
	union := make(map[string]struct{}, len(sitemapIDs)+len(categoryIDs))
	for id := range sitemapIDs {
		union[id] = struct{}{}
	}
	for id := range categoryIDs {
		union[id] = struct{}{}
	}
	if len(sitemapIDs) != 7 || len(categoryIDs) != 3 || len(union) != 9 {
		t.Fatalf("sitemap %d 部 / category %d 部 / 并集 %d 部, 期望 7 / 3 / 9",
			len(sitemapIDs), len(categoryIDs), len(union))
	}
	onlyCategory := make([]string, 0, 2)
	for id := range categoryIDs {
		if _, ok := sitemapIDs[id]; !ok {
			onlyCategory = append(onlyCategory, id)
		}
	}
	if len(onlyCategory) != 2 {
		t.Fatalf("只在 /category 里的 = %v, 期望 2 部（只用 sitemap 会漏的正是这一批）", onlyCategory)
	}
	t.Logf("sitemap %d 部 + /category %d 部（交集 1 部）→ 并集 %d 部；只在 /category 的 2 部：%v",
		len(sitemapIDs), len(categoryIDs), len(union), onlyCategory)
}

// 游标续跑：从任意一页的游标起跑，只读该页及其后续，不回头重读前面的文档。
func TestEnumerateResumeFromCursor(t *testing.T) {
	cases := []struct {
		name     string
		token    string
		wantReqs []string
		wantIDs  []string
		wantNext string // 空 = 期望 Next == nil
	}{
		{
			name:     "子 sitemap 第 2 份",
			token:    "v1:s:1",
			wantReqs: []string{"/sitemap/hongguoduanju/index.xml", "/sitemap/hongguoduanju/index2.xml"},
			wantIDs:  []string{"7683103397246946328", "7603667977744960574", "7605500183484320792"},
			wantNext: "v1:e:0",
		},
		{
			name:     "seo 段",
			token:    "v1:e:0",
			wantReqs: []string{"/seo-important.xml"},
			wantIDs:  []string{"7605500183484320792", "7615465407347952664"},
			wantNext: "v1:c:1",
		},
		{
			name:     "/category 末页",
			token:    "v1:c:2",
			wantReqs: []string{"/category?page=2"},
			wantIDs:  []string{"7693854781961423897", "7701334722449787928"},
			wantNext: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, srv := newFakeSite(t, http.StatusOK, "")
			fake.setRoutes(enumerateRoutes(t, srv))
			p := newTestPlugin(t, srv)

			page, err := p.Enumerate(context.Background(), &plugin.Cursor{Token: tc.token})
			if err != nil {
				t.Fatalf("Enumerate(%s): %v", tc.token, err)
			}
			if got := fake.requests(); !reflect.DeepEqual(got, tc.wantReqs) {
				t.Fatalf("上游请求 = %v, 期望 %v（续跑不许回头重读）", got, tc.wantReqs)
			}
			if got := idsOf(page); !reflect.DeepEqual(got, tc.wantIDs) {
				t.Fatalf("refs = %v, 期望 %v", got, tc.wantIDs)
			}
			next := ""
			if page.Next != nil {
				next = page.Next.Token
			}
			if next != tc.wantNext {
				t.Fatalf("Next = %q, 期望 %q", next, tc.wantNext)
			}
		})
	}
}

// 游标被改坏（老版本、手改、截断）按 ErrSchema 打回，且不发任何上游请求：
// 静默「当作从头开始」会把「游标坏了」伪装成「枚举走完了」。
func TestEnumerateRejectsUnknownCursor(t *testing.T) {
	for _, token := range []string{
		"v0:s:0", "abc", "v1:x:0", "v1:s:-1", "v1:c:0", "v1:s", "v1:s:1:2",
	} {
		t.Run(token, func(t *testing.T) {
			fake, srv := newFakeSite(t, http.StatusOK, "")
			fake.setRoutes(enumerateRoutes(t, srv))
			p := newTestPlugin(t, srv)

			_, err := p.Enumerate(context.Background(), &plugin.Cursor{Token: token})
			if !errors.Is(err, plugin.ErrSchema) {
				t.Fatalf("err = %v, 期望 ErrSchema", err)
			}
			if reqs := fake.requests(); len(reqs) != 0 {
				t.Fatalf("游标非法却发了上游请求 %v", reqs)
			}
		})
	}
}

// 空 Token 与 nil 等价：框架用空串表示「上一轮走完了」，下一轮从第一份子 sitemap 重来。
func TestEnumerateAcceptsEmptyCursor(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(enumerateRoutes(t, srv))
	p := newTestPlugin(t, srv)

	page, err := p.Enumerate(context.Background(), &plugin.Cursor{})
	if err != nil {
		t.Fatalf("Enumerate(空游标): %v", err)
	}
	if got := idsOf(page); len(got) != 5 {
		t.Fatalf("refs = %d 项, 期望 5 项（第一份子 sitemap）", len(got))
	}
	if want := []string{"/sitemap/hongguoduanju/index.xml", "/sitemap/hongguoduanju/index1.xml"}; !reflect.DeepEqual(fake.requests(), want) {
		t.Fatalf("上游请求 = %v, 期望 %v", fake.requests(), want)
	}
}

// 子 sitemap 是 5 MiB 的「一份文件」，不是一页：读取上限必须容得下它。
// 实测 29 份子 sitemap 每份 5,242,7xx 字节，按页面级的 4 MiB 会全部被判成超限。
func TestEnumerateSitemapBodyLimit(t *testing.T) {
	const entries = 90000 // 单条 63 字节 → 约 5.4 MiB，与实测的一份子 sitemap 同量级
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for i := 0; i < entries; i++ {
		fmt.Fprintf(&body, "<url><loc>https://hongguoduanju.com/player/%d</loc></url>", 100000+i)
	}
	body.WriteString(`</urlset>`)
	sitemap := body.String()
	if len(sitemap) <= maxBodyBytes {
		t.Fatalf("构造的 sitemap 只有 %d 字节，不比页面闸 %d 大，测不出上限差异", len(sitemap), maxBodyBytes)
	}

	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(map[string]route{
		"/seo-important.xml": {http.StatusOK, sitemap},
	})
	p := newTestPlugin(t, srv)

	page, err := p.Enumerate(context.Background(), &plugin.Cursor{Token: "v1:e:0"})
	if err != nil {
		t.Fatalf("Enumerate(seo 段): %v", err)
	}
	if len(page.Refs) != entries {
		t.Fatalf("refs = %d 项, 期望 %d 项（整份 sitemap 一页返回）", len(page.Refs), entries)
	}
}

// 超限仍然按 ErrSchema（上限放大不等于不限量）。
func TestEnumerateSitemapOversize(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(map[string]route{
		"/seo-important.xml": {http.StatusOK, strings.Repeat("a", maxSitemapBytes+1)},
	})
	p := newTestPlugin(t, srv)

	_, err := p.Enumerate(context.Background(), &plugin.Cursor{Token: "v1:e:0"})
	if !errors.Is(err, plugin.ErrSchema) {
		t.Fatalf("err = %v, 期望 ErrSchema", err)
	}
}

// /category 的页码校验：上游忽略 ?page 参数（总返回第 1 页）时按 ErrSchema 打回，
// 而不是把同一页 800 部当成 34 页重复灌一遍。
func TestEnumerateCategoryPageMismatch(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(map[string]route{
		"/category?page=2": {http.StatusOK, readFixture(t, "category-page1.html")}, // 回的是第 1 页
	})
	p := newTestPlugin(t, srv)

	_, err := p.Enumerate(context.Background(), &plugin.Cursor{Token: "v1:c:2"})
	if !errors.Is(err, plugin.ErrSchema) {
		t.Fatalf("err = %v, 期望 ErrSchema", err)
	}
}

// sitemap 里一个剧目 loc 都认不出（上游把 /player 换成别的形状）按 ErrSchema：
// 静默返回空等于宣称「源里一部剧都没有」。
func TestParseSitemapIDsUnrecognized(t *testing.T) {
	_, err := parseSitemapIDs([]byte(`<urlset><url><loc>https://hongguoduanju.com/watch/123</loc></url></urlset>`))
	if !errors.Is(err, plugin.ErrSchema) {
		t.Fatalf("err = %v, 期望 ErrSchema", err)
	}
	// 空的 urlset 是合法状态（这份 sitemap 里没有剧），不是结构变更。
	ids, err := parseSitemapIDs([]byte(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"></urlset>`))
	if err != nil || len(ids) != 0 {
		t.Fatalf("空 urlset → ids=%v err=%v, 期望空且无错误", ids, err)
	}
	// 换错文档类型（把索引页当 urlset 解）按 ErrSchema。
	if _, err := parseSitemapIDs([]byte(readFixture(t, "sitemap-index.xml"))); !errors.Is(err, plugin.ErrSchema) {
		t.Fatalf("把索引当 urlset 解 err = %v, 期望 ErrSchema", err)
	}
	if _, err := parseSitemapIndex([]byte(readFixture(t, "sitemap-1.xml"))); !errors.Is(err, plugin.ErrSchema) {
		t.Fatalf("把 urlset 当索引解 err = %v, 期望 ErrSchema", err)
	}
	// 空索引：0 份子 sitemap 与「源可枚举 608,372 部」冲突，不能当成「源里没有作品」。
	if _, err := parseSitemapIndex([]byte(`<sitemapindex></sitemapindex>`)); !errors.Is(err, plugin.ErrSchema) {
		t.Fatalf("空索引 err = %v, 期望 ErrSchema", err)
	}
}

// 相对 loc 也要能取（上游若改成相对地址，不该整轮枚举崩在拼接上）。
func TestGetSitemapResolvesRelativeLoc(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(map[string]route{
		"/sitemap/hongguoduanju/index.xml":  {http.StatusOK, `<sitemapindex><sitemap><loc>/sitemap/hongguoduanju/index1.xml</loc></sitemap></sitemapindex>`},
		"/sitemap/hongguoduanju/index1.xml": {http.StatusOK, readFixture(t, "sitemap-1.xml")},
	})
	p := newTestPlugin(t, srv)

	page, err := p.Enumerate(context.Background(), nil)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(page.Refs) != 5 {
		t.Fatalf("refs = %d 项, 期望 5 项", len(page.Refs))
	}
}

// enumerateAll 从零游标起逐页走完，返回每页（含末页）。
func enumerateAll(t *testing.T, p *Plugin) []plugin.Page {
	t.Helper()
	var (
		pages []plugin.Page
		cur   *plugin.Cursor
	)
	for i := 0; ; i++ {
		if i > 20 {
			t.Fatalf("枚举没有终止：已取 %d 页", i)
		}
		page, err := p.Enumerate(context.Background(), cur)
		if err != nil {
			t.Fatalf("第 %d 页 Enumerate: %v", i+1, err)
		}
		pages = append(pages, page)
		if page.Next == nil {
			return pages
		}
		cur = page.Next
	}
}

// idsOf 取一页 refs 的 ID，用于与 fixture 里的顺序逐项对照。
func idsOf(page plugin.Page) []string {
	out := make([]string, 0, len(page.Refs))
	for _, ref := range page.Refs {
		out = append(out, ref.ID)
	}
	return out
}

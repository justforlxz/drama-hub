package hongguo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// fakeSite 是假官网：可替换的「路径 → 响应」回调 + 请求路径与请求头记录。
type fakeSite struct {
	mu      sync.Mutex
	paths   []string
	headers []http.Header
	respond func(path string) (int, string)
}

func (f *fakeSite) setReply(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = func(string) (int, string) { return status, body }
}

// route 是一条假上游应答。
type route struct {
	status int
	body   string
}

// setRoutes 按 RequestURI（含 query）逐条应答；未登记的返回 404（模拟上游的真实兜底）。
func (f *fakeSite) setRoutes(routes map[string]route) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = func(path string) (int, string) {
		if r, ok := routes[path]; ok {
			return r.status, r.body
		}
		return http.StatusNotFound, ""
	}
}

func (f *fakeSite) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// headerList 返回收到的全部请求头（克隆，按到达顺序）。
func (f *fakeSite) headerList() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]http.Header, len(f.headers))
	copy(out, f.headers)
	return out
}

// newFakeSite 起一个 httptest 假官网，初始响应固定为 status + body。
func newFakeSite(t *testing.T, status int, body string) (*fakeSite, *httptest.Server) {
	t.Helper()
	f := &fakeSite{}
	f.setReply(status, body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 记录 RequestURI 而不是 Path：目录发现要按 ?page=N 区分 /category 的每一页，
		// 路径相同而页码不同，只记 Path 就断言不出「按页推进」。
		uri := r.URL.RequestURI()
		f.mu.Lock()
		f.paths = append(f.paths, uri)
		f.headers = append(f.headers, r.Header.Clone())
		respond := f.respond
		f.mu.Unlock()
		st, b := respond(uri)
		w.WriteHeader(st)
		_, _ = io.WriteString(w, b)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// newTestPlugin 构造指向假官网的插件，日志丢弃。
func newTestPlugin(t *testing.T, srv *httptest.Server) *Plugin {
	t.Helper()
	return New(Config{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func testRef() plugin.WorkRef { return plugin.WorkRef{Plugin: Name, ID: fixtureSID} }

// Work 一次请求拿到剧目元数据与完整集表（下标即集号）。
func TestWorkDetail(t *testing.T) {
	vids := fixtureVidList(t, "page-series.html")
	fake, srv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series.html"))
	p := newTestPlugin(t, srv)

	got, err := p.Work(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if reqs := fake.requests(); len(reqs) != 1 || reqs[0] != "/player/"+fixtureSID {
		t.Fatalf("上游请求 = %v, 期望恰一次 /player/%s", reqs, fixtureSID)
	}
	w := got.Work
	if w.Ref != testRef() || w.Title != fixtureName || w.Plot != fixtureIntro || w.CoverURL != fixtureCover {
		t.Fatalf("剧目字段不符：ref=%+v title=%q plot=%q cover=%q", w.Ref, w.Title, w.Plot, w.CoverURL)
	}
	if w.Year != 2025 {
		t.Fatalf("year = %d, 期望 2025（first_visible_time 推出）", w.Year)
	}
	if w.Genres != nil {
		t.Fatalf("Genres = %v, 页面没有 tags 时必须是 nil（= 未声明）", w.Genres)
	}
	if w.EpisodeDecl != len(vids) {
		t.Fatalf("episode_decl = %d, 期望 %d", w.EpisodeDecl, len(vids))
	}
	if len(got.Episodes) != len(vids) {
		t.Fatalf("集表 %d 项, 期望 %d", len(got.Episodes), len(vids))
	}
	for i, ep := range got.Episodes {
		if ep.No != i+1 {
			t.Fatalf("episodes[%d].No = %d, 期望 %d", i, ep.No, i+1)
		}
		// 源侧批量拿不到标题与时长，契约要求留零值而不是编造。
		if ep.Title != "" || ep.DurationSec != 0 {
			t.Fatalf("episodes[%d] 编造了标题/时长：%+v", i, ep)
		}
	}
}

// Genres 取上游 seriesDetail.tags 并保序（Genres[0] 是主类型）。
//
// fixture 是裁剪页面，不含 tags，所以在 seriesDetail 里注入一段固定标签来断言顺序；
// 上游真的不给 tags 时 Genres 必须是 nil（= 未声明），不能编造。
func TestWorkGenresFromTags(t *testing.T) {
	base := readFixture(t, "page-series-small.html")
	if strings.Contains(base, `"tags"`) {
		t.Fatal("fixture 已含 tags：注入逻辑需要同步改写")
	}
	page := strings.Replace(base, `"seriesDetail":{`, `"seriesDetail":{"tags":["都市","逆袭"],`, 1)
	if page == base {
		t.Fatal("fixture 里没有 seriesDetail，无法注入 tags")
	}
	_, srv := newFakeSite(t, http.StatusOK, page)
	p := newTestPlugin(t, srv)

	got, err := p.Work(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	want := []string{"都市", "逆袭"}
	if strings.Join(got.Work.Genres, ",") != strings.Join(want, ",") {
		t.Fatalf("Genres = %v, 期望 %v（保序，Genres[0] 是主类型）", got.Work.Genres, want)
	}
}

// WorkWithEpisodeKeys 一次调用同时给详情与每集的源侧集标识（红果的 vid）。
func TestWorkWithEpisodeKeys(t *testing.T) {
	vids := fixtureVidList(t, "page-series.html")
	_, srv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series.html"))
	p := newTestPlugin(t, srv)

	detail, keys, err := p.WorkWithEpisodeKeys(context.Background(), testRef())
	if err != nil {
		t.Fatalf("WorkWithEpisodeKeys: %v", err)
	}
	if len(keys) != len(vids) || len(detail.Episodes) != len(vids) {
		t.Fatalf("keys %d 项 / 集表 %d 项, 期望各 %d 项", len(keys), len(detail.Episodes), len(vids))
	}
	for i := range vids {
		if keys[i] != vids[i] {
			t.Fatalf("keys[%d] = %q, 期望 %q", i, keys[i], vids[i])
		}
	}
}

// Resolve 用集号换直链：详情页 + 集页两个请求，返回的 URL 规整过。
func TestResolveFirstEpisode(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(map[string]route{
		"/player/" + fixtureSID:                    {http.StatusOK, readFixture(t, "page-series.html")},
		"/player/" + fixtureSID + "/" + fixtureVID: {http.StatusOK, readFixture(t, "page-episode.html")},
	})
	p := newTestPlugin(t, srv)

	got, err := p.Resolve(context.Background(), testRef(), 1)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := DefaultBaseURL + "/video/public/" + fixtureVID + "/360p.mp4"
	if got.URL != want {
		t.Fatalf("直链 = %q, 期望 %q", got.URL, want)
	}
	if !got.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt = %v, 上游没给过期时间就该是零值", got.ExpiresAt)
	}
	// 官网通道是明文 MP4：Key 必须是 nil（nil = 明文），不能给空切片或随手造 key。
	if got.Key != nil {
		t.Fatalf("Key = %x, 期望 nil（官网通道是明文）", got.Key)
	}
	reqs := fake.requests()
	if len(reqs) != 2 || reqs[0] != "/player/"+fixtureSID || reqs[1] != "/player/"+fixtureSID+"/"+fixtureVID {
		t.Fatalf("上游请求 = %v, 期望 详情页 → 集页 各一次", reqs)
	}
}

// epNo 超出集数 → ErrNotFound，且不再发集页请求。
func TestResolveEpisodeOutOfRange(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series-small.html"))
	p := newTestPlugin(t, srv)

	_, err := p.Resolve(context.Background(), testRef(), 99)
	if !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("err = %v, 期望 ErrNotFound", err)
	}
	if reqs := fake.requests(); len(reqs) != 1 {
		t.Fatalf("上游请求 = %v, 期望只有详情页一次", reqs)
	}
}

// Health 取一次目录索引：200 即通过，失败是 ErrUpstream。
func TestHealth(t *testing.T) {
	_, okSrv := newFakeSite(t, http.StatusOK, "<sitemapindex/>")
	if err := newTestPlugin(t, okSrv).Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	_, badSrv := newFakeSite(t, http.StatusInternalServerError, "boom")
	if err := newTestPlugin(t, badSrv).Health(context.Background()); !errors.Is(err, plugin.ErrUpstream) {
		t.Fatalf("err = %v, 期望 ErrUpstream", err)
	}
}

// 契约错误语义表的「触发条件」逐行落到哨兵上：一行一个子测试。
//
// 这张表是 S2 的验收对象（docs/PLUGIN-CONTRACT.md「错误语义」）：
// 429/403 → ErrRejected、5xx/传输失败/空响应 → ErrUpstream、结构不符 → ErrSchema、
// 作品 404 → ErrNotFound、集页 404 或 contentUrl 缺失 → ErrNoStream。
func TestSentinelMapping(t *testing.T) {
	const detailPath = "/player/" + fixtureSID
	episodePath := "/player/" + fixtureSID + "/" + fixtureVID

	cases := []struct {
		sentinel error
		name     string
		routes   map[string]route
		call     func(t *testing.T, p *Plugin) error
	}{
		{
			sentinel: plugin.ErrUpstream,
			name:     "5xx",
			routes:   map[string]route{detailPath: {http.StatusBadGateway, "bad gateway"}},
			call:     workCall,
		},
		{
			sentinel: plugin.ErrUpstream,
			name:     "空响应体",
			routes:   map[string]route{detailPath: {http.StatusOK, ""}},
			call:     workCall,
		},
		{
			sentinel: plugin.ErrRejected,
			name:     "429 限流",
			routes:   map[string]route{detailPath: {http.StatusTooManyRequests, "too many requests"}},
			call:     workCall,
		},
		{
			sentinel: plugin.ErrRejected,
			name:     "403 风控",
			routes:   map[string]route{detailPath: {http.StatusForbidden, "forbidden"}},
			call:     workCall,
		},
		{
			// 444 是上游丢连接（限流），不是「这集不可播」也不是「这个作品不存在」：
			// 实测全量灌库约 1 万次 /player 后源侧开始全量回 444（判据见 statusDropped 注释）。
			sentinel: plugin.ErrRejected,
			name:     "444 限流丢连接",
			routes:   map[string]route{detailPath: {statusDropped, ""}},
			call:     workCall,
		},
		{
			sentinel: plugin.ErrSchema,
			name:     "页面身份不符",
			routes:   map[string]route{detailPath: {http.StatusOK, readFixture(t, "page-wrong-series.html")}},
			call:     workCall,
		},
		{
			sentinel: plugin.ErrSchema,
			name:     "响应体超 4 MiB",
			routes:   map[string]route{detailPath: {http.StatusOK, strings.Repeat("a", maxBodyBytes+1)}},
			call:     workCall,
		},
		{
			sentinel: plugin.ErrNotFound,
			name:     "作品 404",
			routes:   map[string]route{detailPath: {http.StatusNotFound, ""}},
			call:     workCall,
		},
		{
			sentinel: plugin.ErrNoStream,
			name:     "集页 404",
			routes: map[string]route{
				detailPath:  {http.StatusOK, readFixture(t, "page-series.html")},
				episodePath: {http.StatusNotFound, ""},
			},
			call: resolveCall,
		},
		{
			sentinel: plugin.ErrNoStream,
			name:     "VideoObject 无 contentUrl",
			routes: map[string]route{
				detailPath:  {http.StatusOK, readFixture(t, "page-series.html")},
				episodePath: {http.StatusOK, readFixture(t, "page-episode-no-content.html")},
			},
			call: resolveCall,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, srv := newFakeSite(t, http.StatusOK, "")
			fake.setRoutes(tc.routes)
			err := tc.call(t, newTestPlugin(t, srv))
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("err = %v, 期望 %v", err, tc.sentinel)
			}
			// 错误类别必须唯一：混了另一个哨兵就等于给框架两条互相矛盾的动作指令。
			for _, other := range []error{plugin.ErrUpstream, plugin.ErrRejected,
				plugin.ErrSchema, plugin.ErrNotFound, plugin.ErrNoStream} {
				if other != tc.sentinel && errors.Is(err, other) {
					t.Fatalf("err = %v 同时命中 %v", err, other)
				}
			}
		})
	}
}

func workCall(t *testing.T, p *Plugin) error {
	t.Helper()
	_, err := p.Work(context.Background(), testRef())
	return err
}

func resolveCall(t *testing.T, p *Plugin) error {
	t.Helper()
	_, err := p.Resolve(context.Background(), testRef(), 1)
	return err
}

// TestOutboundHeaders 覆盖 MKACG-624 AC4：每次出站都带浏览器身份，且 Referer
// 跟着配置的 base_url 走（换成假站点也仍然指向本站根）。
//
// 判据是「每一条出站请求都带」：漏在一次 sitemap / 集页请求上，那条请求就是
// Go-http-client/1.1，等于没做（源侧风控按请求看，不按平均看）。
func TestOutboundHeaders(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series.html"))
	p := newTestPlugin(t, srv)

	if _, err := p.Work(context.Background(), testRef()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}

	headers := fake.headerList()
	if len(headers) != 2 {
		t.Fatalf("出站请求数 = %d, 期望 2（Work + Health）", len(headers))
	}
	wantReferer := srv.URL + "/"
	for i, h := range headers {
		if got := h.Get("User-Agent"); got != outboundUA {
			t.Errorf("第 %d 个请求 User-Agent = %q, 期望浏览器 UA", i, got)
		}
		if got := h.Get("Accept"); got != outboundAccept {
			t.Errorf("第 %d 个请求 Accept = %q", i, got)
		}
		if got := h.Get("Accept-Language"); got != outboundAcceptLanguage {
			t.Errorf("第 %d 个请求 Accept-Language = %q", i, got)
		}
		if got := h.Get("Referer"); got != wantReferer {
			t.Errorf("第 %d 个请求 Referer = %q, 期望 %q（跟随 base_url）", i, got, wantReferer)
		}
	}
}

// TestPlaybackHeaderLines 钉住给 ffmpeg 的那一份身份：顺序固定、只含 UA 与
// Referer（直链是媒体请求，页面那套 Accept 不适用），base_url 为空时回落到
// 内置默认地址。
func TestPlaybackHeaderLines(t *testing.T) {
	got := PlaybackHeaderLines("https://mirror.example/base/")
	want := []string{
		"User-Agent: " + outboundUA,
		"Referer: https://mirror.example/base/",
	}
	if len(got) != len(want) {
		t.Fatalf("PlaybackHeaderLines = %q, 期望 %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PlaybackHeaderLines[%d] = %q, 期望 %q", i, got[i], want[i])
		}
	}

	for _, base := range []string{"", DefaultBaseURL} {
		if lines := PlaybackHeaderLines(base); lines[1] != "Referer: "+DefaultBaseURL+"/" {
			t.Errorf("base_url = %q 时 Referer = %q, 期望内置默认地址", base, lines[1])
		}
	}
	// 不返回共享切片：调用方（装配根）改了不该污染别的实例。
	if a, b := PlaybackHeaderLines(""), PlaybackHeaderLines(""); &a[0] == &b[0] {
		t.Error("PlaybackHeaderLines 返回了同一个底层数组")
	}
}

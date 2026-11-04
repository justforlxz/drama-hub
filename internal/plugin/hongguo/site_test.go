package hongguo

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// legacyPlayInfoBody 造一份站点播放接口的成功答复（实测形态：main_url 明文直链，
// width/height 是字符串，duration 是数字，base_resp 的 StatusCode 是大写 S）。
func legacyPlayInfoBody(mainURL string) string {
	return `{"legacy_play_info":{"main_url":"` + mainURL +
		`","width":"1280","height":"720","duration":72.122},"base_resp":{"StatusCode":0,"StatusMessage":""}}`
}

// newSiteTestPlugin 构造一个「网页通道与站点播放接口都指向 srv、App 通道不可达」的
// 插件：任何误走 App 的情况都会立刻失败，而不是静默通过（测试规范：不碰外网）。
func newSiteTestPlugin(t *testing.T, srv *httptest.Server, logs *logSink, cookie func(context.Context) string) *Plugin {
	t.Helper()
	return New(Config{
		BaseURL:           srv.URL,
		HTTPClient:        srv.Client(),
		Logger:            slog.New(slog.NewTextHandler(logs, nil)),
		DeviceRegisterURL: "http://127.0.0.1:1/service/2/device_register/",
		AppVideoURL:       "http://127.0.0.1:1/video",
		Cookie:            cookie,
	})
}

// 受保护剧的集走站点播放接口：一次 POST 换 legacy_play_info 明文直链，不带 key。
func TestResolveSitePlainStream(t *testing.T) {
	const media = "https://v26-hgweb.qznovelvod.com/x/y.mp4?a=1&ch=0"
	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(map[string]route{
		"/player/" + fixtureSID: {http.StatusOK, readFixture(t, "page-series-protected.html")},
		sitePlayInfoPath:        {http.StatusOK, legacyPlayInfoBody(media)},
	})
	logs := &logSink{}
	p := newSiteTestPlugin(t, srv, logs, nil)

	got, err := p.Resolve(context.Background(), testRef(), 1)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.URL != media {
		t.Fatalf("直链 = %q, 期望 %q", got.URL, media)
	}
	// legacy_play_info 是明文：Key 必须是 nil（nil = 明文），不能给空切片。
	if got.Key != nil {
		t.Fatalf("Key = %x, 期望 nil（legacy_play_info 是明文）", got.Key)
	}

	// 请求顺序：详情页取集表 → 站点播放接口。不请求集页（受保护剧只有预览）。
	reqs := fake.requests()
	if len(reqs) != 2 || reqs[0] != "/player/"+fixtureSID || reqs[1] != sitePlayInfoPath {
		t.Fatalf("上游请求 = %v, 期望 详情页 → %s", reqs, sitePlayInfoPath)
	}

	// 请求体就是这一集的 (series_id, item_id, definition)。
	bodies := fake.bodyList()
	if len(bodies) != 2 {
		t.Fatalf("请求体条数 = %d, 期望 2", len(bodies))
	}
	var sent sitePlayInfoRequest
	if err := json.Unmarshal([]byte(bodies[1]), &sent); err != nil {
		t.Fatalf("播放接口请求体不是 JSON（%v）：%q", err, bodies[1])
	}
	want := sitePlayInfoRequest{SeriesID: fixtureSID, ItemID: fixtureVID, Definition: siteDefinition}
	if sent != want {
		t.Fatalf("播放接口请求体 = %+v, 期望 %+v", sent, want)
	}

	headers := fake.headerList()
	if ct := headers[1].Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, 期望 application/json", ct)
	}
	// 未配置登录态就是匿名：不许凭空造一个 Cookie 头。
	if ck := headers[1].Get("Cookie"); ck != "" {
		t.Errorf("未配置登录态却发了 Cookie = %q", ck)
	}
	// 契约：每个出站请求都要带浏览器身份（UA + Referer）。
	if ua := headers[1].Get("User-Agent"); ua == "" {
		t.Error("播放接口请求没带 User-Agent")
	}
	if ref := headers[1].Get("Referer"); ref == "" {
		t.Error("播放接口请求没带 Referer")
	}

	// 明文直链是带签名的播放地址：MUST NOT 进日志。
	assertNotLogged(t, logs.text(), map[string]string{"片源地址": media})
}

// 锁定集：站点回 403 + protected_playback_unavailable → ErrLoginRequired。
//
// 这是「配置问题」不是「源坏了」：必须能与 ErrUpstream 区分，否则页面只会显示 502。
func TestResolveSiteLockedEpisodeNeedsLogin(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(map[string]route{
		"/player/" + fixtureSID: {http.StatusOK, readFixture(t, "page-series-protected.html")},
		sitePlayInfoPath:        {http.StatusForbidden, `{"error":"protected_playback_unavailable"}`},
	})
	logs := &logSink{}
	p := newSiteTestPlugin(t, srv, logs, nil)

	_, err := p.Resolve(context.Background(), testRef(), 1)
	if !errors.Is(err, plugin.ErrLoginRequired) {
		t.Fatalf("err = %v, 期望 ErrLoginRequired", err)
	}
	if errors.Is(err, plugin.ErrUpstream) {
		t.Fatalf("登录态缺失被折成了上游失败：%v", err)
	}
	// 页面要能直接读到「该怎么办」，而不是一个裸答复文。
	if !strings.Contains(err.Error(), "登录") {
		t.Errorf("错误文本没有说明需要登录态：%v", err)
	}
	// 确定答复不回落 App 通道（App 端点不可达，回落就会得到 ErrUpstream）。
	if reqs := fake.requests(); len(reqs) != 2 {
		t.Fatalf("上游请求 = %v, 期望 详情页 + 播放接口 两条（不回落 App）", reqs)
	}
}

// 站点偶尔把「未登录」回成 200 + error：判定不能只看状态码（MKACG-635 的同一条原则）。
func TestResolveSiteLoginErrorAt200(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(map[string]route{
		"/player/" + fixtureSID: {http.StatusOK, readFixture(t, "page-series-protected.html")},
		sitePlayInfoPath:        {http.StatusOK, `{"error":"protected_playback_unavailable"}`},
	})
	p := newSiteTestPlugin(t, srv, &logSink{}, nil)

	_, err := p.Resolve(context.Background(), testRef(), 1)
	if !errors.Is(err, plugin.ErrLoginRequired) {
		t.Fatalf("err = %v, 期望 ErrLoginRequired", err)
	}
}

// 配置了登录态：只在「站点明确说要登录」之后才带，且只发给站点播放接口。
//
// MKACG-676 的口径是「Cookie 只作最后兜底」：先匿名要一次，回
// protected_playback_unavailable 才带登录态重试；页面请求永远不带。
func TestResolveSiteSendsConfiguredCookieOnRetry(t *testing.T) {
	const cookie = "sid=" + fixtureSID + "; uid=42; device=web"
	const media = "https://v26-hgweb.qznovelvod.com/x/y.mp4?a=1"
	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(map[string]route{
		"/player/" + fixtureSID: {http.StatusOK, readFixture(t, "page-series-protected.html")},
	})
	// 匿名请求被拒，带登录态的请求才给明文直链。
	var playInfo int
	fake.mu.Lock()
	fake.respond = func(path string) (int, string) {
		if path == "/player/"+fixtureSID {
			return http.StatusOK, readFixture(t, "page-series-protected.html")
		}
		if path != sitePlayInfoPath {
			return http.StatusNotFound, ""
		}
		playInfo++
		if playInfo == 1 {
			return http.StatusForbidden, `{"error":"protected_playback_unavailable"}`
		}
		return http.StatusOK, legacyPlayInfoBody(media)
	}
	fake.mu.Unlock()

	logs := &logSink{}
	p := newSiteTestPlugin(t, srv, logs, func(context.Context) string { return cookie })

	st, err := p.Resolve(context.Background(), testRef(), 1)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if st.URL != media {
		t.Fatalf("直链 = %q, 期望 %q", st.URL, media)
	}
	headers := fake.headerList()
	if len(headers) != 3 {
		t.Fatalf("请求条数 = %d, 期望 详情页 + 匿名播放接口 + 带登录播放接口", len(headers))
	}
	if ck := headers[0].Get("Cookie"); ck != "" {
		t.Errorf("详情页请求不该带登录 Cookie，实际 = %q", ck)
	}
	if ck := headers[1].Get("Cookie"); ck != "" {
		t.Errorf("首次播放接口请求是匿名的，不该带 Cookie，实际 = %q", ck)
	}
	if ck := headers[2].Get("Cookie"); ck != cookie {
		t.Errorf("重试请求 Cookie = %q, 期望带上配置值", ck)
	}
	// 日志只记「带没带」，不记内容。
	if !strings.Contains(logs.text(), "login=true") || !strings.Contains(logs.text(), "login=false") {
		t.Errorf("日志里看不到 login=true/false：%s", logs.text())
	}
	assertNotLogged(t, logs.text(), map[string]string{
		"登录 Cookie": cookie,
		"Cookie 片段": "7610609482288073753",
	})
}

// 站点一直说「要登录」但没配登录态：直接报需登录，不是 502。
func TestResolveSiteLockedWithoutCookieStaysLoginRequired(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(map[string]route{
		"/player/" + fixtureSID: {http.StatusOK, readFixture(t, "page-series-protected.html")},
		sitePlayInfoPath:        {http.StatusForbidden, `{"error":"protected_playback_unavailable"}`},
	})
	p := newSiteTestPlugin(t, srv, &logSink{}, nil)

	_, err := p.Resolve(context.Background(), testRef(), 1)
	if !errors.Is(err, plugin.ErrLoginRequired) {
		t.Fatalf("err = %v, 期望 ErrLoginRequired", err)
	}
	if got := len(fake.headerList()); got != 2 {
		t.Fatalf("请求条数 = %d, 没配 Cookie 就不该重试", got)
	}
}

// 站点只给 DASH-CENC（kid + player_access_key）：拿不到 16 字节 key 就明确报不支持。
func TestResolveSiteEncryptedStreamUnsupported(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, "")
	fake.setRoutes(map[string]route{
		"/player/" + fixtureSID: {http.StatusOK, readFixture(t, "page-series-protected.html")},
		sitePlayInfoPath: {http.StatusOK, `{"play_info":{"kid":"0123456789abcdef",` +
			`"player_access_key":"pa-key"},"base_resp":{"StatusCode":0,"StatusMessage":""}}`},
	})
	p := newSiteTestPlugin(t, srv, &logSink{}, nil)

	_, err := p.Resolve(context.Background(), testRef(), 1)
	if !errors.Is(err, plugin.ErrUnsupported) {
		t.Fatalf("err = %v, 期望 ErrUnsupported", err)
	}
	if errors.Is(err, plugin.ErrUpstream) {
		t.Fatalf("加密流被折成了上游失败：%v", err)
	}
	if !strings.Contains(err.Error(), "加密流") {
		t.Errorf("错误文本没有说明是加密流：%v", err)
	}
	// 确定答复不回落 App 通道。
	if reqs := fake.requests(); len(reqs) != 2 {
		t.Fatalf("上游请求 = %v, 期望两条（不回落 App）", reqs)
	}
}

// 未受保护的剧集页取不到直链（404）时落到站点播放接口，而不是直接走 App。
func TestResolveFallsBackToSiteWhenWebHasNoStream(t *testing.T) {
	const media = "https://v26-hgweb.qznovelvod.com/x/z.mp4?a=1"
	fake, srv := newFakeSite(t, http.StatusOK, "")
	// 集页不登记 → 404 → 网页通道 ErrNoStream。
	fake.setRoutes(map[string]route{
		"/player/" + fixtureSID: {http.StatusOK, readFixture(t, "page-series.html")},
		sitePlayInfoPath:        {http.StatusOK, legacyPlayInfoBody(media)},
	})
	p := newSiteTestPlugin(t, srv, &logSink{}, nil)

	got, err := p.Resolve(context.Background(), testRef(), 1)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.URL != media {
		t.Fatalf("直链 = %q, 期望 %q", got.URL, media)
	}
	reqs := fake.requests()
	want := []string{"/player/" + fixtureSID, "/player/" + fixtureSID + "/" + fixtureVID, sitePlayInfoPath}
	if len(reqs) != len(want) {
		t.Fatalf("上游请求 = %v, 期望 %v", reqs, want)
	}
	for i := range want {
		if reqs[i] != want[i] {
			t.Fatalf("上游请求 = %v, 期望 %v", reqs, want)
		}
	}
}

// parseSitePlayInfo 的判定顺序（纯函数，不碰网络）。
func TestParseSitePlayInfo(t *testing.T) {
	const media = "https://v26-hgweb.qznovelvod.com/x/y.mp4?a=1"
	cases := []struct {
		name    string
		body    string
		wantURL string // 期望的 URL（成功用例）
		wantErr error  // 期望的哨兵（失败用例）
	}{
		{name: "明文直链", body: legacyPlayInfoBody(media), wantURL: media},
		{
			name:    "答复文优先于其它字段",
			body:    `{"legacy_play_info":{"main_url":"` + media + `"},"error":"protected_playback_unavailable"}`,
			wantErr: plugin.ErrLoginRequired,
		},
		{
			name:    "未知答复文算上游失败",
			body:    `{"error":"something_else"}`,
			wantErr: plugin.ErrUpstream,
		},
		{
			name:    "非零业务码算上游失败",
			body:    `{"base_resp":{"StatusCode":1001,"StatusMessage":"boom"}}`,
			wantErr: plugin.ErrUpstream,
		},
		{
			name:    "只有加密流",
			body:    `{"play_info":{"kid":"0123456789abcdef","player_access_key":"k"}}`,
			wantErr: plugin.ErrUnsupported,
		},
		{
			name:    "非 https 直链",
			body:    legacyPlayInfoBody("http://v26-hgweb.qznovelvod.com/x/y.mp4"),
			wantErr: plugin.ErrSchema,
		},
		{
			name:    "既无明文也无加密流",
			body:    `{"base_resp":{"StatusCode":0,"StatusMessage":""}}`,
			wantErr: plugin.ErrSchema,
		},
		{name: "不是 JSON", body: "<html>", wantErr: plugin.ErrSchema},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseSitePlayInfo([]byte(c.body))
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, 期望 %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSitePlayInfo: %v", err)
			}
			if got.URL != c.wantURL {
				t.Fatalf("URL = %q, 期望 %q", got.URL, c.wantURL)
			}
			if got.Key != nil {
				t.Fatalf("Key = %x, 期望 nil", got.Key)
			}
		})
	}
}

package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

// fakeClock 提供可推进的固定时钟：sync_at 的「第二次值」必须可断言，而不是靠
// 真实时间（测试规范禁止依赖真实时间）。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// fakeSite 是假官网：可替换的「路径 → 响应」回调 + 请求路径记录。
type fakeSite struct {
	mu      sync.Mutex
	paths   []string
	respond func(path string) (int, string)
}

func (f *fakeSite) setReply(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = func(string) (int, string) { return status, body }
}

func (f *fakeSite) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// newFakeSite 起一个 httptest 假官网，初始响应固定为 status + body。
func newFakeSite(t *testing.T, status int, body string) (*fakeSite, *httptest.Server) {
	t.Helper()
	f := &fakeSite{}
	f.setReply(status, body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		respond := f.respond
		f.mu.Unlock()
		st, b := respond(r.URL.Path)
		w.WriteHeader(st)
		_, _ = io.WriteString(w, b)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// newTestSource 构造指向假官网的 Source，日志丢弃，时钟可控。
func newTestSource(t *testing.T, srv *httptest.Server, clock *fakeClock) *Source {
	t.Helper()
	s := NewSource(srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.BaseURL = srv.URL
	s.now = clock.now
	return s
}

// newTestStore 建临时 SQLite 库并迁移到最新。
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "provider.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return store.NewStore(db.DB)
}

// dbCounts 用 store 的导出 API 数出 (剧数, 集数)，用于断言「库内容不变」。
func dbCounts(t *testing.T, st *store.Store) (dramas, episodes int) {
	t.Helper()
	ctx := context.Background()
	ds, err := st.ListDramas(ctx)
	if err != nil {
		t.Fatalf("ListDramas: %v", err)
	}
	for _, d := range ds {
		eps, err := st.ListEpisodes(ctx, d.ID)
		if err != nil {
			t.Fatalf("ListEpisodes(%d): %v", d.ID, err)
		}
		episodes += len(eps)
	}
	return len(ds), episodes
}

// AC2 + AC8：一次上游请求、剧目字段映射、集齐全且 play_url 留空。
func TestSyncSeriesWritesDramaAndEpisodes(t *testing.T) {
	vids := fixtureVidList(t, "page-series.html")
	fake, srv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series.html"))
	st := newTestStore(t)
	clock := &fakeClock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)}
	s := newTestSource(t, srv, clock)
	ctx := context.Background()

	if err := s.SyncSeries(ctx, st, fixtureSID); err != nil {
		t.Fatalf("SyncSeries: %v", err)
	}

	// AC8：只发 1 次上游请求，且路径正确（不做 N+1 集页抓取）。
	if got := fake.requests(); len(got) != 1 || got[0] != "/player/"+fixtureSID {
		t.Fatalf("上游请求 = %v, 期望恰一次 /player/%s", got, fixtureSID)
	}

	d, err := st.DramaByProviderID(ctx, providerName, fixtureSID)
	if err != nil {
		t.Fatalf("DramaByProviderID: %v", err)
	}
	if d.Provider != providerName || d.ProviderID != fixtureSID {
		t.Fatalf("剧目身份 = (%q,%q), 期望 (%q,%q)", d.Provider, d.ProviderID, providerName, fixtureSID)
	}
	if d.Title != fixtureName {
		t.Fatalf("title = %q, 期望 %q", d.Title, fixtureName)
	}
	if d.Plot != fixtureIntro {
		t.Fatalf("plot 与 fixture 的 series_intro 不一致：%q", d.Plot)
	}
	if d.CoverURL != fixtureCover {
		t.Fatalf("cover_url = %q, 期望 %q", d.CoverURL, fixtureCover)
	}
	if d.EpisodeTotal != len(vids) {
		t.Fatalf("episode_total = %d, 期望 %d", d.EpisodeTotal, len(vids))
	}
	if d.Year != 2025 {
		t.Fatalf("year = %d, 期望 2025", d.Year)
	}
	syncAt, err := time.Parse(time.RFC3339, d.SyncAt)
	if err != nil {
		t.Fatalf("sync_at %q 不是 RFC3339: %v", d.SyncAt, err)
	}
	if syncAt.Location() != time.UTC || !syncAt.Equal(clock.t) {
		t.Fatalf("sync_at = %v, 期望注入时钟的 UTC 值 %v", syncAt, clock.t)
	}

	// AC8：集行数 == len(vid_list)，下标即集号，play_url / play_url_at 全空。
	eps, err := st.ListEpisodes(ctx, d.ID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(eps) != len(vids) {
		t.Fatalf("集数 = %d, 期望 %d", len(eps), len(vids))
	}
	for i, ep := range eps {
		if ep.EpNo != i+1 {
			t.Fatalf("eps[%d].EpNo = %d, 期望 %d（下标即集号）", i, ep.EpNo, i+1)
		}
		if ep.Vid != vids[i] {
			t.Fatalf("eps[%d].Vid = %q, 期望 vid_list[%d] = %q", i, ep.Vid, i, vids[i])
		}
		if ep.Source != providerName {
			t.Fatalf("eps[%d].Source = %q, 期望 %q", i, ep.Source, providerName)
		}
		if ep.PlayURL != "" || ep.PlayURLAt != "" {
			t.Fatalf("eps[%d] 写了 play_url：%q / %q（SSR 只有 10s 预览链，必须留空）", i, ep.PlayURL, ep.PlayURLAt)
		}
	}
}

// AC9：同 sid 重复同步幂等：行数与 id 不变，第二次的元数据与 sync_at 生效。
func TestSyncSeriesIdempotent(t *testing.T) {
	fixture := readFixture(t, "page-series.html")
	renamed := strings.Replace(fixture, fixtureName, fixtureName+"（重同步）", 1)
	if renamed == fixture {
		t.Fatal("fixture 里没找到剧名，无法构造第二次响应")
	}
	fake, srv := newFakeSite(t, http.StatusOK, fixture)
	st := newTestStore(t)
	clock := &fakeClock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)}
	s := newTestSource(t, srv, clock)
	ctx := context.Background()

	mustSync := func() (int64, []int64) {
		t.Helper()
		if err := s.SyncSeries(ctx, st, fixtureSID); err != nil {
			t.Fatalf("SyncSeries: %v", err)
		}
		d, err := st.DramaByProviderID(ctx, providerName, fixtureSID)
		if err != nil {
			t.Fatalf("DramaByProviderID: %v", err)
		}
		eps, err := st.ListEpisodes(ctx, d.ID)
		if err != nil {
			t.Fatalf("ListEpisodes: %v", err)
		}
		ids := make([]int64, len(eps))
		for i, ep := range eps {
			ids[i] = ep.ID
		}
		return d.ID, ids
	}

	dramaID1, epIDs1 := mustSync()
	clock.advance(time.Second)
	fake.setReply(http.StatusOK, renamed)
	dramaID2, epIDs2 := mustSync()

	if dramaID1 != dramaID2 {
		t.Fatalf("两次同步的 drama id 不同：%d vs %d", dramaID1, dramaID2)
	}
	if len(epIDs1) != len(epIDs2) {
		t.Fatalf("两次同步的集数不同：%d vs %d", len(epIDs1), len(epIDs2))
	}
	for i := range epIDs1 {
		if epIDs1[i] != epIDs2[i] {
			t.Fatalf("第 %d 集 id 变了：%d vs %d", i+1, epIDs1[i], epIDs2[i])
		}
	}
	if dramas, episodes := dbCounts(t, st); dramas != 1 || episodes != 76 {
		t.Fatalf("重同步后 (剧,集) = (%d,%d), 期望 (1,76)", dramas, episodes)
	}
	d, err := st.DramaByProviderID(ctx, providerName, fixtureSID)
	if err != nil {
		t.Fatalf("DramaByProviderID: %v", err)
	}
	if d.Title != fixtureName+"（重同步）" {
		t.Fatalf("title = %q, 期望第二次响应的剧名", d.Title)
	}
	if d.SyncAt != clock.t.Format(time.RFC3339) {
		t.Fatalf("sync_at = %q, 期望第二次同步的值 %q", d.SyncAt, clock.t.Format(time.RFC3339))
	}
}

// AC7：上游 502 / 空响应 → ErrUpstream 且库内容不变。
func TestSyncSeriesUpstreamErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"502", http.StatusBadGateway, "bad gateway"},
		{"200空响应", http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, srv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series.html"))
			st := newTestStore(t)
			s := newTestSource(t, srv, &fakeClock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)})
			ctx := context.Background()

			if err := s.SyncSeries(ctx, st, fixtureSID); err != nil {
				t.Fatalf("预备同步: %v", err)
			}
			beforeDramas, beforeEpisodes := dbCounts(t, st)

			fake.setReply(tc.status, tc.body)
			err := s.SyncSeries(ctx, st, fixtureSID)
			if !errors.Is(err, ErrUpstream) {
				t.Fatalf("err = %v, 期望 ErrUpstream", err)
			}
			afterDramas, afterEpisodes := dbCounts(t, st)
			if afterDramas != beforeDramas || afterEpisodes != beforeEpisodes {
				t.Fatalf("库变了：(剧,集) %d/%d → %d/%d", beforeDramas, beforeEpisodes, afterDramas, afterEpisodes)
			}
		})
	}
}

// AC5 + AC6：身份不符与页面结构变化 → ErrParse 且库内容不变。
func TestSyncSeriesParseErrors(t *testing.T) {
	cases := []struct{ name, fixture string }{
		{"身份不符", "page-wrong-series.html"},
		{"缺_ROUTER_DATA", "page-missing-router.html"},
		{"JSON截断", "page-truncated-json.html"},
		{"缺vid_list", "page-missing-vidlist.html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, srv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series.html"))
			st := newTestStore(t)
			s := newTestSource(t, srv, &fakeClock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)})
			ctx := context.Background()

			if err := s.SyncSeries(ctx, st, fixtureSID); err != nil {
				t.Fatalf("预备同步: %v", err)
			}
			beforeDramas, beforeEpisodes := dbCounts(t, st)

			fake.setReply(http.StatusOK, readFixture(t, tc.fixture))
			err := s.SyncSeries(ctx, st, fixtureSID)
			if !errors.Is(err, ErrParse) {
				t.Fatalf("err = %v, 期望 ErrParse", err)
			}
			afterDramas, afterEpisodes := dbCounts(t, st)
			if afterDramas != beforeDramas || afterEpisodes != beforeEpisodes {
				t.Fatalf("库变了：(剧,集) %d/%d → %d/%d", beforeDramas, beforeEpisodes, afterDramas, afterEpisodes)
			}
		})
	}
}

// 响应体超过 4 MiB 上限视为 ErrParse（页面异常放大，不是可重试的传输错误）。
func TestSyncSeriesRejectsOversizedBody(t *testing.T) {
	_, srv := newFakeSite(t, http.StatusOK, strings.Repeat("a", maxBodyBytes+1))
	st := newTestStore(t)
	s := newTestSource(t, srv, &fakeClock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)})

	err := s.SyncSeries(context.Background(), st, fixtureSID)
	if !errors.Is(err, ErrParse) {
		t.Fatalf("err = %v, 期望 ErrParse", err)
	}
	if dramas, episodes := dbCounts(t, st); dramas != 0 || episodes != 0 {
		t.Fatalf("超限响应后 (剧,集) = (%d,%d), 期望 (0,0)", dramas, episodes)
	}
}

// AC3（集成）：FetchDirectURL 走 HTTP 取单集页并解析出本集直链，不写库。
func TestFetchDirectURL(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, readFixture(t, "page-episode.html"))
	st := newTestStore(t)
	s := newTestSource(t, srv, &fakeClock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)})

	got, err := s.FetchDirectURL(context.Background(), fixtureSID, fixtureVID)
	if err != nil {
		t.Fatalf("FetchDirectURL: %v", err)
	}
	want := defaultBaseURL + "/video/public/" + fixtureVID + "/360p.mp4"
	if got != want {
		t.Fatalf("直链 = %q, 期望 %q", got, want)
	}
	if reqs := fake.requests(); len(reqs) != 1 || reqs[0] != "/player/"+fixtureSID+"/"+fixtureVID {
		t.Fatalf("上游请求 = %v, 期望恰一次单集页请求", reqs)
	}
	if dramas, episodes := dbCounts(t, st); dramas != 0 || episodes != 0 {
		t.Fatalf("FetchDirectURL 写了库：(剧,集) = (%d,%d)", dramas, episodes)
	}
}

// AC10（集成）：第 2..N 集的真实形态（VideoObject 在、contentUrl 缺）→ ErrNoDirectURL。
func TestFetchDirectURLNoContent(t *testing.T) {
	_, srv := newFakeSite(t, http.StatusOK, readFixture(t, "page-episode-no-content.html"))
	s := newTestSource(t, srv, &fakeClock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)})

	_, err := s.FetchDirectURL(context.Background(), fixtureSID, fixtureVID)
	if !errors.Is(err, ErrNoDirectURL) {
		t.Fatalf("err = %v, 期望 ErrNoDirectURL", err)
	}
}

// 单集页被 302 到别的集/别的剧页面时，绑定校验必须挡住（否则会把别的集直链当本集）。
func TestFetchDirectURLRejectsOtherSeries(t *testing.T) {
	_, srv := newFakeSite(t, http.StatusOK, readFixture(t, "page-episode-other-series.html"))
	s := newTestSource(t, srv, &fakeClock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)})

	_, err := s.FetchDirectURL(context.Background(), fixtureSID, fixtureVID)
	if !errors.Is(err, ErrParse) {
		t.Fatalf("err = %v, 期望 ErrParse", err)
	}
}

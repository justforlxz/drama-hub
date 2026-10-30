package sync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/plugin/hongguo"
	"github.com/justforlxz/drama-hub/internal/store"
)

// 真实页面 fixture 的身份（2026-10-29 抓取的《宴律，你的白月光回国了》）。
// 页面 fixture 归插件包（它们描述的是插件上游），本包只读同一份文件——
// 「迁移前后入库结果逐字段一致」必须用同一个 fixture 才成立。
const fixtureSID = "7503745441910017049"

const (
	fixtureName  = "宴律，你的白月光回国了"
	fixtureIntro = "七年前，顾星晚捅伤亲生父亲入狱；五年前，宴矜撑伞在监狱门口等她，却得知她早已出狱。时隔七年重逢，顾星晚望着律界大佬宴矜怔愣，对方却称不认识她，且早已订有婚约。顾星晚选择缄口不言，无人知晓，她曾在狱中为宴矜生下过一个孩子，过往的纠葛与未说出口的秘密，成了她心底最深的牵挂。"
	fixtureCover = "https://p3-novel.byteimg.com/novel-pic/34ad28f319c9c2b6598792ce2deaaaf8~tplv-shrink:640:0.image"
)

// readFixture 读插件包 testdata 下的页面 fixture。
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "plugin", "hongguo", "testdata", "html", name))
	if err != nil {
		t.Fatalf("读 fixture %s: %v", name, err)
	}
	return string(b)
}

// fixtureVidList 从 fixture 的原始 JSON 里独立取出 vid_list，作为入库结果的对照。
//
// 刻意不复用插件的解析：这段用最直白的正则 + json.Unmarshal，若被测链路丢项、
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

// newTestSyncer 构造「红果插件 + 同步编排」，台账走假官网，时钟可控。
func newTestSyncer(t *testing.T, srv *httptest.Server, clock *fakeClock) *Syncer {
	t.Helper()
	p := hongguo.New(hongguo.Config{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	s := New(p)
	s.now = clock.now
	return s
}

// newTestStore 建临时 SQLite 库并迁移到最新。
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "sync.db"))
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

// 入库字段与 1.0（internal/provider.SyncSeries）逐字段一致：一次上游请求、
// 剧目字段映射、集齐全（下标即集号、vid 保留）、play_url 留空。
func TestSyncSeriesWritesDramaAndEpisodes(t *testing.T) {
	vids := fixtureVidList(t, "page-series.html")
	fake, srv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series.html"))
	st := newTestStore(t)
	clock := &fakeClock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)}
	s := newTestSyncer(t, srv, clock)
	ctx := context.Background()

	if err := s.SyncSeries(ctx, st, fixtureSID); err != nil {
		t.Fatalf("SyncSeries: %v", err)
	}

	// 只发 1 次上游请求（不做 N+1 集页抓取），路径正确。
	if got := fake.requests(); len(got) != 1 || got[0] != "/player/"+fixtureSID {
		t.Fatalf("上游请求 = %v, 期望恰一次 /player/%s", got, fixtureSID)
	}

	d, err := st.DramaByProviderID(ctx, hongguo.Name, fixtureSID)
	if err != nil {
		t.Fatalf("DramaByProviderID: %v", err)
	}
	if d.Provider != hongguo.Name || d.ProviderID != fixtureSID {
		t.Fatalf("剧目身份 = (%q,%q), 期望 (%q,%q)", d.Provider, d.ProviderID, hongguo.Name, fixtureSID)
	}
	if d.Title != fixtureName || d.Plot != fixtureIntro || d.CoverURL != fixtureCover {
		t.Fatalf("剧目字段与 fixture 不一致：title=%q plot=%q cover=%q", d.Title, d.Plot, d.CoverURL)
	}
	if d.EpisodeTotal != len(vids) {
		t.Fatalf("episode_total = %d, 期望 %d", d.EpisodeTotal, len(vids))
	}
	if d.Year != 2025 {
		t.Fatalf("year = %d, 期望 2025（首见时间 1747533495）", d.Year)
	}
	syncAt, err := time.Parse(time.RFC3339, d.SyncAt)
	if err != nil {
		t.Fatalf("sync_at %q 不是 RFC3339: %v", d.SyncAt, err)
	}
	if syncAt.Location() != time.UTC || !syncAt.Equal(clock.t) {
		t.Fatalf("sync_at = %v, 期望注入时钟的 UTC 值 %v", syncAt, clock.t)
	}

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
		if ep.Source != hongguo.Name {
			t.Fatalf("eps[%d].Source = %q, 期望 %q", i, ep.Source, hongguo.Name)
		}
		if ep.Title != "" || ep.DurationSec != 0 {
			t.Fatalf("eps[%d] 编造了标题/时长：%q / %d", i, ep.Title, ep.DurationSec)
		}
		if ep.PlayURL != "" || ep.PlayURLAt != "" {
			t.Fatalf("eps[%d] 写了 play_url：%q / %q（SSR 只有 10s 预览链，必须留空）", i, ep.PlayURL, ep.PlayURLAt)
		}
	}
}

// 同 id 重复同步幂等：行数与行 id 不变，第二次的元数据与 sync_at 生效。
func TestSyncSeriesIdempotent(t *testing.T) {
	fixture := readFixture(t, "page-series.html")
	renamed := strings.Replace(fixture, fixtureName, fixtureName+"（重同步）", 1)
	if renamed == fixture {
		t.Fatal("fixture 里没找到剧名，无法构造第二次响应")
	}
	fake, srv := newFakeSite(t, http.StatusOK, fixture)
	st := newTestStore(t)
	clock := &fakeClock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)}
	s := newTestSyncer(t, srv, clock)
	ctx := context.Background()

	mustSync := func() (int64, []int64) {
		t.Helper()
		if err := s.SyncSeries(ctx, st, fixtureSID); err != nil {
			t.Fatalf("SyncSeries: %v", err)
		}
		d, err := st.DramaByProviderID(ctx, hongguo.Name, fixtureSID)
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
	d, err := st.DramaByProviderID(ctx, hongguo.Name, fixtureSID)
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

// 插件报错时库内容一字不变：错误原样返回（哨兵语义由插件定义），不写半截数据。
func TestSyncSeriesErrorsKeepDB(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		sentinel error
	}{
		{"上游 502", http.StatusBadGateway, "bad gateway", plugin.ErrUpstream},
		{"上游 429", http.StatusTooManyRequests, "too many requests", plugin.ErrRejected},
		{"页面身份不符", http.StatusOK, readFixture(t, "page-wrong-series.html"), plugin.ErrSchema},
		{"缺 vid_list", http.StatusOK, readFixture(t, "page-missing-vidlist.html"), plugin.ErrSchema},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, srv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series.html"))
			st := newTestStore(t)
			s := newTestSyncer(t, srv, &fakeClock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)})
			ctx := context.Background()

			if err := s.SyncSeries(ctx, st, fixtureSID); err != nil {
				t.Fatalf("预备同步: %v", err)
			}
			beforeDramas, beforeEpisodes := dbCounts(t, st)

			fake.setReply(tc.status, tc.body)
			err := s.SyncSeries(ctx, st, fixtureSID)
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("err = %v, 期望 %v", err, tc.sentinel)
			}
			afterDramas, afterEpisodes := dbCounts(t, st)
			if afterDramas != beforeDramas || afterEpisodes != beforeEpisodes {
				t.Fatalf("库变了：(剧,集) %d/%d → %d/%d", beforeDramas, beforeEpisodes, afterDramas, afterEpisodes)
			}
		})
	}
}

// fakePlugin 是最小插件实现：Work 返回预置详情，其余方法不参与本包。
type fakePlugin struct {
	name    string
	detail  plugin.WorkDetail
	err     error
	workAdd int
}

func (f *fakePlugin) Name() string { return f.name }

func (f *fakePlugin) Enumerate(context.Context, *plugin.Cursor) (plugin.Page, error) {
	return plugin.Page{}, nil
}

func (f *fakePlugin) Work(context.Context, plugin.WorkRef) (plugin.WorkDetail, error) {
	f.workAdd++
	return f.detail, f.err
}

func (f *fakePlugin) Resolve(context.Context, plugin.WorkRef, int) (plugin.Stream, error) {
	return plugin.Stream{}, plugin.ErrNoStream
}

func (f *fakePlugin) Health(context.Context) error { return nil }

// fakePlugin 不实现 WorkWithEpisodeKeys：框架写库时 episode.vid 留空，
// 不得为它编造任何值（别的源没有源侧集标识这回事）。
func TestSyncWithoutEpisodeKeys(t *testing.T) {
	st := newTestStore(t)
	p := &fakePlugin{
		name: "fake",
		detail: plugin.WorkDetail{
			Work:     plugin.Work{Ref: plugin.WorkRef{Plugin: "fake", ID: "w1"}, Title: "假剧", Year: 2024},
			Episodes: []plugin.Episode{{No: 1}, {No: 2}},
		},
	}
	s := New(p)
	s.now = (&fakeClock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)}).now

	if err := s.SyncSeries(context.Background(), st, "w1"); err != nil {
		t.Fatalf("SyncSeries: %v", err)
	}
	d, err := st.DramaByProviderID(context.Background(), "fake", "w1")
	if err != nil {
		t.Fatalf("DramaByProviderID: %v", err)
	}
	eps, err := st.ListEpisodes(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("集数 = %d, 期望 2", len(eps))
	}
	for _, ep := range eps {
		if ep.Vid != "" {
			t.Fatalf("episode.vid = %q，插件没给源侧集标识时必须是空", ep.Vid)
		}
	}
}

// 框架校验：集号不连续、集表为空都是插件 bug → ErrSchema，且不写库。
func TestSyncValidatesEpisodes(t *testing.T) {
	cases := []struct {
		name     string
		episodes []plugin.Episode
	}{
		{"集号不连续", []plugin.Episode{{No: 1}, {No: 3}}},
		{"集号不从 1 起", []plugin.Episode{{No: 2}}},
		{"集表为空", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			p := &fakePlugin{name: "fake", detail: plugin.WorkDetail{
				Work:     plugin.Work{Ref: plugin.WorkRef{Plugin: "fake", ID: "w1"}, Title: "假剧"},
				Episodes: tc.episodes,
			}}
			s := New(p)

			err := s.SyncSeries(context.Background(), st, "w1")
			if !errors.Is(err, plugin.ErrSchema) {
				t.Fatalf("err = %v, 期望 ErrSchema", err)
			}
			if dramas, episodes := dbCounts(t, st); dramas != 0 || episodes != 0 {
				t.Fatalf("校验失败却写了库：(剧,集) = (%d,%d)", dramas, episodes)
			}
		})
	}
}

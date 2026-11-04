package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/store"
)

// fakeSource 是最小的插件替身：只实现 Resolve 的两种结果，其余方法返回零值。
//
// calls 带锁：并发用例里多个请求会同时进 Resolve，计数器本身不能成为竞态的
// 来源，否则「只调一次」的断言证明不了单飞。
type fakeSource struct {
	name string
	byEp map[int]plugin.Stream
	err  error

	mu    sync.Mutex
	calls []string
	// callers 与 calls 一一对应：记录每次 Resolve 看到的 caller 标记
	// （MKACG-683：播放面必须打 caller=play）。
	callers []string
}

// callsCount 返回 Resolve 的调用次数（调用方可能并发，读也要加锁）。
func (f *fakeSource) callsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Enumerate(context.Context, *plugin.Cursor) (plugin.Page, error) {
	return plugin.Page{}, nil
}

func (f *fakeSource) Work(context.Context, plugin.WorkRef) (plugin.WorkDetail, error) {
	return plugin.WorkDetail{}, nil
}

func (f *fakeSource) Resolve(ctx context.Context, ref plugin.WorkRef, epNo int) (plugin.Stream, error) {
	f.mu.Lock()
	f.calls = append(f.calls, ref.ID)
	f.callers = append(f.callers, plugin.CallerOf(ctx))
	f.mu.Unlock()
	if f.err != nil {
		return plugin.Stream{}, f.err
	}
	return f.byEp[epNo], nil
}

func (f *fakeSource) Health(context.Context) error { return nil }

// newResolveRig 建临时库，写入一部剧与两集，并按插件注册表装配 Resolver。
//
// src 是注册进框架的插件本体：批量解析的用例要传实现了 ResolveSeries 的实现，
// 所以这里收接口而不是 *fakeSource（剧与集的 Source 用 src.Name()）。
func newResolveRig(t *testing.T, src plugin.Plugin) (*Resolver, *store.Store, []store.Episode) {
	t.Helper()
	db, err := store.Open(context.Background(), pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	ctx := context.Background()
	dramaID, err := st.UpsertDrama(ctx, store.Drama{
		Provider: src.Name(), ProviderID: "w1", Title: "测试剧", Year: 2026,
	})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	eps := make([]store.Episode, 0, 2)
	for _, no := range []int{1, 2} {
		id, err := st.UpsertEpisode(ctx, store.Episode{DramaID: dramaID, EpNo: no, Source: src.Name(), Vid: "v"})
		if err != nil {
			t.Fatalf("UpsertEpisode: %v", err)
		}
		ep, err := st.EpisodeByID(ctx, id)
		if err != nil {
			t.Fatalf("EpisodeByID: %v", err)
		}
		eps = append(eps, ep)
	}
	reg := plugin.NewRegistry()
	if err := reg.Register(src); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return NewResolver(st, store.NewSettings(db.DB), reg), st, eps
}

// TestDirectReusesUnexpiredChain 覆盖反转后的契约：第一次解析并落库，第二次直接
// 复用未过期的行，上游一次都不调（ADR-0016）。
func TestDirectReusesUnexpiredChain(t *testing.T) {
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4"},
	}}
	r, _, eps := newResolveRig(t, src)

	first, err := r.Direct(context.Background(), eps[0])
	if err != nil {
		t.Fatalf("第一次 Direct: %v", err)
	}
	// 上游换链不该影响未过期的复用：第二次拿到的必须是库里那条。
	src.byEp[1] = plugin.Stream{URL: "https://cdn.example/1-rotated.mp4"}
	second, err := r.Direct(context.Background(), eps[0])
	if err != nil {
		t.Fatalf("第二次 Direct: %v", err)
	}
	if second.URL != first.URL {
		t.Fatalf("第二次直链 = %q, 期望复用 %q", second.URL, first.URL)
	}
	if src.callsCount() != 1 {
		t.Fatalf("Resolve 调用 %d 次, 期望 1（未过期必须零外呼）", src.callsCount())
	}
}

// TestDirectRefreshesExpiredChain 覆盖过期刷新：把行的 expires_at 改到过去，下一次
// 请求重新解析一次并覆盖同一行。
func TestDirectRefreshesExpiredChain(t *testing.T) {
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4"},
	}}
	r, st, eps := newResolveRig(t, src)
	ctx := context.Background()
	if _, err := r.Direct(ctx, eps[0]); err != nil {
		t.Fatalf("第一次 Direct: %v", err)
	}
	es, err := st.GetEpisodeStream(ctx, eps[0].ID)
	if err != nil {
		t.Fatalf("GetEpisodeStream: %v", err)
	}
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	es.ExpiresAt = past
	if err := st.UpsertEpisodeStream(ctx, es); err != nil {
		t.Fatalf("UpsertEpisodeStream: %v", err)
	}
	src.byEp[1] = plugin.Stream{URL: "https://cdn.example/1-new.mp4"}

	got, err := r.Direct(ctx, eps[0])
	if err != nil {
		t.Fatalf("过期后的 Direct: %v", err)
	}
	if want := "https://cdn.example/1-new.mp4"; got.URL != want {
		t.Fatalf("直链 = %q, 期望刷新成 %q", got.URL, want)
	}
	if src.callsCount() != 2 {
		t.Fatalf("Resolve 调用 %d 次, 期望 2（过期只刷新 1 次）", src.callsCount())
	}
}

// TestDirectSingleFlight 覆盖并发单飞：20 个并发请求同一集，上游只调 1 次，
// 所有调用拿到同一份结果。
func TestDirectSingleFlight(t *testing.T) {
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4"},
	}}
	r, _, eps := newResolveRig(t, src)

	const n = 20
	var wg sync.WaitGroup
	results := make([]Direct, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = r.Direct(context.Background(), eps[0])
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个 Direct: %v", i, errs[i])
		}
		if results[i].URL != "https://cdn.example/1.mp4" {
			t.Fatalf("第 %d 个直链 = %q", i, results[i].URL)
		}
	}
	if src.callsCount() != 1 {
		t.Fatalf("Resolve 调用 %d 次, 期望 1（单飞）", src.callsCount())
	}
}

// TestDirectSingleFlightRepeated 用多轮并发反复压同一段窗口：cached 判为未命中之后、
// claim 拿到所有权之前的间隙里，前一个 owner 可能刚落了库。修法是 owner 抢到所有权
// 后再复核一次缓存；每轮都要恰好一次外呼，多轮只是让这个窗口被反复踩到。
func TestDirectSingleFlightRepeated(t *testing.T) {
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4"},
	}}
	r, _, eps := newResolveRig(t, src)
	ctx := context.Background()

	const rounds, n = 40, 8
	for round := 0; round < rounds; round++ {
		if err := r.Invalidate(ctx, eps[0].ID); err != nil {
			t.Fatalf("第 %d 轮 Invalidate: %v", round, err)
		}
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = r.Direct(ctx, eps[0])
			}(i)
		}
		wg.Wait()
		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Fatalf("第 %d 轮第 %d 个 Direct: %v", round, i, errs[i])
			}
		}
	}
	if got := src.callsCount(); got != rounds {
		t.Fatalf("Resolve 调用 %d 次, 期望 %d（每轮恰好 1 次）", got, rounds)
	}
}

// TestDirectInvalidateRefreshes 覆盖「标记失效」入口：作废后下一次请求重新解析，
// 拿到上游的新链。
func TestDirectInvalidateRefreshes(t *testing.T) {
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4"},
	}}
	r, _, eps := newResolveRig(t, src)
	ctx := context.Background()
	if _, err := r.Direct(ctx, eps[0]); err != nil {
		t.Fatalf("第一次 Direct: %v", err)
	}
	if err := r.Invalidate(ctx, eps[0].ID); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	src.byEp[1] = plugin.Stream{URL: "https://cdn.example/1-fresh.mp4"}
	got, err := r.Direct(ctx, eps[0])
	if err != nil {
		t.Fatalf("作废后的 Direct: %v", err)
	}
	if want := "https://cdn.example/1-fresh.mp4"; got.URL != want {
		t.Fatalf("直链 = %q, 期望 %q", got.URL, want)
	}
	if src.callsCount() != 2 {
		t.Fatalf("Resolve 调用 %d 次, 期望 2", src.callsCount())
	}
}

// TestDirectTTLAppliesToNewRows 覆盖 stream_url_ttl_sec 即时生效：改值后下一次解析
// 用新值算 expires_at，不需要重启。
func TestDirectTTLAppliesToNewRows(t *testing.T) {
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4"},
		2: {URL: "https://cdn.example/2.mp4"},
	}}
	r, st, eps := newResolveRig(t, src)
	ctx := context.Background()
	settings := r.settings

	expiresAfter := func(ep store.Episode) time.Time {
		t.Helper()
		es, err := st.GetEpisodeStream(ctx, ep.ID)
		if err != nil {
			t.Fatalf("GetEpisodeStream: %v", err)
		}
		got, err := time.Parse(time.RFC3339, es.ExpiresAt)
		if err != nil {
			t.Fatalf("解析 expires_at %q: %v", es.ExpiresAt, err)
		}
		return got
	}

	if err := settings.Set(ctx, store.KeyStreamURLTTL, "60"); err != nil {
		t.Fatalf("Set ttl=60: %v", err)
	}
	before := time.Now()
	if _, err := r.Direct(ctx, eps[0]); err != nil {
		t.Fatalf("Direct ep1: %v", err)
	}
	if got := expiresAfter(eps[0]); got.Sub(before) < 55*time.Second || got.Sub(before) > 70*time.Second {
		t.Fatalf("ttl=60 时 expires_at = %v 后, 期望约 60s", got.Sub(before))
	}

	if err := settings.Set(ctx, store.KeyStreamURLTTL, "600"); err != nil {
		t.Fatalf("Set ttl=600: %v", err)
	}
	before = time.Now()
	if _, err := r.Direct(ctx, eps[1]); err != nil {
		t.Fatalf("Direct ep2: %v", err)
	}
	if got := expiresAfter(eps[1]); got.Sub(before) < 595*time.Second || got.Sub(before) > 610*time.Second {
		t.Fatalf("ttl=600 时 expires_at = %v 后, 期望约 600s（改配置必须即时生效）", got.Sub(before))
	}
}

// TestDirectHonorsPluginExpiry 覆盖上游给了 ExpiresAt：以它为准，而不是 TTL。
func TestDirectHonorsPluginExpiry(t *testing.T) {
	upstream := time.Now().Add(2 * time.Minute).UTC().Truncate(time.Second)
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4", ExpiresAt: upstream},
	}}
	r, st, eps := newResolveRig(t, src)
	ctx := context.Background()
	if _, err := r.Direct(ctx, eps[0]); err != nil {
		t.Fatalf("Direct: %v", err)
	}
	es, err := st.GetEpisodeStream(ctx, eps[0].ID)
	if err != nil {
		t.Fatalf("GetEpisodeStream: %v", err)
	}
	got, err := time.Parse(time.RFC3339, es.ExpiresAt)
	if err != nil {
		t.Fatalf("解析 expires_at %q: %v", es.ExpiresAt, err)
	}
	if !got.Equal(upstream) {
		t.Fatalf("expires_at = %v, 期望上游给的 %v", got, upstream)
	}
}

// TestDirectPassesKey 覆盖 key 透传：插件给的 16 字节 AES-128 key 原样带出，
// 明文流（插件不设 key）带出 nil。「可播」判据只看 URL，key 不影响。
func TestDirectPassesKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x2a}, 16)
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4", Key: key},
		2: {URL: "https://cdn.example/2.mp4"},
	}}
	r, st, eps := newResolveRig(t, src)

	got, err := r.Direct(context.Background(), eps[0])
	if err != nil {
		t.Fatalf("Direct: %v", err)
	}
	if !bytes.Equal(got.Key, key) {
		t.Fatalf("Key = %x, 期望 %x（插件给的 key 必须原样透传）", got.Key, key)
	}
	if want := "https://cdn.example/1.mp4"; got.URL != want {
		t.Fatalf("直链 = %q, 期望 %q", got.URL, want)
	}
	// key 与 URL 一起落库（同生命周期），第二次请求从库里读回同一份 key。
	es, err := st.GetEpisodeStream(context.Background(), eps[0].ID)
	if err != nil {
		t.Fatalf("GetEpisodeStream: %v", err)
	}
	if !bytes.Equal(es.Key, key) {
		t.Fatalf("库内 key = %x, 期望 %x", es.Key, key)
	}
	cached, err := r.Direct(context.Background(), eps[0])
	if err != nil {
		t.Fatalf("第二次 Direct: %v", err)
	}
	if !bytes.Equal(cached.Key, key) {
		t.Fatalf("缓存返回的 key = %x, 期望 %x", cached.Key, key)
	}

	plain, err := r.Direct(context.Background(), eps[1])
	if err != nil {
		t.Fatalf("Direct（明文集）: %v", err)
	}
	if plain.Key != nil {
		t.Fatalf("Key = %x, 期望 nil（插件没给 key 就是明文）", plain.Key)
	}
}

// TestDirectNoStream 覆盖「插件说没有直链」→ 错误里带 plugin.ErrNoStream，
// 边界包据此映射 502（契约状态码矩阵）。
func TestDirectNoStream(t *testing.T) {
	src := &fakeSource{name: "web", err: plugin.ErrNoStream}
	r, _, eps := newResolveRig(t, src)

	if _, err := r.Direct(context.Background(), eps[0]); !errors.Is(err, plugin.ErrNoStream) {
		t.Fatalf("err = %v, 期望 plugin.ErrNoStream", err)
	}
}

// TestDirectEmptyURL 覆盖「插件不报错但给空链」：同样归 ErrNoStream，不放空 URL 出去。
func TestDirectEmptyURL(t *testing.T) {
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{1: {}}}
	r, _, eps := newResolveRig(t, src)

	if _, err := r.Direct(context.Background(), eps[0]); !errors.Is(err, plugin.ErrNoStream) {
		t.Fatalf("err = %v, 期望 plugin.ErrNoStream", err)
	}
}

// TestDirectUnknownPlugin 覆盖剧的 provider 没有注册插件：返回错误而不是空链。
func TestDirectUnknownPlugin(t *testing.T) {
	db, err := store.Open(context.Background(), pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	ctx := context.Background()
	dramaID, err := st.UpsertDrama(ctx, store.Drama{Provider: "ghost", ProviderID: "w1", Title: "剧"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	epID, err := st.UpsertEpisode(ctx, store.Episode{DramaID: dramaID, EpNo: 1, Source: "ghost", Vid: "v"})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}
	ep, err := st.EpisodeByID(ctx, epID)
	if err != nil {
		t.Fatalf("EpisodeByID: %v", err)
	}

	r := NewResolver(st, store.NewSettings(db.DB), plugin.NewRegistry())
	d, err := r.Direct(ctx, ep)
	if err == nil || d.URL != "" {
		t.Fatalf("未注册插件却返回 (%q, %v)", d.URL, err)
	}
}

// fakeSeriesSource 在 fakeSource 之上实现 SeriesResolver：记录批量调用与集号，
// 证明 DirectMany 走的是「一次页面解析多集」而不是逐集回退。
type fakeSeriesSource struct {
	fakeSource
	batches [][]int
}

func (f *fakeSeriesSource) ResolveSeries(_ context.Context, _ plugin.WorkRef, epNos []int) ([]plugin.Stream, error) {
	f.batches = append(f.batches, append([]int(nil), epNos...))
	out := make([]plugin.Stream, len(epNos))
	for i, no := range epNos {
		out[i] = f.byEp[no]
	}
	return out, nil
}

// TestDirectManyUsesSeriesResolver 覆盖批量路径：插件声明了批量能力时，一次调用
// 解析全部集号，逐集的 Resolve 一次都不调（页面请求数不随集数增长）。
func TestDirectManyUsesSeriesResolver(t *testing.T) {
	src := &fakeSeriesSource{fakeSource: fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4"},
		2: {URL: "https://cdn.example/2.mp4"},
	}}}
	r, _, eps := newResolveRig(t, src)

	got, err := r.DirectMany(context.Background(), eps)
	if err != nil {
		t.Fatalf("DirectMany: %v", err)
	}
	if len(got) != len(eps) {
		t.Fatalf("结果条数 = %d, 期望 %d", len(got), len(eps))
	}
	for i, want := range []string{"https://cdn.example/1.mp4", "https://cdn.example/2.mp4"} {
		if got[i].URL != want {
			t.Errorf("第 %d 条直链 = %q, 期望 %q", i, got[i].URL, want)
		}
	}
	if src.callsCount() != 0 {
		t.Fatalf("逐集 Resolve 调用 %d 次, 期望 0（应走批量）", src.callsCount())
	}
	if len(src.batches) != 1 || len(src.batches[0]) != 2 || src.batches[0][0] != 1 || src.batches[0][1] != 2 {
		t.Fatalf("批量调用 = %v, 期望一次 [1 2]", src.batches)
	}
}

// TestDirectManyReusesCache 覆盖整块第二次直接命中库：第二次 DirectMany 一次批量
// 调用都不发（合并块首播之后的播放零外呼）。
func TestDirectManyReusesCache(t *testing.T) {
	src := &fakeSeriesSource{fakeSource: fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4"},
		2: {URL: "https://cdn.example/2.mp4"},
	}}}
	r, _, eps := newResolveRig(t, src)
	ctx := context.Background()

	if _, err := r.DirectMany(ctx, eps); err != nil {
		t.Fatalf("第一次 DirectMany: %v", err)
	}
	got, err := r.DirectMany(ctx, eps)
	if err != nil {
		t.Fatalf("第二次 DirectMany: %v", err)
	}
	if len(got) != 2 || got[0].URL != "https://cdn.example/1.mp4" || got[1].URL != "https://cdn.example/2.mp4" {
		t.Fatalf("结果 = %v, 期望复用库内两条直链", got)
	}
	if len(src.batches) != 1 {
		t.Fatalf("批量调用 %d 次, 期望 1（第二次必须全命中库）", len(src.batches))
	}
}

// TestDirectManyTenEpisodeBlockCount 用默认合并块规模（10 集）钉住改造前后的数字：
// 首播 1 次批量解析（10 集一次页面请求），第二次 0 次（全部命中未过期的库内行）。
func TestDirectManyTenEpisodeBlockCount(t *testing.T) {
	src := &fakeSeriesSource{fakeSource: fakeSource{name: "web", byEp: map[int]plugin.Stream{}}}
	for no := 1; no <= 10; no++ {
		src.byEp[no] = plugin.Stream{URL: fmt.Sprintf("https://cdn.example/%d.mp4", no)}
	}
	r, st, eps := newResolveRig(t, src)
	ctx := context.Background()
	for no := 3; no <= 10; no++ {
		id, err := st.UpsertEpisode(ctx, store.Episode{
			DramaID: eps[0].DramaID, EpNo: no, Source: "web", Vid: fmt.Sprintf("v%d", no),
		})
		if err != nil {
			t.Fatalf("UpsertEpisode %d: %v", no, err)
		}
		ep, err := st.EpisodeByID(ctx, id)
		if err != nil {
			t.Fatalf("EpisodeByID %d: %v", no, err)
		}
		eps = append(eps, ep)
	}
	if len(eps) != 10 {
		t.Fatalf("成员数 = %d, 期望 10", len(eps))
	}

	if _, err := r.DirectMany(ctx, eps); err != nil {
		t.Fatalf("首播 DirectMany: %v", err)
	}
	if len(src.batches) != 1 || len(src.batches[0]) != 10 {
		t.Fatalf("首播批量调用 = %v, 期望一次 10 集", src.batches)
	}
	if _, err := r.DirectMany(ctx, eps); err != nil {
		t.Fatalf("第二次 DirectMany: %v", err)
	}
	if len(src.batches) != 1 {
		t.Fatalf("第二次批量调用 = %v, 期望 0 次新增（未过期全部复用）", src.batches)
	}
}

// TestDirectManyFallsBackPerEpisode 覆盖插件没有批量能力：仍能逐集解析（行为与
// 加批量之前一致），结果按入参顺序对齐、key 照常透传。
func TestDirectManyFallsBackPerEpisode(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 16)
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4", Key: key},
		2: {URL: "https://cdn.example/2.mp4"},
	}}
	r, _, eps := newResolveRig(t, src)

	got, err := r.DirectMany(context.Background(), eps)
	if err != nil {
		t.Fatalf("DirectMany: %v", err)
	}
	if len(got) != 2 || got[0].URL != "https://cdn.example/1.mp4" || got[1].URL != "https://cdn.example/2.mp4" {
		t.Fatalf("结果 = %v, 期望按集号对齐的两条直链", got)
	}
	if !bytes.Equal(got[0].Key, key) {
		t.Fatalf("Key = %x, 期望 %x", got[0].Key, key)
	}
	if src.callsCount() != 2 {
		t.Fatalf("Resolve 调用 %d 次, 期望 2（逐集回退）", src.callsCount())
	}
}

// TestDirectManyEmpty 覆盖空入参：不发请求、不报错。
func TestDirectManyEmpty(t *testing.T) {
	src := &fakeSeriesSource{fakeSource: fakeSource{name: "web"}}
	r, _, _ := newResolveRig(t, src)

	got, err := r.DirectMany(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("DirectMany(nil) = (%v, %v), 期望 (空, nil)", got, err)
	}
	if len(src.batches) != 0 {
		t.Fatalf("批量调用 = %v, 期望零调用", src.batches)
	}
}

// TestDirectManyCrossDrama 覆盖同一批里混入别的剧的集：宁可报错也不按第一集的
// series 去解析别的剧的集号（批量解析的前提是同一次播放单元属于同一部剧）。
func TestDirectManyCrossDrama(t *testing.T) {
	src := &fakeSeriesSource{fakeSource: fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4"},
	}}}
	r, st, eps := newResolveRig(t, src)
	otherID, err := st.UpsertDrama(context.Background(), store.Drama{
		Provider: "web", ProviderID: "w2", Title: "另一部", Year: 2026,
	})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	epID, err := st.UpsertEpisode(context.Background(), store.Episode{DramaID: otherID, EpNo: 1, Source: "web", Vid: "v"})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}
	other, err := st.EpisodeByID(context.Background(), epID)
	if err != nil {
		t.Fatalf("EpisodeByID: %v", err)
	}

	if _, err := r.DirectMany(context.Background(), []store.Episode{eps[0], other}); err == nil {
		t.Fatal("跨剧批量解析没有报错")
	}
	if len(src.batches) != 0 {
		t.Fatalf("批量调用 = %v, 期望校验失败时不外呼", src.batches)
	}
}

// TestDirectMarksCallerPlay 覆盖 MKACG-683 的 caller 标记：播放面无论是单集
// Direct 还是批量 DirectMany，进到插件时 ctx 里都必须带 caller=play。
// 缓存命中不解析、也就不打日志，因此这里用两集都未落库的首次解析。
func TestDirectMarksCallerPlay(t *testing.T) {
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4"},
		2: {URL: "https://cdn.example/2.mp4"},
	}}
	r, _, eps := newResolveRig(t, src)

	if _, err := r.Direct(context.Background(), eps[0]); err != nil {
		t.Fatalf("Direct: %v", err)
	}
	if _, err := r.DirectMany(context.Background(), []store.Episode{eps[1]}); err != nil {
		t.Fatalf("DirectMany: %v", err)
	}

	src.mu.Lock()
	got := append([]string(nil), src.callers...)
	src.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("插件被调用 %d 次, 期望 2（Direct 与 DirectMany 各一次）", len(got))
	}
	for i, caller := range got {
		if caller != plugin.CallerPlay {
			t.Errorf("第 %d 次调用的 caller = %q, 期望 %q", i+1, caller, plugin.CallerPlay)
		}
	}
}

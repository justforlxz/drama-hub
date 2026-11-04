package stream

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/store"
)

// fakeSource 是最小的插件替身：只实现 Resolve 的两种结果，其余方法返回零值。
type fakeSource struct {
	name  string
	byEp  map[int]plugin.Stream
	err   error
	calls []string
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Enumerate(context.Context, *plugin.Cursor) (plugin.Page, error) {
	return plugin.Page{}, nil
}

func (f *fakeSource) Work(context.Context, plugin.WorkRef) (plugin.WorkDetail, error) {
	return plugin.WorkDetail{}, nil
}

func (f *fakeSource) Resolve(_ context.Context, ref plugin.WorkRef, epNo int) (plugin.Stream, error) {
	f.calls = append(f.calls, ref.ID)
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
	return NewResolver(st, reg), st, eps
}

// TestDirectResolvesEveryRequest 覆盖正常路径：按集号向剧所属插件现解析，每次请求
// 都调一次 Resolve（直链不缓存，插件换链后下一次就拿新链）。
func TestDirectResolvesEveryRequest(t *testing.T) {
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4"},
		2: {URL: "https://cdn.example/2.mp4"},
	}}
	r, _, eps := newResolveRig(t, src)

	got, err := r.Direct(context.Background(), eps[0])
	if err != nil {
		t.Fatalf("Direct: %v", err)
	}
	if want := "https://cdn.example/1.mp4"; got.URL != want {
		t.Fatalf("直链 = %q, 期望 %q", got.URL, want)
	}
	src.byEp[1] = plugin.Stream{URL: "https://cdn.example/1-v2.mp4"}
	got, err = r.Direct(context.Background(), eps[0])
	if err != nil {
		t.Fatalf("第二次 Direct: %v", err)
	}
	if want := "https://cdn.example/1-v2.mp4"; got.URL != want {
		t.Fatalf("第二次直链 = %q, 期望 %q（必须重新解析，不缓存旧链）", got.URL, want)
	}
	if len(src.calls) != 2 {
		t.Fatalf("Resolve 调用 %d 次, 期望 2", len(src.calls))
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
	r, _, eps := newResolveRig(t, src)

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

	r := NewResolver(st, plugin.NewRegistry())
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
	if len(src.calls) != 0 {
		t.Fatalf("逐集 Resolve 调用 %d 次, 期望 0（应走批量）", len(src.calls))
	}
	if len(src.batches) != 1 || len(src.batches[0]) != 2 || src.batches[0][0] != 1 || src.batches[0][1] != 2 {
		t.Fatalf("批量调用 = %v, 期望一次 [1 2]", src.batches)
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
	if len(src.calls) != 2 {
		t.Fatalf("Resolve 调用 %d 次, 期望 2（逐集回退）", len(src.calls))
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

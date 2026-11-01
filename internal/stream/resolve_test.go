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
func newResolveRig(t *testing.T, src *fakeSource) (*Resolver, *store.Store, []store.Episode) {
	t.Helper()
	db, err := store.Open(context.Background(), pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	ctx := context.Background()
	dramaID, err := st.UpsertDrama(ctx, store.Drama{
		Provider: src.name, ProviderID: "w1", Title: "测试剧", Year: 2026,
	})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	eps := make([]store.Episode, 0, 2)
	for _, no := range []int{1, 2} {
		id, err := st.UpsertEpisode(ctx, store.Episode{DramaID: dramaID, EpNo: no, Source: src.name, Vid: "v"})
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

// TestDirectURL 覆盖正常路径：按集号向剧所属插件现解析，每次请求都调一次 Resolve。
func TestDirectURL(t *testing.T) {
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{
		1: {URL: "https://cdn.example/1.mp4"},
		2: {URL: "https://cdn.example/2.mp4"},
	}}
	r, _, eps := newResolveRig(t, src)

	got, err := r.DirectURL(context.Background(), eps[0])
	if err != nil {
		t.Fatalf("DirectURL: %v", err)
	}
	if want := "https://cdn.example/1.mp4"; got != want {
		t.Fatalf("直链 = %q, 期望 %q", got, want)
	}
	// 第二次请求仍然回源（直链不缓存）：插件换链后下一次就拿新链。
	src.byEp[1] = plugin.Stream{URL: "https://cdn.example/1-v2.mp4"}
	got, err = r.DirectURL(context.Background(), eps[0])
	if err != nil {
		t.Fatalf("第二次 DirectURL: %v", err)
	}
	if want := "https://cdn.example/1-v2.mp4"; got != want {
		t.Fatalf("第二次直链 = %q, 期望 %q（必须重新解析，不缓存旧链）", got, want)
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

// TestDirectURLNoStream 覆盖「插件说没有直链」→ 错误里带 plugin.ErrNoStream，
// 边界包据此映射 502（契约状态码矩阵）。
func TestDirectURLNoStream(t *testing.T) {
	src := &fakeSource{name: "web", err: plugin.ErrNoStream}
	r, _, eps := newResolveRig(t, src)

	if _, err := r.DirectURL(context.Background(), eps[0]); !errors.Is(err, plugin.ErrNoStream) {
		t.Fatalf("err = %v, 期望 plugin.ErrNoStream", err)
	}
}

// TestDirectURLEmptyURL 覆盖「插件不报错但给空链」：同样归 ErrNoStream，不放空 URL 出去。
func TestDirectURLEmptyURL(t *testing.T) {
	src := &fakeSource{name: "web", byEp: map[int]plugin.Stream{1: {}}}
	r, _, eps := newResolveRig(t, src)

	if _, err := r.DirectURL(context.Background(), eps[0]); !errors.Is(err, plugin.ErrNoStream) {
		t.Fatalf("err = %v, 期望 plugin.ErrNoStream", err)
	}
}

// TestDirectURLUnknownPlugin 覆盖剧的 provider 没有注册插件：返回错误而不是空链。
func TestDirectURLUnknownPlugin(t *testing.T) {
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
	url, err := r.DirectURL(ctx, ep)
	if err == nil || url != "" {
		t.Fatalf("未注册插件却返回 (%q, %v)", url, err)
	}
}

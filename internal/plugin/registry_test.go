package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakePlugin 是测试用的插件：四个方法都返回预置值并记录调用轨迹。
//
// 它存在的理由是 AC：注册一个假插件就能被 Enumerate / Work / Resolve / Health 驱动，
// 框架侧不需要为它改一行代码 —— 契约只靠 Plugin 接口这一条耦合。
type fakePlugin struct {
	name   string
	pages  []Page
	page   int
	detail WorkDetail
	stream Stream
	health error
	calls  []string
}

func (f *fakePlugin) Name() string { return f.name }

func (f *fakePlugin) Enumerate(_ context.Context, cur *Cursor) (Page, error) {
	f.calls = append(f.calls, "enumerate")
	// cur 只是断点：框架原样回传，插件自己解释。
	if cur != nil {
		for i, p := range f.pages {
			if p.Next != nil && p.Next.Token == cur.Token {
				f.page = i + 1
			}
		}
	}
	if f.page >= len(f.pages) {
		return Page{}, nil
	}
	return f.pages[f.page], nil
}

func (f *fakePlugin) Work(_ context.Context, ref WorkRef) (WorkDetail, error) {
	f.calls = append(f.calls, "work:"+ref.ID)
	return f.detail, nil
}

func (f *fakePlugin) Resolve(_ context.Context, ref WorkRef, epNo int) (Stream, error) {
	f.calls = append(f.calls, "resolve:"+ref.ID)
	return f.stream, nil
}

func (f *fakePlugin) Health(context.Context) error {
	f.calls = append(f.calls, "health")
	return f.health
}

func newFakePlugin() *fakePlugin {
	first := WorkRef{Plugin: "fake", ID: "w1"}
	second := WorkRef{Plugin: "fake", ID: "w2"}
	return &fakePlugin{
		name: "fake",
		pages: []Page{
			{Refs: []WorkRef{first}, Next: &Cursor{Token: "page-2"}},
			{Refs: []WorkRef{second}},
		},
		detail: WorkDetail{
			Work:     Work{Ref: first, Title: "假剧", Year: 2024, EpisodeDecl: 1},
			Episodes: []Episode{{No: 1}},
		},
		stream: Stream{URL: "https://cdn.example/ep1.mp4", ExpiresAt: time.Unix(0, 0)},
	}
}

// 注册一个假插件即可被四个方法驱动（AC）。
func TestRegistryDrivesPlugin(t *testing.T) {
	fake := newFakePlugin()
	reg := NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatalf("Register: %v", err)
	}
	p, ok := reg.Get(fake.name)
	if !ok {
		t.Fatalf("Get(%q) 未命中", fake.name)
	}

	ctx := context.Background()
	var refs []WorkRef
	var cur *Cursor
	for page := 0; page < 10; page++ { // 上界只是防死循环，正常 2 轮就结束
		got, err := p.Enumerate(ctx, cur)
		if err != nil {
			t.Fatalf("Enumerate: %v", err)
		}
		refs = append(refs, got.Refs...)
		if got.Next == nil {
			break
		}
		cur = got.Next
	}
	if len(refs) != 2 || refs[0].ID != "w1" || refs[1].ID != "w2" {
		t.Fatalf("枚举到 %v, 期望 w1 → w2 两页", refs)
	}

	detail, err := p.Work(ctx, refs[0])
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if detail.Work.Title != "假剧" || len(detail.Episodes) != 1 || detail.Episodes[0].No != 1 {
		t.Fatalf("WorkDetail = %+v", detail)
	}

	stream, err := p.Resolve(ctx, refs[0], 1)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if stream.URL != "https://cdn.example/ep1.mp4" {
		t.Fatalf("Stream = %+v", stream)
	}

	if err := p.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}

	want := []string{"enumerate", "enumerate", "work:w1", "resolve:w1", "health"}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("调用轨迹 = %v, 期望 %v", fake.calls, want)
	}
}

// Register 的三条拒绝路径都返回错误而不是 panic。
func TestRegistryRegisterRejects(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(nil); err == nil {
		t.Fatal("注册 nil 插件应当报错")
	}
	if err := reg.Register(&fakePlugin{}); err == nil {
		t.Fatal("注册空名插件应当报错")
	}
	if err := reg.Register(newFakePlugin()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Register(newFakePlugin()); err == nil {
		t.Fatal("重名插件应当报错")
	}
}

// Names 按注册顺序、返回副本；Get 未注册时 ok == false。
func TestRegistryNamesOrder(t *testing.T) {
	reg := NewRegistry()
	for _, name := range []string{"b", "a", "c"} {
		p := newFakePlugin()
		p.name = name
		if err := reg.Register(p); err != nil {
			t.Fatalf("Register(%q): %v", name, err)
		}
	}
	names := reg.Names()
	if strings.Join(names, ",") != "b,a,c" {
		t.Fatalf("Names() = %v, 期望注册顺序 b,a,c", names)
	}
	names[0] = "改写"
	if reg.Names()[0] != "b" {
		t.Fatal("Names() 返回了内部切片，调用方能改到注册表")
	}
	if _, ok := reg.Get("nope"); ok {
		t.Fatal("Get 未注册的名字返回了 ok=true")
	}
}

// 哨兵错误是五个互不相同的值：框架靠 errors.Is 分类，混在一起就没法路由。
func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{ErrUpstream, ErrRejected, ErrSchema, ErrNotFound, ErrNoStream}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Fatalf("哨兵 %v 与 %v 互相命中", a, b)
			}
		}
	}
}

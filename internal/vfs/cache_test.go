package vfs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

// TestListSeesWriteImmediately 覆盖失效点 1：同步落库后立刻 PROPFIND 必须看到新剧，
// 不能读到旧缓存。
func TestListSeesWriteImmediately(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	dir := Root + "/" + genreA

	before, err := f.tree.List(ctx, dir)
	if err != nil {
		t.Fatalf("List 预热: %v", err)
	}

	f.addDrama(t, store.Drama{Provider: "web", ProviderID: "new1", Title: "刚同步的剧", Year: 2026, Genre: genreA})

	after, err := f.tree.List(ctx, dir)
	if err != nil {
		t.Fatalf("List 写入后: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("写入后子项数 = %d, 期望 %d", len(after), len(before)+1)
	}
	want := dramaDirPath(genreA, "刚同步的剧 (2026)")
	for _, n := range after {
		if n.Path == want {
			return
		}
	}
	t.Fatalf("写入后没有新剧目录 %s: %v", want, after)
}

// TestListInvalidatedByMergeChunkSize 覆盖失效点 2 / 硬约束 5：改 merge_chunk_size 后
// 立刻生效，季目录里的虚拟文件按新分块重新派生。
func TestListInvalidatedByMergeChunkSize(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	season := f.dirA + "/" + seasonDir

	before, err := f.tree.List(ctx, season)
	if err != nil {
		t.Fatalf("List 预热: %v", err)
	}
	if got := countStrm(before); got != 2 {
		t.Fatalf("merge=1 时 .strm 数 = %d, 期望 2", got)
	}

	settings := store.NewSettings(f.db.DB)
	if err := settings.Set(ctx, store.KeyMergeChunkSize, "2"); err != nil {
		t.Fatalf("Set merge_chunk_size: %v", err)
	}

	after, err := f.tree.List(ctx, season)
	if err != nil {
		t.Fatalf("List 改设置后: %v", err)
	}
	if got := countStrm(after); got != 1 {
		t.Fatalf("merge=2 时 .strm 数 = %d, 期望 1（读到旧结构？）", got)
	}
}

func countStrm(nodes []Node) int {
	n := 0
	for _, node := range nodes {
		if !node.IsDir && len(node.Path) > len(".strm") && node.Path[len(node.Path)-len(".strm"):] == ".strm" {
			n++
		}
	}
	return n
}

// TestListRootAliasesShareCache 验证 "" 与 "短剧" 是同一个根目录、同一个缓存键。
func TestListRootAliasesShareCache(t *testing.T) {
	f := seed(t, "")
	ctx := context.Background()
	cs := newCountingStore(f.tree.st)
	tree := NewTree(cs, f.tree.settings, f.tree.id, f.tree.covers, f.tree.logger)

	if _, err := tree.List(ctx, ""); err != nil {
		t.Fatalf("List \"\": %v", err)
	}
	if _, err := tree.List(ctx, Root); err != nil {
		t.Fatalf("List %q: %v", Root, err)
	}
	if got := cs.count("ListGenres"); got != 1 {
		t.Fatalf("两次根目录列举查了 %d 次 ListGenres, 期望 1（未命中同一条缓存）", got)
	}
}

// TestDirCacheDoesNotCacheError 验证错误不入缓存：一次抖动之后能立刻恢复。
func TestDirCacheDoesNotCacheError(t *testing.T) {
	ctx := context.Background()
	c := newDirCache()
	boom := errors.New("boom")
	var calls atomic.Int32
	gen := func(context.Context) ([]Node, error) {
		if calls.Add(1) == 1 {
			return nil, boom
		}
		return []Node{{Path: "ok"}}, nil
	}

	if _, err := c.load(ctx, "k", 1, gen); !errors.Is(err, boom) {
		t.Fatalf("第一次 err = %v, 期望 boom", err)
	}
	nodes, err := c.load(ctx, "k", 1, gen)
	if err != nil {
		t.Fatalf("第二次 err = %v, 期望 nil", err)
	}
	if len(nodes) != 1 || nodes[0].Path != "ok" {
		t.Fatalf("第二次 nodes = %v", nodes)
	}
	if calls.Load() != 2 {
		t.Fatalf("gen 调用 %d 次, 期望 2（错误被缓存了？）", calls.Load())
	}
}

// TestDirCacheMissesOnRevisionChange 验证版本号一变就整体失效。
func TestDirCacheMissesOnRevisionChange(t *testing.T) {
	ctx := context.Background()
	c := newDirCache()
	var calls atomic.Int32
	gen := func(context.Context) ([]Node, error) {
		calls.Add(1)
		return []Node{{Path: "x"}}, nil
	}

	if _, err := c.load(ctx, "k", 7, gen); err != nil {
		t.Fatalf("rev=7: %v", err)
	}
	if _, err := c.load(ctx, "k", 7, gen); err != nil {
		t.Fatalf("rev=7 命中: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("同版本 gen 调用 %d 次, 期望 1", calls.Load())
	}
	if _, err := c.load(ctx, "k", 8, gen); err != nil {
		t.Fatalf("rev=8: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("换版本后 gen 调用 %d 次, 期望 2（返回了旧版本缓存？）", calls.Load())
	}
}

// TestDirCacheEvictsWhenOverBudget 覆盖失效点 5：节点总量超预算时按 LRU 淘汰。
func TestDirCacheEvictsWhenOverBudget(t *testing.T) {
	ctx := context.Background()
	c := newDirCacheWithBudget(3)
	two := func(context.Context) ([]Node, error) { return []Node{{Path: "a"}, {Path: "b"}}, nil }

	if _, err := c.load(ctx, "first", 1, two); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := c.load(ctx, "second", 1, two); err != nil {
		t.Fatalf("second: %v", err)
	}
	if _, ok := c.entries.Get("first"); ok {
		t.Fatal("超预算后 first 仍在缓存里")
	}
	if _, ok := c.entries.Get("second"); !ok {
		t.Fatal("刚写入的 second 被淘汰了")
	}
}

// TestDirCacheSingleFlight 覆盖失效点 4：并发未命中同一目录只算一次。
func TestDirCacheSingleFlight(t *testing.T) {
	c := newDirCache()
	var calls atomic.Int32
	release := make(chan struct{})
	gen := func(context.Context) ([]Node, error) {
		calls.Add(1)
		<-release
		return []Node{{Path: "x"}}, nil
	}

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := c.load(context.Background(), "k", 1, gen); err != nil {
				t.Errorf("load: %v", err)
			}
		}()
	}

	// 等 leader 进入 gen，再留一点时间让跟风者到达等待点。
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("并发 %d 个请求让 gen 跑了 %d 次, 期望 1", n, got)
	}
}

// TestDirCacheWaiterHonorsContext 覆盖失效点 4 的另一半：等待者不跟着 leader 一起
// 卡死，自己的 ctx 结束就返回。
func TestDirCacheWaiterHonorsContext(t *testing.T) {
	c := newDirCache()
	release := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = c.load(context.Background(), "k", 1, func(context.Context) ([]Node, error) {
			<-release
			return nil, nil
		})
	}()

	for {
		c.mu.Lock()
		_, ok := c.inflight[flightKey("k", 1)]
		c.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.load(ctx, "k", 1, func(context.Context) ([]Node, error) {
		t.Error("等待者不该自己再算一遍")
		return nil, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("等待者 err = %v, 期望 DeadlineExceeded", err)
	}

	close(release)
	<-leaderDone
}

// TestDirCacheDoesNotReuseOlderRevisionInFlight 覆盖跨版本漏洞：旧版本的计算还在进行中
// 时写入让版本号前进，新版本的请求必须自己算一次，不能复用旧版本的结果 —— 否则旧结构
// 会被当成新结构返回，并被响应缓存以新版本号固化。
func TestDirCacheDoesNotReuseOlderRevisionInFlight(t *testing.T) {
	c := newDirCache()
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		_, _ = c.load(context.Background(), "k", 1, func(context.Context) ([]Node, error) {
			close(oldStarted)
			<-releaseOld
			return []Node{{Path: "old"}}, nil
		})
	}()
	<-oldStarted

	var newCalls atomic.Int32
	nodes, err := c.load(context.Background(), "k", 2, func(context.Context) ([]Node, error) {
		newCalls.Add(1)
		return []Node{{Path: "new"}}, nil
	})
	if err != nil {
		t.Fatalf("新版本 load: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Path != "new" {
		t.Fatalf("新版本 load = %v, 期望 new（不能复用旧版本进行中的结果）", nodes)
	}
	if got := newCalls.Load(); got != 1 {
		t.Fatalf("新版本 gen 跑了 %d 次, 期望 1", got)
	}

	close(releaseOld)
	<-oldDone

	// 旧版本算完后，新版本请求仍必须重新算，不能拿到旧结构。
	nodes, err = c.load(context.Background(), "k", 2, func(context.Context) ([]Node, error) {
		return []Node{{Path: "newer"}}, nil
	})
	if err != nil {
		t.Fatalf("旧版本结束后 load: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Path != "newer" {
		t.Fatalf("旧版本结束后拿到 %v, 期望 newer（旧版本结果不能当新版本命中）", nodes)
	}
}

// TestDirCacheWaiterSurvivesLeaderError 覆盖同源缺陷：leader 失败（含 ctx 取消）时
// 等待者自己算一次，不把别人的错误当自己的结果。
func TestDirCacheWaiterSurvivesLeaderError(t *testing.T) {
	c := newDirCache()
	leaderStarted := make(chan struct{})
	releaseLeader := make(chan struct{})
	go func() {
		_, _ = c.load(context.Background(), "k", 1, func(context.Context) ([]Node, error) {
			close(leaderStarted)
			<-releaseLeader
			return nil, context.Canceled
		})
	}()
	<-leaderStarted

	go func() {
		time.Sleep(10 * time.Millisecond)
		close(releaseLeader)
	}()

	nodes, err := c.load(context.Background(), "k", 1, func(context.Context) ([]Node, error) {
		return []Node{{Path: "ok"}}, nil
	})
	if err != nil {
		t.Fatalf("等待者不该收到 leader 的错误: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Path != "ok" {
		t.Fatalf("等待者 nodes = %v, 期望 ok", nodes)
	}
}

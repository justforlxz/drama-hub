package vfs

import (
	"context"
	"strconv"
	"sync"

	"github.com/justforlxz/drama-hub/internal/lru"
)

// dirCacheBudget 是缓存里所有目录节点数之和的上限：21.8 万项造数场景下整棵树仍能
// 装下（约 13MB），再往上是淘汰而不是无限增长（docs/adr/0015「容量」）。
const dirCacheBudget = 300_000

// dirEntry 是缓存的一个目录：子项 + 计算它时的树结构版本号。
type dirEntry struct {
	rev   uint64
	nodes []Node
}

// dirCache 是 Tree 的分结构缓存：按目录缓存「直接子项」，按树结构版本号整体失效。
//
// 键是归一化后的目录路径（根为 ""），值带版本号：命中条件是「版本一致」，因此任何
// 影响结构的写入（store 递增版本号）都会让所有旧条目自动失效，不需要按写入路径
// 逐个摘除。同一目录的并发未命中由 single-flight 收敛为一次计算。
//
// 缓存只在内存里，不落盘；进程重启后从空开始，冷启动就是原来的现算路径
// （AGENTS.md 硬约束 1、4）。
type dirCache struct {
	entries  *lru.Cache[dirEntry]
	mu       sync.Mutex
	inflight map[string]*dirGen
}

// dirGen 是一次进行中的目录计算；等待者拿到 leader 的结果而不是自己再算一遍。
type dirGen struct {
	done  chan struct{}
	nodes []Node
	err   error
}

// newDirCache 构造空缓存。
func newDirCache() *dirCache {
	return newDirCacheWithBudget(dirCacheBudget)
}

// newDirCacheWithBudget 按给定节点预算构造缓存（测试用来验证淘汰）。
func newDirCacheWithBudget(budget int) *dirCache {
	return &dirCache{
		entries:  lru.New[dirEntry](budget, func(e dirEntry) int { return len(e.nodes) }),
		inflight: map[string]*dirGen{},
	}
}

// load 返回目录 key 在版本 rev 下的子项：命中直接返回，未命中调 gen 计算并缓存。
//
// gen 返回错误时不缓存（错误不是结构，缓存它会让一次数据库抖动变成持续不可用）。
// 并发未命中同一「目录 + 版本」时只有一个 goroutine 真正调 gen，其余等待它的结果
// 或自己的 ctx 结束 —— 既不重复全量计算，也不会相互拖死。
//
// inflight 的键带版本号：计算期间若有写入让版本号前进，新版本的请求必须自己算一次，
// 不能复用旧版本正在算的结果 —— 否则旧结构会被当成新结构返回，并被上层响应缓存以
// 新版本号固化，之后一直读到旧数据（docs/adr/0015「失效」）。
func (c *dirCache) load(ctx context.Context, key string, rev uint64, gen func(context.Context) ([]Node, error)) ([]Node, error) {
	if e, ok := c.entries.Get(key); ok && e.rev == rev {
		return e.nodes, nil
	}

	flight := flightKey(key, rev)
	c.mu.Lock()
	if f, ok := c.inflight[flight]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			if f.err != nil {
				// leader 失败（含它自己的 ctx 被取消）不传染给健康等待者：
				// 自己算一次，避免一个被取消的请求把并发请求变成 5xx。
				return c.generate(ctx, key, rev, gen)
			}
			return f.nodes, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &dirGen{done: make(chan struct{})}
	c.inflight[flight] = f
	c.mu.Unlock()

	nodes, err := c.generate(ctx, key, rev, gen)

	c.mu.Lock()
	delete(c.inflight, flight)
	f.nodes, f.err = nodes, err
	close(f.done)
	c.mu.Unlock()
	return nodes, err
}

// generate 调 gen，成功时把结果写进缓存（失败不入缓存）。
func (c *dirCache) generate(ctx context.Context, key string, rev uint64, gen func(context.Context) ([]Node, error)) ([]Node, error) {
	nodes, err := gen(ctx)
	if err == nil {
		c.entries.Put(key, dirEntry{rev: rev, nodes: nodes})
	}
	return nodes, err
}

// flightKey 让同一目录的不同版本各自单飞：旧版本的计算不能被新版本的请求搭便车。
func flightKey(key string, rev uint64) string {
	return strconv.FormatUint(rev, 10) + "\x00" + key
}

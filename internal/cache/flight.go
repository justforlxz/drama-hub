package cache

import (
	"context"
	"errors"
	"sync"
)

// flight 是一次进行中的生成：leader 写完 body / err 后 close(done)，等待者读同一份。
type flight struct {
	done chan struct{}
	body []byte
	err  error
}

// flightGroup 是 single-flight 的唯一实现：同一 id 的并发生成只做一次，其余等同一份结果。
//
// 两个实例：Cache（分片 / 关键帧索引）与 CoverCache（海报）。海报单独一组是因为
// 「同步本地化封面」与「请求 poster.jpg 按需回源」是同一个键上的同一件事——
// 它们共用一组才能让撞在一起的两个调用点只回源一次（MKACG-650）。
type flightGroup struct {
	mu      sync.Mutex // 只保护 flights；goroutine 之间不共享别的可变状态
	flights map[string]*flight
}

// newFlightGroup 构造一个空的 single-flight 组。
func newFlightGroup() *flightGroup {
	return &flightGroup{flights: make(map[string]*flight)}
}

// do 把同一 id 的并发生成合成一次：先到的是 leader，其余等它的结果。
//
// mutex 只保护 map（与 channel 分工，不混用）。leader 的 ctx 决定生成是否继续：
// 它被取消时等待者拿到同一个错误，各自的请求自己重试 —— 比在这里做生成的生命周期
// 管理便宜，也不会留下无人接收的结果。
//
// 但「leader 被取消」不等于「这块内容生成失败」：分片预热（MKACG-682）当 leader 时
// 会被前台请求抢走 ffmpeg 闸而取消，同一时刻前台正好可能在等这个键。把
// context.Canceled 原样交给等待者会被边界包映射成 502。所以等待者看到取消类错误时
// 重新领跑一次：缓存还没发布，重跑是唯一正确的结果来源。
//
// published 是「结果已发布、flight 尚未摘除」时的观察点，只给测试用：它在持锁状态下
// 调用，回调里只能做只读断言（或调不碰这把锁的 hit）。
func (g *flightGroup) do(ctx context.Context, id string, gen func() ([]byte, error), published func(id string)) ([]byte, error) {
	for {
		g.mu.Lock()
		if f, ok := g.flights[id]; ok {
			g.mu.Unlock()
			select {
			case <-f.done:
				if isCancellation(f.err) && ctx.Err() == nil {
					// 领跑者被取消，结果不是这块内容的真值。摘掉这条已关闭的 flight
					// 再自己领跑：不摘会陷在「看到同一个取消结果」的循环里。
					g.mu.Lock()
					if g.flights[id] == f {
						delete(g.flights, id)
					}
					g.mu.Unlock()
					continue
				}
				return f.body, f.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		f := &flight{done: make(chan struct{})}
		g.flights[id] = f
		g.mu.Unlock()

		// 先赋值再 close：close 建立 happens-before，等待者看不到半写的 body/err。
		f.body, f.err = gen()

		// 发布与摘除在同一把锁内，且发布在前：晚到者要么还能在 map 里看到这个 flight、
		// 等 done（结果已经可用，不重生成），要么看到它已消失 —— 而「已消失」意味着结果
		// 已发布，那时 hit() 必然命中。反过来（先摘除、解锁后再 close）会留出一个
		// 「map 里没有、结果又还没发布」的窗口，晚到者在窗口里当上第二个 leader，
		// 同一个分片被切两遍 —— MKACG-640 的 10 并发用例就是这么红的。
		g.mu.Lock()
		close(f.done)
		if published != nil {
			published(id)
		}
		delete(g.flights, id)
		g.mu.Unlock()
		return f.body, f.err
	}
}

// isCancellation 判定错误是否只是「生成被取消/超时」，而不是内容本身失败。
func isCancellation(err error) bool {
	return err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}

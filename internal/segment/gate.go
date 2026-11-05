package segment

import (
	"context"
	"sync"
)

// ffmpegGate 是 ffmpeg / ffprobe 子进程的并发闸：容量固定（ffmpegSlots），超出的
// 前台调用排队等待。
//
// 前台优先（MKACG-682）：槽位全被后台预热占着时，前台请求到达会取消正在跑的预热，
// 让它尽快退出并释放槽位；后台预热拿不到空槽位就跳过这一集，绝不排队堵在前台前面。
// 预热是可丢的后台工作，它存在的意义只是让前台更快，不能反过来拖慢前台。
type ffmpegGate struct {
	slots chan struct{}

	mu sync.Mutex
	// bgID / bgCancel 是在跑的后台预热任务的登记。bgID 让归还槽位的那一方只清理
	// 自己的登记，不会把后来者的取消函数清掉。
	bgID     uint64
	bgCancel context.CancelFunc
}

func newFFmpegGate(n int) *ffmpegGate {
	return &ffmpegGate{slots: make(chan struct{}, n)}
}

// acquire 前台取槽位：先试空位；没空位就取消在跑的后台预热，再排队等它退出。
func (g *ffmpegGate) acquire(ctx context.Context) error {
	select {
	case g.slots <- struct{}{}:
		return nil
	default:
	}
	g.preempt()
	select {
	case g.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release 归还前台占用的槽位。
func (g *ffmpegGate) release() { <-g.slots }

// acquireBackground 后台预热取槽位：没有空位就直接放弃（ok=false），从不排队。
//
// 成功时返回的 ctx 是预热这次生成的生命周期：前台来抢槽位时它会被取消
// （ffmpeg 子进程随之被杀），从而尽快让出槽位。
func (g *ffmpegGate) acquireBackground(ctx context.Context) (context.Context, func(), bool) {
	select {
	case g.slots <- struct{}{}:
	default:
		return ctx, func() {}, false
	}
	bg, cancel := context.WithCancel(ctx)
	g.mu.Lock()
	g.bgID++
	id := g.bgID
	g.bgCancel = cancel
	g.mu.Unlock()
	release := func() {
		g.mu.Lock()
		if g.bgID == id {
			g.bgCancel = nil
		}
		g.mu.Unlock()
		cancel()
		<-g.slots
	}
	return bg, release, true
}

// preempt 取消正在跑的后台预热（没有则空操作）。cancel 是幂等的，重复调用无副作用。
func (g *ffmpegGate) preempt() {
	g.mu.Lock()
	cancel := g.bgCancel
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// busy 报告当前是否有 ffmpeg 子进程在跑。只给「后台任务主动让路」的判断用。
func (g *ffmpegGate) busy() bool { return len(g.slots) != 0 }

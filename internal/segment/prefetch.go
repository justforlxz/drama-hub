package segment

import (
	"context"
	"sync"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
)

// 播放触发的整剧直链预取（docs/adr/0017-prefetch-and-stream-health.md）。
//
// 播放列表成功后，后台把这**整部剧**的直链申请好，让「拖到相邻块」落到
// episode_stream 的缓存命中，而不是现场等一次页面请求（冷启动第二刀）。
//
// 三条边界：
//   - 只走 Cutter.resolveMany（= stream.Resolver.DirectMany）：缓存判定、单飞、
//     落库、通道判据都只有那一份实现，预取不是第二条解析路径；
//   - 前台路径（Direct / DirectMany / ServePlaylist / ServeSegment）从不读预取
//     状态、不等预取结果，也不因预取失败改变任何行为；
//   - 进程里没有常驻循环：任务只在播放列表成功后由 kickPrefetch 起一次，跑完即止
//     （不轮询、不做整剧健康巡检）。

// 预取优先级（数字进日志，便于按顺序核对「当前块 → 下一块 → 其余」）。
const (
	tierCurrentBlock = 1 // 当前播放块的成员：块内直接拖拽
	tierNextBlock    = 2 // 下一块：拖到相邻块的第一片
	tierRestOfDrama  = 3 // 其余块（含已播过的块），按 ep_no 升序
)

// prefetchStopTimeout 是 StopPrefetch 等当前任务收尾的上限。
//
// 任务内每个出站调用都受进程 ctx 取消（预取用进程 ctx，不是播放请求 ctx），正常
// 远小于这个值；超时只放弃等待、不再拖住关机，进程退出后后台任务随之消失。
const prefetchStopTimeout = 5 * time.Second

// prefetchRun 是一次预取任务的生命周期句柄。
//
// 「不再起新批」与「中断在途批」是两个不同的信号，必须分开：
//   - stop 关闭 = 只丢弃尚未开始的批次（换剧就是这种情况）；
//   - cancel 调用 = 中断在途批，只有进程停机才做。
//
// 混在一起会把在途批的解析所有权一起取消，前台等同一集的请求读到
// context.Canceled、变 502（ADR-0017 §7）。
type prefetchRun struct {
	dramaID int64
	stop    chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
}

// stopped 报告「不再起新批」的信号是否已经发出。
//
// 必须先查它再做可取消的 select：两者同时就绪时 select 随机选，会多起一批。
func (r *prefetchRun) stopped() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

// prefetchBatch 是一个执行单元：一块（或按退避过滤后剩下的成员）+ 优先级。
//
// 一次任务把整部剧切成若干批：批 = 一个合并块。批级是 `DirectMany` 的粒度，
// 也是失败的粒度（批量解析要么整块成功、要么整块报错，见 ADR-0017「风险」）。
type prefetchBatch struct {
	eps  []store.Episode
	tier int
}

// StartPrefetch 把进程级 ctx 交给播放面，之后 kickPrefetch 才能起任务。
//
// 幂等：只有第一次生效（重复调用不替换已有的进程 ctx）。ctx 是预取任务的根，
// 不是某个播放请求的 ctx —— 请求结束/切剧只是不再起新批，不会取消在途批，
// 也就不会污染正在等直链的前台。
func (c *Cutter) StartPrefetch(ctx context.Context) {
	c.pfMu.Lock()
	defer c.pfMu.Unlock()
	if c.pfCtx == nil {
		c.pfCtx = ctx
	}
}

// StopPrefetch 停掉当前预取任务并等它收尾（最多 prefetchStopTimeout）。
//
// 这是**停机**路径：先关 stop 丢弃尚未开始的批次，再 cancel 中断在途批（进程要走，
// 不再等它跑完）。换剧不走这里 —— 换剧只关 stop，在途批用旧任务自己的 ctx 跑完，
// 解析所有权不会被取消，前台等同一集的请求不会读到 context.Canceled。
func (c *Cutter) StopPrefetch() {
	c.stopSegmentPrefetch()

	c.pfMu.Lock()
	run := c.pfRun
	c.pfRun = nil
	c.pfCtx = nil
	c.pfMu.Unlock()

	if run == nil {
		return
	}
	close(run.stop)
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(prefetchStopTimeout):
		c.logger.Warn("prefetch run did not stop in time",
			"drama", run.dramaID, "timeout", prefetchStopTimeout)
	}
}

// kickPrefetch 在播放列表成功后为这部剧起一次后台预取（非阻塞）。
//
// 只加锁、起 goroutine，不查库：未调用 StartPrefetch（单元测试默认）时零副作用。
// 同一部剧已经在跑就不重复起（单飞）；换剧时只关旧任务的 stop（不再起新批），
// 不 cancel —— 在途批要继续跑完（ADR-0017 §7）。
func (c *Cutter) kickPrefetch(dramaID int64, current []store.Episode) {
	if dramaID <= 0 || len(current) == 0 {
		return
	}
	c.pfMu.Lock()
	if c.pfCtx == nil {
		c.pfMu.Unlock()
		return
	}
	if c.pfRun != nil && c.pfRun.dramaID == dramaID {
		c.pfMu.Unlock()
		return
	}
	// 复制一份成员：goroutine 的生命周期可能长于本次请求，不能读请求侧还在用的切片。
	eps := append([]store.Episode(nil), current...)
	ctx, cancel := context.WithCancel(c.pfCtx)
	run := &prefetchRun{
		dramaID: dramaID,
		stop:    make(chan struct{}),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	prev := c.pfRun
	c.pfRun = run
	if prev != nil {
		close(prev.stop)
	}
	c.pfMu.Unlock()

	go c.runPrefetch(ctx, run, prev, eps)
}

// clearPrefetchRun 在任务结束时放开单飞占位；只有自己还是「当前任务」时才清，
// 不误清新任务（换剧后旧任务的收尾晚于新任务启动）。
func (c *Cutter) clearPrefetchRun(run *prefetchRun) {
	c.pfMu.Lock()
	if c.pfRun == run {
		c.pfRun = nil
	}
	c.pfMu.Unlock()
}

// runPrefetch 是一个预取任务的唯一入口：等旧任务收尾 → 读配置 → 现算计划 → 按并发上限跑批。
//
// 配置在任务开始时读一次（不在每批现读）：一次任务的并发与退避口径必须一致，
// 中途改配置留给下一次播放触发的任务（ADR-0017）。
func (c *Cutter) runPrefetch(ctx context.Context, run *prefetchRun, prev *prefetchRun, current []store.Episode) {
	defer close(run.done)
	defer c.clearPrefetchRun(run)

	// 换剧时旧任务在途的那一批还在跑：等它收尾再开始，让并发上限是**进程级**的，
	// 而不是每个任务各算一份（否则切剧瞬间会翻倍外呼）。等待有界：旧任务只会跑完
	// 手上那一批（≤ 一个合并块），不再起新批。
	if prev != nil {
		select {
		case <-prev.done:
		case <-ctx.Done(): // 停机不陪旧任务等
		}
	}
	if run.stopped() || ctx.Err() != nil {
		c.logger.InfoContext(ctx, "prefetch run cancelled", "drama", run.dramaID)
		return
	}

	concurrency, err := c.settings.Int(ctx, store.KeyPrefetchConcurrency)
	if err != nil {
		c.logger.WarnContext(ctx, "prefetch config read failed",
			"drama", run.dramaID, "err", err)
		return
	}
	if concurrency <= 0 {
		// 0 = 关闭预取：不查库、不外呼，行为与没有这个功能一致。
		c.logger.DebugContext(ctx, "prefetch disabled", "drama", run.dramaID)
		return
	}
	retrySec, err := c.settings.Int(ctx, store.KeyPrefetchRetrySec)
	if err != nil {
		c.logger.WarnContext(ctx, "prefetch config read failed",
			"drama", run.dramaID, "err", err)
		return
	}

	blocks, err := c.st.DramaBlocks(ctx, run.dramaID)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.WarnContext(ctx, "prefetch plan failed", "drama", run.dramaID, "err", err)
		}
		return
	}
	fails, err := c.st.StreamFailures(ctx, run.dramaID)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.WarnContext(ctx, "prefetch plan failed", "drama", run.dramaID, "err", err)
		}
		return
	}
	plan := prefetchPlan(blocks, current, fails, time.Now().UTC())
	if len(plan) == 0 {
		c.logger.DebugContext(ctx, "prefetch nothing to do", "drama", run.dramaID)
		return
	}

	c.logger.InfoContext(ctx, "prefetch run started",
		"drama", run.dramaID, "batches", len(plan), "concurrency", concurrency)

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	// 先拿信号量再起 goroutine：并发上限约束的是「在途批次」，不是「已起 goroutine」。
	// 拿不到槽位的批次在这里等（换剧/停机都会把它丢掉），因此尚未开始的批次不再外呼。
	// 先查 stopped() 再做 select：两者同时就绪时 select 随机选，会多起一批。
batches:
	for _, b := range plan {
		if run.stopped() || ctx.Err() != nil {
			break batches
		}
		select {
		case sem <- struct{}{}:
		case <-run.stop:
			break batches
		case <-ctx.Done():
			break batches
		}
		wg.Add(1)
		go func(b prefetchBatch) {
			defer wg.Done()
			defer func() { <-sem }()
			c.prefetchBatch(ctx, b, retrySec)
		}(b)
	}
	wg.Wait()

	if run.stopped() || ctx.Err() != nil {
		c.logger.InfoContext(ctx, "prefetch run cancelled", "drama", run.dramaID)
		return
	}
	c.logger.InfoContext(ctx, "prefetch run finished", "drama", run.dramaID, "batches", len(plan))
}

// prefetchBatch 执行一批：解析整块成员 → 成功则清失败记录（下一块顺带补时长），
// 失败则把退避记录写进 episode_stream_fail。
//
// 取消导致的失败**不记**：换剧取消不是上游故障，记下来会让这些集在退避期内不被
// 任何任务碰（而它们其实没被真正尝试过）。
func (c *Cutter) prefetchBatch(ctx context.Context, b prefetchBatch, retrySec int) {
	directs, err := c.resolveMany(ctx, b.eps)
	if err != nil {
		if ctx.Err() != nil {
			c.logPrefetchBatch(ctx, b, "cancelled", "")
			return
		}
		reason := stream.FailureReason(err)
		now := time.Now().UTC()
		failedAt := now.Format(time.RFC3339)
		retryAfter := now.Add(time.Duration(retrySec) * time.Second).Format(time.RFC3339)
		for _, ep := range b.eps {
			if werr := c.st.RecordStreamFailure(ctx, ep.ID, reason, failedAt, retryAfter); werr != nil {
				c.logger.WarnContext(ctx, "prefetch failure record failed",
					"episode_id", ep.ID, "err", werr)
			}
		}
		c.logPrefetchBatch(ctx, b, "failed", reason)
		return
	}
	if ctx.Err() != nil {
		// 解析成功了但任务已被取消：不再写库、不再补时长，收尾即可。
		c.logPrefetchBatch(ctx, b, "cancelled", "")
		return
	}

	ids := make([]int64, len(b.eps))
	for i, ep := range b.eps {
		ids[i] = ep.ID
	}
	if err := c.st.ClearStreamFailures(ctx, ids); err != nil {
		c.logger.WarnContext(ctx, "prefetch failure clear failed",
			"drama", b.eps[0].DramaID, "err", err)
	}
	c.logPrefetchBatch(ctx, b, "ok", "")

	// 只给「下一块」补时长：当前块播放列表已经探过，其余块收益低且工作量无界。
	// 时长是拖到下一块时 playlist 从 503/慢探测变成零外呼的关键（ADR-0017）。
	if b.tier == tierNextBlock {
		c.backfillPrefetchDurations(ctx, b.eps, directs)
	}
}

// backfillPrefetchDurations 为下一块里还没时长的集补一次 ffprobe（尽力而为）。
//
// 只在这时候做：ffmpeg 闸空闲（有前台转封装在跑就让路，预取不抢前台的进程槽位）、
// 该集时长仍为 0。失败只记 debug：下一次播放列表仍会按原路径现探测，功能不受影响。
func (c *Cutter) backfillPrefetchDurations(ctx context.Context, eps []store.Episode, directs []stream.Direct) {
	if c.gate.busy() {
		return
	}
	for i, ep := range eps {
		if ep.DurationSec > 0 || i >= len(directs) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if _, err := c.ensureDuration(ctx, ep, directs[i]); err != nil {
			c.logger.DebugContext(ctx, "prefetch duration backfill failed",
				"episode_id", ep.ID, "err", err)
		}
	}
}

// logPrefetchBatch 每批恰好一条日志（ok / failed / cancelled）。
//
// 字段即验收口径：tier 与 ep_from/ep_to 的先后顺序就是「当前块 → 下一块 → 其余」。
// 默认并发 1 时日志顺序严格等于计划顺序；并发 >1 时只保证「先开始的批次先打日志」。
func (c *Cutter) logPrefetchBatch(ctx context.Context, b prefetchBatch, outcome, reason string) {
	attrs := []any{
		"drama", b.eps[0].DramaID,
		"tier", b.tier,
		"ep_from", b.eps[0].EpNo,
		"ep_to", b.eps[len(b.eps)-1].EpNo,
		"eps", len(b.eps),
		"outcome", outcome,
	}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	if outcome == "failed" {
		c.logger.WarnContext(ctx, "prefetch batch", attrs...)
		return
	}
	c.logger.InfoContext(ctx, "prefetch batch", attrs...)
}

// prefetchPlan 把整部剧切成按优先级排序的批次（纯函数：不碰库、不读时钟）。
//
// 顺序：当前块 → 下一块（当前是最后一块时没有）→ 其余块（含更早的块，按 ep_no 升序）。
// 退避期内的集从批次里剔除；整块都被剔除时这一批消失。缓存命中与否**不在这里**过滤
// —— 那是 DirectMany 的判定，计划只看「哪些集允许被尝试」（ADR-0017）。
//
// current 是本次播放单元的成员（播放列表刚用过的那一块）：按首集主键定位它在
// blocks 里的位置，因此 merge_chunk_size 中途被改也能落到正确的块（定位不到就不预取，
// 不猜一个块）。
func prefetchPlan(blocks [][]store.Episode, current []store.Episode, fails map[int64]store.StreamFailure, now time.Time) []prefetchBatch {
	idx := currentBlockIndex(blocks, current)
	if idx < 0 {
		return nil
	}
	out := make([]prefetchBatch, 0, len(blocks))
	add := func(block []store.Episode, tier int) {
		eps := make([]store.Episode, 0, len(block))
		for _, ep := range block {
			if f, ok := fails[ep.ID]; ok && retryPending(f, now) {
				continue
			}
			eps = append(eps, ep)
		}
		if len(eps) > 0 {
			out = append(out, prefetchBatch{eps: eps, tier: tier})
		}
	}

	add(blocks[idx], tierCurrentBlock)
	if idx+1 < len(blocks) {
		add(blocks[idx+1], tierNextBlock)
	}
	for i, block := range blocks {
		if i == idx || i == idx+1 {
			continue
		}
		add(block, tierRestOfDrama)
	}
	return out
}

// currentBlockIndex 返回当前播放块在 blocks 里的下标；找不到返回 -1。
func currentBlockIndex(blocks [][]store.Episode, current []store.Episode) int {
	if len(current) == 0 {
		return -1
	}
	target := current[0].ID
	for i, block := range blocks {
		for _, ep := range block {
			if ep.ID == target {
				return i
			}
		}
	}
	return -1
}

// retryPending 报告这一集的退避是否还没到期。
//
// retry_after 解析不了就按「可以重试」处理：不因为一条坏记录让一集永远不被预取。
func retryPending(f store.StreamFailure, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, f.RetryAfter)
	if err != nil {
		return false
	}
	return at.After(now)
}

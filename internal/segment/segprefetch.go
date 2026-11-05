package segment

import (
	"context"
	"errors"
	"time"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/store"
)

// 播放触发的分片预热（MKACG-682，docs/adr/0018-segment-prefetch.md）。
//
// 与整剧直链预取（prefetch.go / ADR-0017）是两件事：那一个只申请直链、不生成字节；
// 这一个真的跑 ffmpeg 整集转封装、把分片落进缓存，让「拖到同块下一集 / 下一块」
// 的首片请求直接命中。
//
// 边界：
//   - 触发点只有播放列表成功后（ServePlaylist），分片端点不触发 —— 拖动不重启任务；
//   - 只预热当前块里当前集**之后**的成员 + 后面 segment_prefetch_blocks 个块，
//     有界、顺序执行（同一时刻一个 ffmpeg），不越上游闸；
//   - 与前台共用 single-flight 与 ffmpeg 闸；闸被前台占满时这一集直接跳过，
//     绝不排队堵在前台前面；
//   - 预热生成的分片标 prefetched，容量吃紧时先淘汰（docs/CACHE-MODEL.md）；
//   - 换集 / 换剧 / 播放单元切换 / 进程停机 / 前台静默超过 segmentPrefetchIdle，
//     都取消任务；取消后不再生成新分片。

// segmentPrefetchIdle 是「播放已经把这一轮放完 / 客户端已经断开」的判定窗口：
// 距上一次分片请求超过它，预热就不再开始下一集。预热只是提前量，没人看就不该继续
// 吃 CPU 与上游配额（固定值，不做成配置项）。
const segmentPrefetchIdle = 30 * time.Second

// errGateBusy 表示 ffmpeg 闸被前台占满，预热这一集直接跳过（不是失败，不记 Warn）。
var errGateBusy = errors.New("ffmpeg gate busy")

// segmentPrefetchRun 是一次分片预热任务的生命周期句柄。
//
// 与前台的让路是「立即取消」而不是「跑完这批」：预热占用 ffmpeg 槽位，前台要用的
// 时候必须马上让出来。取消可能落在某个前台也在等的 single-flight 上，cell 层
// （cache.flightGroup）会把取消当成「领跑者作废」并让等待者重新领跑，不会把
// context.Canceled 当成内容失败回报给播放端。
type segmentPrefetchRun struct {
	unitID  string
	dramaID int64
	cancel  context.CancelFunc
	done    chan struct{}
}

// kickSegmentPrefetch 在播放列表成功后为该播放单元起一次后台分片预热（非阻塞）。
//
// 同一单元已经在跑就跳过（单飞）；换单元 / 换剧取消旧任务。未调用 StartPrefetch
// （单元测试默认）时零副作用。
func (c *Cutter) kickSegmentPrefetch(uid string, eps []store.Episode) {
	if len(eps) == 0 {
		return
	}
	c.spMu.Lock()
	if c.pfCtx == nil {
		c.spMu.Unlock()
		return
	}
	if run := c.spRun; run != nil && run.unitID == uid {
		c.spMu.Unlock()
		return
	}
	prev := c.spRun
	run := &segmentPrefetchRun{unitID: uid, dramaID: eps[0].DramaID, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(c.pfCtx)
	run.cancel = cancel
	c.spRun = run
	c.spMu.Unlock()
	if prev != nil {
		prev.cancel()
	}
	go c.runSegmentPrefetch(ctx, run, eps)
}

// stopSegmentPrefetch 取消当前分片预热并等它收尾（停机路径）。
func (c *Cutter) stopSegmentPrefetch() {
	c.spMu.Lock()
	run := c.spRun
	c.spRun = nil
	c.spMu.Unlock()
	if run == nil {
		return
	}
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(prefetchStopTimeout):
		c.logger.Warn("segment prefetch did not stop in time", "unit", run.unitID)
	}
}

// clearSegmentPrefetchRun 只清自己的登记，避免把后来者的任务抹掉。
func (c *Cutter) clearSegmentPrefetchRun(run *segmentPrefetchRun) {
	c.spMu.Lock()
	if c.spRun == run {
		c.spRun = nil
	}
	c.spMu.Unlock()
}

func (c *Cutter) runSegmentPrefetch(ctx context.Context, run *segmentPrefetchRun, eps []store.Episode) {
	defer close(run.done)
	defer c.clearSegmentPrefetchRun(run)

	// 提前量每个任务开始时读一次（硬约束 5：热改后下一次播放列表生效）。
	ahead, err := c.settings.Int(ctx, store.KeySegmentPrefetchBlocks)
	if err != nil {
		c.logger.WarnContext(ctx, "segment prefetch setting unavailable", "err", err)
		return
	}
	if ahead <= 0 {
		return // 0 = 关闭
	}
	blocks, err := c.st.DramaBlocks(ctx, run.dramaID)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.WarnContext(ctx, "segment prefetch blocks unavailable", "drama", run.dramaID, "err", err)
		}
		return
	}
	plan := segmentPrefetchPlan(blocks, eps, ahead)
	if len(plan) == 0 {
		return
	}
	c.logger.InfoContext(ctx, "segment prefetch run started",
		"drama", run.dramaID, "unit", run.unitID, "eps", len(plan))

	for _, ep := range plan {
		if ctx.Err() != nil {
			break
		}
		if c.lastPlayIdle() {
			// 前台已经静默（播放结束 / 客户端断开）：不再开始下一集。
			c.logger.InfoContext(ctx, "segment prefetch run cancelled",
				"drama", run.dramaID, "unit", run.unitID, "reason", "idle")
			return
		}
		if err := c.prefetchSegment(ctx, ep); err != nil {
			if ctx.Err() != nil {
				break
			}
			if errors.Is(err, errGateBusy) {
				continue
			}
			// 单集失败不扩散：后面的集继续试，下一次播放列表还会重新触发。
			c.logger.WarnContext(ctx, "segment prefetch failed", "episode_id", ep.ID, "err", err)
		}
	}
	if ctx.Err() != nil {
		c.logger.InfoContext(ctx, "segment prefetch run cancelled",
			"drama", run.dramaID, "unit", run.unitID, "reason", "cancelled")
		return
	}
	c.logger.InfoContext(ctx, "segment prefetch run finished",
		"drama", run.dramaID, "unit", run.unitID)
}

// prefetchSegment 把一集转封装进缓存（标记 prefetched）。
//
// 已经有本地字节就直接返回：预热不重复生成，也不碰「已经被播过」的条目。
func (c *Cutter) prefetchSegment(ctx context.Context, ep store.Episode) error {
	channel, direct, resolved, err := c.episodeChannel(ctx, ep)
	if err != nil {
		return keepCancel(ctx, err)
	}
	spec := cache.Spec{
		Kind: store.ArtifactKindSegment,
		Key:  cache.SegmentKey(episodeUID(ep.ID), channel),
		Ext:  segmentSuffix,
	}
	if ok, err := c.caches.Has(ctx, spec); err != nil || ok {
		return err
	}
	// 闸与 single-flight 的先后：先取后台槽位再进单飞。槽位被前台占满就整集跳过；
	// 单飞内部如果前台正在生成同一个键，我们只是等它，等的时候占着一个槽位——
	// 前台需要槽位时会 preempt 取消我们，所以不会把它堵死。
	bg, release, ok := c.gate.acquireBackground(ctx)
	if !ok {
		return errGateBusy
	}
	defer release()

	return c.caches.Prefetch(bg, spec, func(ctx context.Context) ([]byte, error) {
		src := direct
		if !resolved {
			var rerr error
			if src, rerr = c.resolve(ctx, ep); rerr != nil {
				return nil, keepCancel(ctx, resolveFailure(ep.ID, rerr))
			}
		}
		out, err := remux(ctx, c.logger, src, c.fetchHeaders(src))
		if err != nil && errors.Is(err, errRejected) {
			// 已经被取消就不要再去作废/重解析一次：那会白烧一次上游配额。
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// 与前台同一条自愈路径：作废这一集的直链、重解析、重试一次（ADR-0016）。
			fresh, refreshErr := c.refreshRejected(ctx, ep, channel)
			if refreshErr != nil {
				return nil, keepCancel(ctx, refreshErr)
			}
			src = fresh
			out, err = remux(ctx, c.logger, src, c.fetchHeaders(src))
		}
		if err = keepCancel(ctx, err); err != nil {
			return nil, err
		}
		c.touchStream(ctx, ep.ID)
		c.logger.InfoContext(ctx, "segment prefetched",
			"episode_id", ep.ID, "channel", channel, "bytes", len(out))
		return out, nil
	})
}

// keepCancel 把「被取消」从各条失败路径里还原出来。
//
// 预热闭包里三个失败出口（resolve / refreshRejected / remux）各自都会把取消折成
// 上游错误：`resolveFailure` 拼的是原文 + `httpx.ErrUpstream`，`remux` 只会包成
// `ffmpeg failed: ErrUpstream`。而 single-flight 的等待者只认
// `errors.Is(err, context.Canceled)`；不还原，前台等同一个键的请求就会收到 502。
// 这些函数的全局语义不能动（前台 403/501/502/超时规则不跟着变），所以只在预热
// 闭包里还原（MKACG-682 代码审查 blocker）。
func keepCancel(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// segmentPrefetchPlan 返回要预热的集：当前块里当前集之后的成员，然后是后面 ahead 个
// 块（按块与集序）。纯函数，不进库、不发请求。
//
// 当前集本身不预热：它已经在播，走的是前台生成路径。
func segmentPrefetchPlan(blocks [][]store.Episode, current []store.Episode, ahead int) []store.Episode {
	if len(current) == 0 || ahead <= 0 {
		return nil
	}
	idx := -1
	start := 0
	for i, block := range blocks {
		for j, ep := range block {
			if ep.ID == current[0].ID {
				idx, start = i, j+1
				break
			}
		}
		if idx >= 0 {
			break
		}
	}
	if idx < 0 {
		return nil
	}
	plan := make([]store.Episode, 0)
	plan = append(plan, blocks[idx][start:]...)
	for i := idx + 1; i < len(blocks) && i <= idx+ahead; i++ {
		plan = append(plan, blocks[i]...)
	}
	return plan
}

// markPlayed 记录一次「播放面还在被使用」（播放列表或分片成功写出）。
//
// 分片预热用它判断前台是否已经静默：超过 segmentPrefetchIdle 没有新请求，
// 就认为播放结束 / 客户端断开，不再开始下一集。
func (c *Cutter) markPlayed() {
	c.lastPlay.Store(time.Now().UnixNano())
}

// lastPlayIdle 报告前台是否已经超过 segmentPrefetchIdle 没有请求分片。
func (c *Cutter) lastPlayIdle() bool {
	last := c.lastPlay.Load()
	if last == 0 {
		return true
	}
	return time.Since(time.Unix(0, last)) > segmentPrefetchIdle
}

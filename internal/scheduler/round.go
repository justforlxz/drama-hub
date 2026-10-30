// Package scheduler 的一轮执行：三段 + 状态写回。这是唯一的写库入口，也是唯一的错误汇聚点。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/store"
)

// roundStats 是一轮的可观察量：三段各自的耗时与处理量，每轮一条结构化日志。
//
// 三段分开计时是契约要求（docs/SCHEDULER-MODEL.md「三段」）：枚举是常数成本、
// 新剧灌库是首次全量的几十小时、存量轮转是每轮一个批次，混在一起就看不出一轮
// 的时间花在哪。
type roundStats struct {
	enumerate time.Duration
	ingest    time.Duration
	rotate    time.Duration
	refs      int // 枚举到的作品标识数
	ingested  int // 新剧灌库成功数
	rotated   int // 存量轮转成功数
	failed    int // 单部失败数（跳过，不中止本轮）
	unknown   int // 未知错误数（按 ErrUpstream 处理，额外计数）
	cursor    string
	// record 是要记进 last_error 的单部失败（ErrUpstream / 未知错误）。它不影响
	// 「本轮是否完成」：单部失败只跳过这一部，本轮照常收尾并推进 next_due。
	record error
}

// runRound 跑一轮并写回 source_state。
//
// 状态写回用 context.WithoutCancel：本轮超时也要把 last_run_at 与游标落库
// （「单轮超时不清游标」）；进程退出（父 ctx 被取消）不把它记成源故障。
func (s *Scheduler) runRound(parent context.Context, p plugin.Plugin, cfg sourceConfig) {
	start := s.now()
	ctx, cancel := context.WithTimeout(parent, cfg.timeout)
	defer cancel()

	stats, roundErr := s.execute(ctx, p, cfg)
	s.saveState(context.WithoutCancel(ctx), p.Name(), start, stats, roundErr, parent.Err() != nil)

	level := slog.LevelInfo
	if roundErr != nil {
		level = slog.LevelWarn
	}
	s.logger.Log(ctx, level, "一轮结束",
		"source", p.Name(),
		"enumerate_ms", stats.enumerate.Milliseconds(),
		"ingest_ms", stats.ingest.Milliseconds(),
		"rotate_ms", stats.rotate.Milliseconds(),
		"refs", stats.refs,
		"ingested", stats.ingested,
		"rotated", stats.rotated,
		"failed", stats.failed,
		"unknown_errors", stats.unknown,
		"cursor", stats.cursor,
		"err", roundErr,
	)
}

// execute 跑三段：增量枚举 → 新剧灌库 → 存量轮转。
//
// 枚举失败不丢已枚举到的标识：它们照常进第二段，本轮仍记为失败。超时/被拒后
// ingest 会因为 ctx 已取消而立即收尾，不再逐部尝试。
// panic 在这里被 recover 成一轮失败（状态页可见），不拖垮进程。
func (s *Scheduler) execute(ctx context.Context, p plugin.Plugin, cfg sourceConfig) (stats roundStats, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("一轮 panic: %v", r)
		}
	}()

	name := p.Name()
	syncer := s.syncers[name]
	if syncer == nil {
		return stats, fmt.Errorf("没有源 %s 的写库实现", name)
	}
	st, loadErr := s.st.LoadState(ctx, name)
	if loadErr != nil && !errors.Is(loadErr, store.ErrNotFound) {
		return stats, fmt.Errorf("读数据源状态: %w", loadErr)
	}
	stats.cursor = st.Cursor

	// 段 1：增量枚举。
	began := time.Now()
	refs, cursor, enumErr := s.enumerate(ctx, p, st.Cursor)
	stats.enumerate = time.Since(began)
	stats.cursor = cursor
	stats.refs = len(refs)

	// 本轮已经到期/被取消：不再进第二、三段，直接收尾（游标已按最后成功的位置写好）。
	if ctx.Err() != nil {
		return stats, enumErr
	}

	// 段 2：新剧灌库（枚举失败也把已枚举到的标识灌进去）。
	var (
		abortErr error
		sqlErr   error
	)
	began = time.Now()
	newIDs, diffErr := s.newIDs(ctx, name, refs)
	if diffErr != nil {
		sqlErr = diffErr
	} else {
		res := s.ingest(ctx, syncer, newIDs)
		stats.ingested = res.wrote
		stats.failed += res.failed
		stats.unknown += res.unknown
		abortErr = res.abort
		stats.record = res.record
	}
	stats.ingest = time.Since(began)

	// 段 3：存量轮转（batch=0 = 关闭；本轮已被中止则不跑）。
	began = time.Now()
	if abortErr == nil && sqlErr == nil && ctx.Err() == nil {
		ids, listErr := s.st.StaleProviderIDs(ctx, name, cfg.batch)
		if listErr != nil {
			sqlErr = listErr
		} else {
			res := s.ingest(ctx, syncer, ids)
			stats.rotated = res.wrote
			stats.failed += res.failed
			stats.unknown += res.unknown
			abortErr = firstErr(abortErr, res.abort)
			stats.record = firstErr(stats.record, res.record)
		}
	}
	stats.rotate = time.Since(began)

	return stats, firstErr(abortErr, sqlErr, enumErr)
}

// enumerate 从游标起推进枚举，直到走完（Next==nil）、出错或本轮 ctx 到期。
//
// 返回的 cursor 是下一轮该从哪继续：走完 → ""（下一轮从起点重来；sitemap 的
// 「增量」就是每轮一次完整枚举）；被砍断 → 最后一个成功推进的位置，游标只前推、
// 不清零（docs/SCHEDULER-MODEL.md「超时」）。
func (s *Scheduler) enumerate(ctx context.Context, p plugin.Plugin, start string) (refs []plugin.WorkRef, cursor string, err error) {
	cursor = start
	var cur *plugin.Cursor
	if start != "" {
		cur = &plugin.Cursor{Token: start}
	}
	seen := make(map[string]struct{})
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return refs, cursor, ctxErr
		}
		page, pageErr := p.Enumerate(ctx, cur)
		if pageErr != nil {
			return refs, cursor, fmt.Errorf("枚举 %s: %w", p.Name(), pageErr)
		}
		for _, ref := range page.Refs {
			// 契约硬性要求 6：WorkRef.Plugin 必须等于 Name()，不等是插件实现 bug。
			if ref.Plugin != "" && ref.Plugin != p.Name() {
				return refs, cursor, fmt.Errorf("枚举 %s: 返回的 WorkRef.Plugin=%q: %w",
					p.Name(), ref.Plugin, plugin.ErrSchema)
			}
			if ref.ID == "" {
				continue // 空标识不可能是稳定身份，丢掉而不是写进库
			}
			if _, dup := seen[ref.ID]; dup {
				continue
			}
			seen[ref.ID] = struct{}{}
			refs = append(refs, plugin.WorkRef{Plugin: p.Name(), ID: ref.ID})
		}
		if page.Next == nil {
			return refs, "", nil
		}
		// 非空 Page 却不让游标推进 = 插件 bug；照它的话循环不会结束。
		if page.Next.Token == "" || (cur != nil && page.Next.Token == cur.Token) {
			return refs, cursor, fmt.Errorf("枚举 %s: 插件返回的游标没有推进（%q）: %w",
				p.Name(), page.Next.Token, plugin.ErrSchema)
		}
		cur = page.Next
		cursor = page.Next.Token
	}
}

// newIDs 算「库里还没有的标识」：枚举结果与库内身份的差集。
func (s *Scheduler) newIDs(ctx context.Context, provider string, refs []plugin.WorkRef) ([]string, error) {
	existing, err := s.st.ExistingProviderIDs(ctx, provider)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if _, ok := existing[ref.ID]; ok {
			continue
		}
		out = append(out, ref.ID)
	}
	return out, nil
}

// ingestResult 是逐部同步的汇总。
type ingestResult struct {
	wrote   int
	failed  int
	unknown int
	// record 是要记进 last_error 的第一条单部失败（ErrUpstream / 未知错误）；
	// ErrNotFound / ErrNoStream 只是跳过，不算失败原因。
	record error
	// abort 非 nil 表示本轮被中止：ErrRejected，或同一轮内连续 ErrSchema 达上限。
	abort error
}

// ingest 逐部同步 ids：worker 数 = min(4, max(1, NumCPU/2))，一部剧一个事务。
//
// 错误按 docs/PLUGIN-CONTRACT.md「错误语义」分级：ErrNotFound / ErrNoStream 只跳过；
// ErrUpstream / 未知错误跳过并记一条；ErrRejected 立刻中止本轮；ErrSchema 连续
// maxConsecutiveSchemaErrors 次（在一次逐部同步内，不跨段累计）也中止本轮。ctx 被取消（本轮超时/进程退出）
// 时立即收尾，不把因此失败的剧算成失败。
func (s *Scheduler) ingest(ctx context.Context, syncer Syncer, ids []string) ingestResult {
	var (
		res         ingestResult
		mu          sync.Mutex
		consecutive int
	)
	if len(ids) == 0 {
		return res
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	work := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < ingestWorkers(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range work {
				if ctx.Err() != nil {
					return
				}
				err := syncer.SyncSeries(ctx, s.st, id)
				if err != nil && ctx.Err() != nil {
					return // 本轮被砍断：这一部不算失败，剩下的也不跑了
				}
				mu.Lock()
				switch {
				case err == nil:
					res.wrote++
					consecutive = 0
				case errors.Is(err, plugin.ErrNotFound), errors.Is(err, plugin.ErrNoStream):
					res.failed++ // 上游明确没有：跳过、不删库、不记失败原因
				case errors.Is(err, plugin.ErrRejected):
					res.failed++
					res.abort = firstErr(res.abort, fmt.Errorf("被上游拒绝，中止本轮: %w", err))
					cancel()
				case errors.Is(err, plugin.ErrSchema):
					res.failed++
					consecutive++
					if consecutive >= maxConsecutiveSchemaErrors {
						res.abort = firstErr(res.abort,
							fmt.Errorf("连续 %d 部结构不符，中止本轮: %w", consecutive, err))
						cancel()
					}
				default:
					res.failed++
					res.record = firstErr(res.record, err)
					if !isKnownError(err) {
						res.unknown++
					}
				}
				mu.Unlock()
			}
		}()
	}

send:
	for _, id := range ids {
		select {
		case <-ctx.Done():
			break send
		case work <- id:
		}
	}
	close(work)
	wg.Wait()
	return res
}

// saveState 把一轮的进度写回 source_state。
//
// 五个字段全量覆盖（store 的语义）：游标、last_run_at（本轮开始时间，算 next_due
// 的唯一真值）、last_ok_at（本轮完整跑完才更新）、last_error（失败文本与时间；
// 成功一轮后清空）。失败不改 next_due 的推导方式，所以这里没有退避表，也没有退避字段。
func (s *Scheduler) saveState(ctx context.Context, name string, start time.Time, stats roundStats, roundErr error, stopping bool) {
	prev, err := s.st.LoadState(ctx, name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("读数据源状态失败，本轮进度未落库", "source", name, "err", err)
		return
	}
	next := prev
	next.Name = name
	next.Cursor = stats.cursor
	next.LastRunAt = formatTime(start)
	// last_error 记一条：本轮级别失败优先，其次是要留痕的单部失败。
	reason := firstErr(roundErr, stats.record)
	switch {
	case roundErr == nil:
		// 本轮跑完了（单部失败不影响这一条），next_due 与「上次成功」都往前走。
		next.LastOkAt = formatTime(s.now())
		next.LastError = ""
	case stopping:
		// 进程退出（父 ctx 被取消）不是源故障：沿用上一条，不新写一条。
		reason = nil
	}
	if reason != nil && !stopping {
		next.LastError = formatTime(s.now()) + " " + reason.Error()
	}
	if err := s.st.SaveState(ctx, next); err != nil {
		s.logger.Error("写数据源状态失败", "source", name, "err", err)
	}
}

// firstErr 返回第一个非 nil 的错误：错误汇聚只留一条语义（DEV-STANDARDS「错误处理」
// 不堆叠「失败: 失败」），优先级由调用方的参数顺序决定。
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// isKnownError 报告 err 是否命中插件契约的五条哨兵；未命中的按 ErrUpstream 处理并
// 额外计数（docs/PLUGIN-CONTRACT.md「错误语义」补充规则）。
func isKnownError(err error) bool {
	for _, sentinel := range []error{
		plugin.ErrUpstream, plugin.ErrRejected, plugin.ErrSchema, plugin.ErrNotFound, plugin.ErrNoStream,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

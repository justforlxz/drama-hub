package scheduler

import (
	"runtime"
	"time"
)

const (
	// maxPollInterval 是「配置变更能被多快感知」的上界：定时器等待取
	// min(最早到期 - now, 它)（docs/SCHEDULER-MODEL.md「实现路径」第 2 条）。
	// 它不决定一轮是否到期（那只看 last_run_at + 现读 interval），也不轮询任何东西。
	maxPollInterval = 60 * time.Second

	// maxIngestWorkers 是剧与剧之间的并发上限：4 并发实测 4.4 req/s，8 并发对上游
	// 收益递减而 403/429 概率上升（docs/SCHEDULER-MODEL.md「并发」）。不做成配置项。
	maxIngestWorkers = 4

	// maxConsecutiveSchemaErrors：连续这么多次 ErrSchema 就当源级结构变更中止本轮
	// （docs/PLUGIN-CONTRACT.md「错误语义」，N = 20）。计数在一次逐部同步内，不跨段累计。
	maxConsecutiveSchemaErrors = 20
)

// 配置读不出来时的兜底值，与 settings 注册表的默认值一致（docs/SCHEDULER-MODEL.md「配置」）。
// 配置错不该让调度停摆：调度器记一条 warn 用默认值继续，原因在日志与状态页可见。
const (
	defaultIntervalSec = 3600
	defaultBatch       = 1000
	defaultTimeoutSec  = 900
)

// nextDue 由 last_run_at + interval 现算下一次到期时间（docs/SCHEDULER-MODEL.md「下次时间」）。
//
// 库里不存 next_due，它就是本函数的结果：重启前后、改间隔前后都自洽。
// lastRun 为零值（还没跑过）→ 立刻到期。
func nextDue(lastRun time.Time, interval time.Duration, now time.Time) time.Time {
	if lastRun.IsZero() {
		return now
	}
	return lastRun.Add(interval)
}

// waitUntil 是调度循环这一次睡多久：min(到期时间 - now, poll)，且不小于 0。
//
// poll 是配置感知上界（生产固定 maxPollInterval）：改完 settings 最多 poll 之后
// 重新按新值算 next_due，不需要任何通知机制。
func waitUntil(due, now time.Time, poll time.Duration) time.Duration {
	wait := due.Sub(now)
	if wait > poll {
		wait = poll
	}
	if wait < 0 {
		return 0
	}
	return wait
}

// ingestWorkers 是剧与剧之间的并发数：min(maxIngestWorkers, max(1, NumCPU/2))。
func ingestWorkers() int {
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}
	if n > maxIngestWorkers {
		n = maxIngestWorkers
	}
	return n
}

// formatTime 把时间写成 source_state 里的不透明 UTC RFC3339 文本；零值写空串（=「还没发生过」）。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseTime 读回 formatTime 写的文本。空串与解析失败都返回零值：库被手改成垃圾时
// 最差的结果是「当作没跑过、立刻跑一轮」，好过让调度器停摆。
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// sourceConfig 是一个源此刻生效的调度配置。每次醒来都重读，因此改 settings 不重启即生效。
type sourceConfig struct {
	enabled  bool
	interval time.Duration
	batch    int
	timeout  time.Duration
}

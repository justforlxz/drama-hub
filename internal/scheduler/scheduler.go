// Package scheduler 的调度循环、手动触发与状态查询。一轮的执行见 round.go。
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

// 手动触发与状态查询的哨兵错误。internal/web 用 errors.Is 翻成状态码：
// ErrSourceNotFound / ErrSourceDisabled → 404，ErrSourceBusy → 409。
var (
	// ErrSourceNotFound 表示装配根没有注册这个源。
	ErrSourceNotFound = errors.New("调度器：没有这个源")
	// ErrSourceDisabled 表示该源在 settings 里停用（plugin.<源>.enabled=false），
	// 不参与刷新调度。手工单剧补录（POST /api/dramas）不受影响。
	ErrSourceDisabled = errors.New("调度器：该源已停用，不参与刷新调度")
	// ErrSourceBusy 表示该源正在跑一轮：手动与定时共用同一把单飞锁，撞车即此错误。
	ErrSourceBusy = errors.New("调度器：该源正在跑一轮")
)

// Syncer 是一个源的写库编排（生产实现是 *sync.Syncer）。
//
// 接口定义在消费方（docs/DEV-STANDARDS.md「代码组织」）：本包只调这一个动作，
// 不 import internal/sync，也不关心事务、派生表与海报。
//
// Name 是写库用的 drama.provider（= 插件名）。调度本身用 map 的键，不需要它；但同一个
// *sync.Syncer 也交给 internal/web 当建剧依赖，那边要靠 provider 名判断这次同步是
// 「新建」还是「更新」（MKACG-623），接口带出来才不需要在装配根拆成两份。
type Syncer interface {
	SyncSeries(ctx context.Context, st *store.Store, id string) error
	Name() string
}

// SourceStatus 是一个源的调度现状（状态页与 /api/status 的 sources[]）。
//
// 时间字段零值表示「还没发生过」；NextDueAt 是查询时按 last_run_at + 现读 interval
// 算出来的，库里没有这一列（docs/SCHEDULER-MODEL.md「下次时间」）。
type SourceStatus struct {
	Name        string
	Enabled     bool
	Running     bool
	LastRunAt   time.Time
	LastOkAt    time.Time
	LastError   string
	IntervalSec int
	NextDueAt   time.Time
}

// Scheduler 是进程内单例调度器。零值不可用，用 New 构造。
type Scheduler struct {
	st       *store.Store
	settings *store.Settings
	sources  []plugin.Plugin
	syncers  map[string]Syncer
	logger   *slog.Logger

	// locks 是每源一把单飞锁：手动与定时共用同一把。键集合在 New 时固定，之后只读，
	// 因此访问它不需要额外加锁。
	locks map[string]*sync.Mutex

	// mu 保护 running / lifetime / rounds：状态页读 running，关闭时等 rounds 收尾。
	mu       sync.Mutex
	running  map[string]bool
	lifetime context.Context
	rounds   sync.WaitGroup

	now  func() time.Time
	poll time.Duration
}

// New 构造调度器。sources 决定调度的对象与顺序；每个源都必须在 syncers 里有对应
// 实现，缺了该源的轮会以「没有写库实现」失败并在状态页可见（装配 bug，不必让进程
// 起不来）。
//
// 不持有任何外部可变状态：配置每轮从 settings 现读，进度每轮写 source_state。
func New(st *store.Store, settings *store.Settings, sources []plugin.Plugin, syncers map[string]Syncer, logger *slog.Logger) *Scheduler {
	s := &Scheduler{
		st:       st,
		settings: settings,
		sources:  sources,
		syncers:  syncers,
		logger:   logger,
		locks:    make(map[string]*sync.Mutex, len(sources)),
		running:  make(map[string]bool, len(sources)),
		now:      time.Now,
		poll:     maxPollInterval,
	}
	for _, p := range sources {
		s.locks[p.Name()] = &sync.Mutex{}
	}
	return s
}

// Run 跑调度循环，阻塞到 ctx 取消；返回前等本轮派生的轮收尾（goroutine 有明确 owner）。
//
// 循环结构见 docs/SCHEDULER-MODEL.md「循环结构」：睡 min(最早 next_due, poll) →
// 醒来重读全部配置、重算各源 next_due → 把到期的源各派一轮出去（同源拿不到单飞锁
// 就跳过）。
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	s.lifetime = ctx
	s.mu.Unlock()
	defer s.rounds.Wait()

	s.ensureStates(ctx)
	for {
		timer := time.NewTimer(waitUntil(s.earliestDue(ctx), s.now(), s.poll))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.dispatchDue(ctx)
	}
}

// Trigger 立刻跑一轮指定源，不等 next_due（POST /api/refresh 的实现）。
//
// 与定时触发共用同一把单飞锁：该源正在跑 → ErrSourceBusy；源不存在 →
// ErrSourceNotFound；已在 settings 停用 → ErrSourceDisabled。
// 返回 nil 只表示「一轮已派出去」（HTTP 202），不代表这一轮成功。
func (s *Scheduler) Trigger(ctx context.Context, source string) error {
	p, ok := s.sourceOf(source)
	if !ok {
		return fmt.Errorf("%w: %q", ErrSourceNotFound, source)
	}
	cfg := s.configFor(ctx, source)
	if !cfg.enabled {
		return fmt.Errorf("%w: %s", ErrSourceDisabled, source)
	}
	// 轮的寿命挂在调度器上而不是这次请求上：HTTP 202 之后请求就结束了。
	return s.start(s.lifetimeCtx(), p, cfg)
}

// Status 现读配置与 source_state，给出每个源的调度现状。
//
// 每次都查库、不缓存：改完 interval 下一次查询就是新值（硬约束 5 的同一条理由）。
func (s *Scheduler) Status(ctx context.Context) ([]SourceStatus, error) {
	now := s.now()
	out := make([]SourceStatus, 0, len(s.sources))
	for _, p := range s.sources {
		name := p.Name()
		cfg := s.configFor(ctx, name)
		st, err := s.st.LoadState(ctx, name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("读数据源状态 %s: %w", name, err)
		}
		lastRun := parseTime(st.LastRunAt)
		out = append(out, SourceStatus{
			Name:        name,
			Enabled:     cfg.enabled,
			Running:     s.isRunning(name),
			LastRunAt:   lastRun,
			LastOkAt:    parseTime(st.LastOkAt),
			LastError:   st.LastError,
			IntervalSec: int(cfg.interval / time.Second),
			NextDueAt:   nextDue(lastRun, cfg.interval, now),
		})
	}
	return out, nil
}

// ensureStates 给每个源铺一行空状态（无行时）。
//
// 与「是否启用」无关：停用过的源在状态页仍要显示上次跑到哪，source_state 是它唯一的
// 历史。幂等，重启不会覆盖已有进度。
func (s *Scheduler) ensureStates(ctx context.Context) {
	for _, p := range s.sources {
		name := p.Name()
		_, err := s.st.LoadState(ctx, name)
		if err == nil {
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.logger.Error("读数据源状态失败", "source", name, "err", err)
			continue
		}
		if err := s.st.SaveState(ctx, store.SourceState{Name: name}); err != nil {
			s.logger.Error("初始化数据源状态失败", "source", name, "err", err)
		}
	}
}

// earliestDue 返回最早的 next_due（全部源现读配置后各算一次），并限制在 poll 之内。
//
// 正在跑的源按 now+poll 计：它的 last_run_at 要等本轮收尾才写回，不这样算会让循环
// 一直立刻到期、空转到本轮结束。没有可调度的源时同样返回 now+poll —— 循环仍要按
// poll 醒来读配置（改 enabled 不重启即生效靠的就是这一条）。
func (s *Scheduler) earliestDue(ctx context.Context) time.Time {
	now := s.now()
	earliest := now.Add(s.poll)
	for _, p := range s.sources {
		cfg := s.configFor(ctx, p.Name())
		if !cfg.enabled {
			continue
		}
		due := now.Add(s.poll)
		if !s.isRunning(p.Name()) {
			st, err := s.st.LoadState(ctx, p.Name())
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				s.logger.Warn("读数据源状态失败，按立刻到期处理", "source", p.Name(), "err", err)
			}
			due = nextDue(parseTime(st.LastRunAt), cfg.interval, now)
		}
		if due.Before(earliest) {
			earliest = due
		}
	}
	return earliest
}

// dispatchDue 把此刻到期的源各派一轮出去。同源拿不到单飞锁就跳过（记一条 info），
// 不影响其它源 —— 源之间并行、同源单飞。
func (s *Scheduler) dispatchDue(ctx context.Context) {
	now := s.now()
	for _, p := range s.sources {
		cfg := s.configFor(ctx, p.Name())
		if !cfg.enabled {
			continue
		}
		st, err := s.st.LoadState(ctx, p.Name())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.logger.Error("读数据源状态失败，跳过该源", "source", p.Name(), "err", err)
			continue
		}
		if nextDue(parseTime(st.LastRunAt), cfg.interval, now).After(now) {
			continue
		}
		switch err := s.start(ctx, p, cfg); {
		case errors.Is(err, ErrSourceBusy):
			s.logger.Info("跳过：该源正在跑一轮", "source", p.Name())
		case err != nil:
			s.logger.Error("派发一轮失败", "source", p.Name(), "err", err)
		}
	}
}

// start 拿单飞锁并派出一个轮 goroutine。锁被占返回 ErrSourceBusy。
func (s *Scheduler) start(ctx context.Context, p plugin.Plugin, cfg sourceConfig) error {
	name := p.Name()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("调度器正在关闭，不再派发: %w", err)
	}
	lock := s.locks[name]
	if lock == nil {
		return fmt.Errorf("%w: %q", ErrSourceNotFound, name)
	}
	if !lock.TryLock() {
		return fmt.Errorf("%w: %s", ErrSourceBusy, name)
	}
	s.mu.Lock()
	s.running[name] = true
	s.rounds.Add(1)
	s.mu.Unlock()

	go func() {
		// LIFO：先摘 running 标记，再放锁，最后告诉 Wait 这一轮收尾了。
		defer s.rounds.Done()
		defer lock.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.running, name)
			s.mu.Unlock()
		}()
		s.runRound(ctx, p, cfg)
	}()
	return nil
}

// configFor 现读一个源的调度配置：enabled / 生效间隔（源级覆盖全局）/ 批量 / 单轮超时。
//
// 读失败用默认值并记一条 warn：配置错不该让调度停摆，原因在日志与状态页可见。
func (s *Scheduler) configFor(ctx context.Context, name string) sourceConfig {
	pluginKey := func(item string) store.SettingKey { return store.PluginSettingKey(name, item) }
	cfg := sourceConfig{
		enabled:  true,
		interval: defaultIntervalSec * time.Second,
		batch:    defaultBatch,
		timeout:  defaultTimeoutSec * time.Second,
	}
	if enabled, err := s.settings.Bool(ctx, pluginKey(store.PluginItemEnabled)); err != nil {
		s.logger.Warn("读启用开关失败，按启用处理", "source", name, "err", err)
	} else {
		cfg.enabled = enabled
	}
	if global, err := s.settings.Int(ctx, store.KeyRefreshIntervalSec); err != nil {
		s.logger.Warn("读 refresh_interval_sec 失败，用默认值", "source", name, "err", err)
	} else {
		cfg.interval = time.Duration(global) * time.Second
	}
	// 源级覆盖：无行 = 继承全局（OptionalInt 的 found=false）。
	override, found, err := s.settings.OptionalInt(ctx, pluginKey(store.PluginItemRefreshInterval))
	switch {
	case err != nil:
		s.logger.Warn("读源级刷新间隔失败，继承全局", "source", name, "err", err)
	case found:
		cfg.interval = time.Duration(override) * time.Second
	}
	if batch, err := s.settings.Int(ctx, store.KeyRefreshBatch); err != nil {
		s.logger.Warn("读 refresh_batch 失败，用默认值", "source", name, "err", err)
	} else {
		cfg.batch = batch
	}
	if timeout, err := s.settings.Int(ctx, store.KeyRefreshTimeoutSec); err != nil {
		s.logger.Warn("读 refresh_timeout_sec 失败，用默认值", "source", name, "err", err)
	} else {
		cfg.timeout = time.Duration(timeout) * time.Second
	}
	return cfg
}

// sourceOf 按名字取已注册的源。
func (s *Scheduler) sourceOf(name string) (plugin.Plugin, bool) {
	for _, p := range s.sources {
		if p.Name() == name {
			return p, true
		}
	}
	return nil, false
}

// isRunning 报告该源此刻是否有一轮在跑。
func (s *Scheduler) isRunning(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[name]
}

// lifetimeCtx 是派生轮用的上下文：取 Run 收到的 ctx（进程寿命），未 Run 时退回
// Background —— 那种情况只会出现在直接调 Trigger 的测试里。
func (s *Scheduler) lifetimeCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifetime == nil {
		return context.Background()
	}
	return s.lifetime
}

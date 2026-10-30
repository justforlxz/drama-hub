package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/store"
)

// fakeSource 是测试用的数据源：按页返回预置的作品标识，并记录 Enumerate 次数
// （即「本轮打了几个上游请求」）。
//
// 它只实现契约的四个方法；调度器不认识更具体的东西，所以它足以覆盖三段、游标与
// 单轮超时，不需要真站点。
type fakeSource struct {
	name    string
	pages   []plugin.Page
	enumErr error
	// blockAfterFirstPage：第一次之后每次 Enumerate 都阻塞到 ctx 结束，模拟「单轮
	// 预算在枚举中途用完」。
	blockAfterFirstPage bool
	// delay 让每次 Enumerate 真实耗时一点，好让日志里的分段耗时不是 0
	// （只用于「三段可观察」那一条，取值在毫秒级）。
	delay time.Duration

	mu         sync.Mutex
	enumerates int
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Enumerate(ctx context.Context, cur *plugin.Cursor) (plugin.Page, error) {
	f.mu.Lock()
	f.enumerates++
	call := f.enumerates
	f.mu.Unlock()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.enumErr != nil {
		return plugin.Page{}, f.enumErr
	}
	if f.blockAfterFirstPage && call > 1 {
		<-ctx.Done()
		return plugin.Page{}, ctx.Err()
	}
	next := 0
	if cur != nil {
		for i, page := range f.pages {
			if page.Next != nil && page.Next.Token == cur.Token {
				next = i + 1
				break
			}
		}
	}
	if next >= len(f.pages) {
		return plugin.Page{}, nil
	}
	return f.pages[next], nil
}

func (f *fakeSource) Work(context.Context, plugin.WorkRef) (plugin.WorkDetail, error) {
	return plugin.WorkDetail{}, errors.New("fakeSource.Work 不参与调度测试")
}

func (f *fakeSource) Resolve(context.Context, plugin.WorkRef, int) (plugin.Stream, error) {
	return plugin.Stream{}, plugin.ErrNoStream
}

func (f *fakeSource) Health(context.Context) error { return nil }

// enumerateCalls 返回 Enumerate 被调用的次数。
func (f *fakeSource) enumerateCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enumerates
}

// fakeSyncer 冒充一个源的写库编排：记录被同步的 id，可按 id 注入错误，可加闸阻塞
// （用来观察同源单飞与源间并行）。
type fakeSyncer struct {
	errs   map[string]error
	err    error // 非 nil：无论 id 都返回它
	gate   chan struct{}
	onCall chan struct{}
	delay  time.Duration

	mu  sync.Mutex
	ids []string
}

func (f *fakeSyncer) SyncSeries(ctx context.Context, _ *store.Store, id string) error {
	f.mu.Lock()
	f.ids = append(f.ids, id)
	f.mu.Unlock()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.onCall != nil {
		f.onCall <- struct{}{}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err, ok := f.errs[id]; ok {
		return err
	}
	return f.err
}

// syncedIDs 返回已同步的 id 副本。
func (f *fakeSyncer) syncedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ids...)
}

// fakeClock 是可注入的时钟：调度器里所有「现在是几点」都走它，测试因此不依赖真实时间。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// syncBuffer 是并发安全的日志缓冲：多个源的轮会并发写日志，测试又要在别处读它
// （bytes.Buffer 两个方向都不安全）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness 是一套真实依赖：临时 SQLite（真迁移、真 settings）+ 假上游 + 假写库编排。
type harness struct {
	t        *testing.T
	db       *store.DB
	st       *store.Store
	settings *store.Settings
	logs     *syncBuffer
	s        *Scheduler
	clock    *fakeClock
	sources  []*fakeSource
	syncers  map[string]*fakeSyncer
}

// 测试起点：固定时刻，避免「断言现在」。
var testNow = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

func newHarness(t *testing.T, sources ...*fakeSource) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := &harness{
		t:        t,
		db:       db,
		st:       store.NewStore(db.DB),
		settings: store.NewSettings(db.DB),
		logs:     &syncBuffer{},
		clock:    &fakeClock{t: testNow},
		syncers:  make(map[string]*fakeSyncer, len(sources)),
		sources:  sources,
	}
	plugins := make([]plugin.Plugin, 0, len(sources))
	syncers := make(map[string]Syncer, len(sources))
	for _, src := range sources {
		plugins = append(plugins, src)
		syncer := &fakeSyncer{}
		h.syncers[src.name] = syncer
		syncers[src.name] = syncer
	}
	logger := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.s = New(h.st, h.settings, plugins, syncers, logger)
	h.s.now = h.clock.Now
	return h
}

// set 写一条 settings，写失败即测试失败（配置校验回归会在这里暴露）。
func (h *harness) set(key store.SettingKey, value string) {
	h.t.Helper()
	if err := h.settings.Set(context.Background(), key, value); err != nil {
		h.t.Fatalf("Set %s=%s: %v", key, value, err)
	}
}

// seedDrama 在库里放一部剧（存量轮转的候选）。
func (h *harness) seedDrama(provider, providerID, syncAt string) {
	h.t.Helper()
	if _, err := h.st.UpsertDrama(context.Background(), store.Drama{
		Provider: provider, ProviderID: providerID, Title: providerID, SyncAt: syncAt,
	}); err != nil {
		h.t.Fatalf("UpsertDrama %s: %v", providerID, err)
	}
}

// runRound 同步跑一轮（不经循环，便于对单轮行为做确定断言）。
func (h *harness) runRound(name string) {
	ctx := context.Background()
	h.s.runRound(ctx, h.source(name), h.s.configFor(ctx, name))
}

// runRoundWithTimeout 用给定的单轮预算跑一轮：比等配置下限（60 秒）快得多，
// 走的仍是生产那条 context.WithTimeout 路径。
func (h *harness) runRoundWithTimeout(name string, timeout time.Duration) {
	ctx := context.Background()
	cfg := h.s.configFor(ctx, name)
	cfg.timeout = timeout
	h.s.runRound(ctx, h.source(name), cfg)
}

func (h *harness) source(name string) plugin.Plugin {
	for _, src := range h.sources {
		if src.name == name {
			return src
		}
	}
	h.t.Fatalf("测试装配里没有源 %s", name)
	return nil
}

// state 读该源的 source_state。
func (h *harness) state(name string) store.SourceState {
	h.t.Helper()
	st, err := h.st.LoadState(context.Background(), name)
	if err != nil {
		h.t.Fatalf("LoadState %s: %v", name, err)
	}
	return st
}

// stateOrZero 读 source_state；无行（调度器还没铺初始行）返回零值。
func (h *harness) stateOrZero(name string) store.SourceState {
	h.t.Helper()
	st, err := h.st.LoadState(context.Background(), name)
	switch {
	case err == nil:
		return st
	case errors.Is(err, store.ErrNotFound):
		return store.SourceState{}
	default:
		h.t.Fatalf("LoadState %s: %v", name, err)
		return store.SourceState{}
	}
}

// status 读该源的调度现状。
func (h *harness) status(name string) SourceStatus {
	h.t.Helper()
	list, err := h.s.Status(context.Background())
	if err != nil {
		h.t.Fatalf("Status: %v", err)
	}
	for _, st := range list {
		if st.Name == name {
			return st
		}
	}
	h.t.Fatalf("Status 里没有源 %s", name)
	return SourceStatus{}
}

// lastRoundLog 解析最后一条「一轮结束」日志。
func (h *harness) lastRoundLog() map[string]any {
	h.t.Helper()
	lines := strings.Split(strings.TrimSpace(h.logs.String()), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var entry map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			h.t.Fatalf("日志不是合法 JSON: %v（%q）", err, lines[i])
		}
		if entry["msg"] == "一轮结束" {
			return entry
		}
	}
	h.t.Fatalf("日志里没有「一轮结束」记录：%s", h.logs.String())
	return nil
}

// waitIdle 等该源的轮收尾（running 标记摘掉）。
func (h *harness) waitIdle(name string) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !h.s.isRunning(name) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("源 %s 的轮没有在 2s 内收尾", name)
}

// TestRound_ThreeSegments 是「一轮三段」的主用例：枚举 → 新剧灌库 → 存量轮转，
// 三段各自可观察（耗时与请求计数进日志），游标跑完后回到起点。
func TestRound_ThreeSegments(t *testing.T) {
	src := &fakeSource{name: "web", pages: []plugin.Page{
		{Refs: refs("web", "a", "b"), Next: &plugin.Cursor{Token: "p2"}},
		{Refs: refs("web", "c", "d")},
	}}
	src.delay = 5 * time.Millisecond // 让三段耗时在日志里可读（毫秒级，不阻塞断言）
	h := newHarness(t, src)
	h.syncers["web"].delay = 5 * time.Millisecond
	// a、b 是库里已有的剧，sync_at 最旧 → 这一轮该刷新它们；c、d 是新增。
	h.seedDrama("web", "a", "2026-10-01T00:00:00Z")
	h.seedDrama("web", "b", "2026-10-02T00:00:00Z")
	h.seedDrama("web", "c", "2026-10-03T00:00:00Z") // 已存在 → 不是新剧
	h.set(store.KeyRefreshIntervalSec, "3600")
	h.set(store.KeyRefreshBatch, "2")

	h.runRound("web")

	if got := src.enumerateCalls(); got != 2 {
		t.Errorf("枚举页数 = %d, 期望 2（枚举本身的上游请求计数）", got)
	}
	entry := h.lastRoundLog()
	want := map[string]float64{
		"refs": 4, "ingested": 1, "rotated": 2, "failed": 0,
	}
	for key, value := range want {
		if got, _ := entry[key].(float64); got != value {
			t.Errorf("日志 %s = %v, 期望 %v（整条：%v）", key, entry[key], value, entry)
		}
	}
	// 三段的耗时各自独立出现在同一条日志里（三段分开可观察）。
	for _, key := range []string{"enumerate_ms", "ingest_ms", "rotate_ms"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("日志缺 %s：%v", key, entry)
		}
	}
	t.Logf("三段耗时与计数：枚举 %v ms（%d 次上游请求）/ 灌库 %v ms（新增 %v 部）/ 轮转 %v ms（刷新 %v 部）",
		entry["enumerate_ms"], src.enumerateCalls(), entry["ingest_ms"], entry["ingested"],
		entry["rotate_ms"], entry["rotated"])

	// d 是新增（c 已在库），a、b 是最旧的两部。
	gotIDs := h.syncers["web"].syncedIDs()
	if !sameSet(gotIDs, []string{"d", "a", "b"}) {
		t.Errorf("同步的 id = %v, 期望 {d(新增), a, b(最旧两部)}", gotIDs)
	}

	st := h.state("web")
	if st.Cursor != "" {
		t.Errorf("枚举走完后 cursor = %q, 期望空（下一轮从起点重来）", st.Cursor)
	}
	if st.LastRunAt != formatTime(testNow) {
		t.Errorf("last_run_at = %q, 期望本轮开始时间 %q", st.LastRunAt, formatTime(testNow))
	}
	if st.LastError != "" {
		t.Errorf("成功一轮后 last_error = %q, 期望清空", st.LastError)
	}
}

// TestRound_TimeoutKeepsCursor 覆盖「单轮超时不清游标」：枚举在一页之后被砍断，
// 游标停在最后一个成功推进的位置，不归零、不丢进度。
func TestRound_TimeoutKeepsCursor(t *testing.T) {
	src := &fakeSource{
		name:                "web",
		blockAfterFirstPage: true,
		pages: []plugin.Page{
			{Refs: refs("web", "a"), Next: &plugin.Cursor{Token: "page-2"}},
			{Refs: refs("web", "b")},
		},
	}
	h := newHarness(t, src)
	if err := h.st.SaveState(context.Background(), store.SourceState{
		Name: "web", Cursor: "page-1", LastRunAt: formatTime(testNow.Add(-time.Hour)),
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	h.runRoundWithTimeout("web", 30*time.Millisecond)

	st := h.state("web")
	if st.Cursor != "page-2" {
		t.Errorf("超时后 cursor = %q, 期望停在最后成功推进的 page-2（不清游标）", st.Cursor)
	}
	if st.LastRunAt != formatTime(testNow) {
		t.Errorf("超时后 last_run_at = %q, 期望仍是本轮开始时间", st.LastRunAt)
	}
	if st.LastOkAt != "" {
		t.Errorf("超时的一轮不该刷新 last_ok_at, 实际 %q", st.LastOkAt)
	}
	if st.LastError == "" {
		t.Error("last_error 为空，期望记下超时原因")
	}
}

// TestRound_FailureAdvancesNextDue 覆盖「失败也推进 next_due、不做退避」：连续三轮
// 失败，每次 next_due 都等于 last_run_at + 现读 interval。
func TestRound_FailureAdvancesNextDue(t *testing.T) {
	src := &fakeSource{name: "web", enumErr: fmt.Errorf("%w: 上游 503", plugin.ErrUpstream)}
	h := newHarness(t, src)
	h.set(store.KeyRefreshIntervalSec, "3600")

	for round := 1; round <= 3; round++ {
		h.runRound("web")
		st := h.state("web")
		if st.LastError == "" {
			t.Fatalf("第 %d 轮失败后 last_error 为空", round)
		}
		runAt := parseTime(st.LastRunAt)
		if runAt.IsZero() {
			t.Fatalf("第 %d 轮没写 last_run_at", round)
		}
		status := h.status("web")
		if want := runAt.Add(time.Hour); !status.NextDueAt.Equal(want) {
			t.Fatalf("第 %d 轮 next_due = %v, 期望 last_run_at + 1h = %v（无退避）",
				round, status.NextDueAt, want)
		}
		h.clock.Advance(time.Hour)
	}
}

// TestRound_ErrorSemantics 覆盖单部失败的分级：ErrNotFound 只跳过（不记 last_error）、
// ErrUpstream 记 last_error 但本轮算完成、ErrRejected 立刻中止剩余工作。
func TestRound_ErrorSemantics(t *testing.T) {
	t.Run("ErrNotFound 只跳过", func(t *testing.T) {
		src := &fakeSource{name: "web", pages: []plugin.Page{{Refs: refs("web", "a", "b")}}}
		h := newHarness(t, src)
		h.syncers["web"].errs = map[string]error{"a": fmt.Errorf("%w: 已下架", plugin.ErrNotFound)}

		h.runRound("web")

		st := h.state("web")
		if st.LastError != "" {
			t.Errorf("ErrNotFound 不该记 last_error, 实际 %q", st.LastError)
		}
		if st.LastOkAt == "" {
			t.Error("单部 404 不该让整轮失败")
		}
	})

	t.Run("ErrUpstream 记原因但本轮完成", func(t *testing.T) {
		src := &fakeSource{name: "web", pages: []plugin.Page{{Refs: refs("web", "a")}}}
		h := newHarness(t, src)
		h.syncers["web"].errs = map[string]error{"a": fmt.Errorf("%w: 连接失败", plugin.ErrUpstream)}

		h.runRound("web")

		st := h.state("web")
		if !strings.Contains(st.LastError, "连接失败") {
			t.Errorf("last_error = %q, 期望带上游失败原因", st.LastError)
		}
		if st.LastOkAt == "" {
			t.Error("单部失败不该让整轮失败（失败隔离）")
		}
	})

	t.Run("ErrRejected 中止本轮", func(t *testing.T) {
		src := &fakeSource{name: "web", pages: []plugin.Page{{Refs: refs("web", "a", "b", "c")}}}
		h := newHarness(t, src)
		h.syncers["web"].errs = map[string]error{"a": fmt.Errorf("%w: 429", plugin.ErrRejected)}

		h.runRound("web")

		st := h.state("web")
		if !strings.Contains(st.LastError, "中止本轮") {
			t.Errorf("last_error = %q, 期望说明本轮被中止", st.LastError)
		}
		if st.LastError == "" {
			t.Error("last_error 为空，期望说明本轮被中止")
		}
	})
}

// TestSingleFlight 覆盖「同源单飞」：手动触发撞车返回 ErrSourceBusy；定时触发撞上
// 正在跑的手动轮时被跳过（不并发跑第二轮）。
func TestSingleFlight(t *testing.T) {
	src := &fakeSource{name: "web", pages: []plugin.Page{{Refs: refs("web", "a")}}}
	h := newHarness(t, src)
	syncer := h.syncers["web"]
	syncer.gate = make(chan struct{})
	syncer.onCall = make(chan struct{}, 4)

	if err := h.s.Trigger(context.Background(), "web"); err != nil {
		t.Fatalf("第一次手动触发: %v", err)
	}
	<-syncer.onCall // 轮已进入写库阶段，锁被占着

	if err := h.s.Trigger(context.Background(), "web"); !errors.Is(err, ErrSourceBusy) {
		t.Fatalf("撞车的手动触发错误 = %v, 期望 ErrSourceBusy（→ 409）", err)
	}
	// 定时触发撞上同一把锁：跳过而不是并发跑第二轮。
	h.s.dispatchDue(context.Background())
	if got := len(syncer.syncedIDs()); got != 1 {
		t.Fatalf("定时与手动同时触发时同步了 %d 部, 期望只跑手上这一部", got)
	}

	close(syncer.gate)
	h.waitIdle("web")
	if err := h.s.Trigger(context.Background(), "web"); err != nil {
		t.Fatalf("轮结束后再触发应成功, 实际 %v", err)
	}
	h.waitIdle("web")
}

// TestParallel_DifferentSources 覆盖「源间并行」：两个源各有轮在跑时都处于 running。
func TestParallel_DifferentSources(t *testing.T) {
	a := &fakeSource{name: "web", pages: []plugin.Page{{Refs: refs("web", "a")}}}
	b := &fakeSource{name: "other", pages: []plugin.Page{{Refs: refs("other", "b")}}}
	h := newHarness(t, a, b)
	for _, name := range []string{"web", "other"} {
		syncer := h.syncers[name]
		syncer.gate = make(chan struct{})
		syncer.onCall = make(chan struct{}, 4)
	}

	for _, name := range []string{"web", "other"} {
		if err := h.s.Trigger(context.Background(), name); err != nil {
			t.Fatalf("触发 %s: %v", name, err)
		}
	}
	// 两个源都进入写库阶段、都还没结束：同源单飞没有退化成全局单飞。
	<-h.syncers["web"].onCall
	<-h.syncers["other"].onCall
	for _, name := range []string{"web", "other"} {
		if !h.status(name).Running {
			t.Errorf("%s 没在 running（源间必须并行）", name)
		}
	}

	close(h.syncers["web"].gate)
	close(h.syncers["other"].gate)
	h.waitIdle("web")
	h.waitIdle("other")
}

// TestStatus_NextDueRestartAndNoColumn 覆盖「下次时间重启可算」：next_due 由
// last_run_at + 现读 interval 得出，换一个调度器实例（= 重启）结果一致；库里没有
// next_due 列。
func TestStatus_NextDueRestartAndNoColumn(t *testing.T) {
	src := &fakeSource{name: "web"}
	h := newHarness(t, src)
	lastRun := testNow.Add(-10 * time.Minute)
	if err := h.st.SaveState(context.Background(), store.SourceState{
		Name: "web", Cursor: "page-3", LastRunAt: formatTime(lastRun),
		LastOkAt: formatTime(lastRun.Add(time.Minute)),
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	h.set(store.KeyRefreshIntervalSec, "3600")

	before := h.status("web")
	if want := lastRun.Add(time.Hour); !before.NextDueAt.Equal(want) {
		t.Fatalf("next_due = %v, 期望 last_run_at + 1h = %v", before.NextDueAt, want)
	}
	if before.IntervalSec != 3600 || before.LastRunAt != lastRun {
		t.Errorf("status = %+v, 期望间隔 3600 秒与库里的 last_run_at", before)
	}

	// 重启：同一库、同一配置，只是新的调度器实例。
	restarted := New(h.st, h.settings, []plugin.Plugin{src},
		map[string]Syncer{"web": h.syncers["web"]}, slog.New(slog.DiscardHandler))
	restarted.now = h.clock.Now
	list, err := restarted.Status(context.Background())
	if err != nil {
		t.Fatalf("重启后 Status: %v", err)
	}
	if !list[0].NextDueAt.Equal(before.NextDueAt) {
		t.Errorf("重启后 next_due = %v, 重启前 = %v（必须一致）", list[0].NextDueAt, before.NextDueAt)
	}

	// 改间隔 → next_due 立刻按新值算（不重启也生效）。
	h.set(store.KeyRefreshIntervalSec, "900")
	if got := h.status("web"); !got.NextDueAt.Equal(lastRun.Add(15 * time.Minute)) {
		t.Errorf("改间隔为 900 后 next_due = %v, 期望 +15m", got.NextDueAt)
	}

	// 库里没有 next_due 列：五列就是全部（S3 QA 留下的对端确认结论）。
	rows, err := h.db.Query(`PRAGMA table_info(source_state)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("读 table_info 行: %v", err)
		}
		columns = append(columns, name)
	}
	want := []string{"name", "cursor", "last_run_at", "last_ok_at", "last_error"}
	if strings.Join(columns, ",") != strings.Join(want, ",") {
		t.Errorf("source_state 列 = %v, 期望 %v（无 next_due 列）", columns, want)
	}
}

// TestIntervalHotRead 覆盖「改间隔不重启即生效」：循环每次醒来重读 settings 并按新
// interval 重算 next_due。时钟注入让这条验证不需要真等 15 分钟。
//
// 这里同时贴出连续三次执行的 last_run_at：间隔 1h → 拒绝在 1h 前跑第二轮；
// 改成 15m 后按 15m 的节奏跑，进程未重启、也没有任何 settings 通知机制。
func TestIntervalHotRead(t *testing.T) {
	src := &fakeSource{name: "web"}
	h := newHarness(t, src)
	h.set(store.KeyRefreshIntervalSec, "3600")
	// poll 压到 1ms：生产是 60s，测试只需要「醒来就会重读」这个机制。
	h.s.poll = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.s.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// waitRun 等 last_run_at 落到 want：状态在每轮收尾时写回，因此读到它就是这一轮
	// 已经结束。只看库（不相信内存），也不读并发写的日志。
	waitRun := func(want time.Time) time.Time {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if got := parseTime(h.stateOrZero("web").LastRunAt); got.Equal(want) {
				return got
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("没有等到 last_run_at = %v（实际 %q）", want, h.stateOrZero("web").LastRunAt)
		return time.Time{}
	}

	// 第一次：last_run_at 为空 → 立刻按 interval 前的起点跑。
	first := waitRun(testNow)
	// 还没到 1 小时：推进 3601 秒才该有第二轮。
	h.clock.Advance(3601 * time.Second)
	second := waitRun(testNow.Add(3601 * time.Second))
	// 改成 15 分钟后：下一次醒来（≤ poll）就按新间隔算，进程没重启、没有通知机制。
	h.set(store.KeyRefreshIntervalSec, "900")
	h.clock.Advance(901 * time.Second)
	third := waitRun(testNow.Add(3601*time.Second + 901*time.Second))

	t.Logf("连续三次执行的 last_run_at：%s / %s / %s",
		first.Format(time.RFC3339), second.Format(time.RFC3339), third.Format(time.RFC3339))
	if gap := second.Sub(first); gap != 3601*time.Second {
		t.Errorf("第一次间隔 = %v, 期望 ≈3601s（1h 间隔）", gap)
	}
	if gap := third.Sub(second); gap != 901*time.Second {
		t.Errorf("改间隔后第二次间隔 = %v, 期望 ≈901s（15m 间隔，未重启）", gap)
	}
}

// TestWaitUntil 锁定「配置最多 60 秒被感知」这条上界：默认配置下睡眠时间不超过 60 秒。
func TestWaitUntil(t *testing.T) {
	s := New(nil, nil, nil, nil, slog.New(slog.DiscardHandler))
	if s.poll != 60*time.Second {
		t.Fatalf("默认感知上界 = %v, 期望 60s", s.poll)
	}
	now := testNow
	cases := []struct {
		name string
		due  time.Time
		want time.Duration
	}{
		{"到期在一小时后 → 仍只睡 60s（改配置最多 60s 被看到）", now.Add(time.Hour), 60 * time.Second},
		{"到期在 10s 后 → 睡 10s", now.Add(10 * time.Second), 10 * time.Second},
		{"已过期 → 不等待", now.Add(-time.Minute), 0},
		{"刚好 60s", now.Add(60 * time.Second), 60 * time.Second},
	}
	for _, c := range cases {
		if got := waitUntil(c.due, now, s.poll); got != c.want {
			t.Errorf("%s: waitUntil = %v, 期望 %v", c.name, got, c.want)
		}
	}
}

// TestDisabledSource 覆盖 PL 裁定：enabled=false = 不参与刷新调度（定时跳过、
// 手动触发 404），但这不是硬门禁（手工单剧补录在 internal/web 覆盖）。
func TestDisabledSource(t *testing.T) {
	src := &fakeSource{name: "web"}
	h := newHarness(t, src)
	h.set(store.PluginSettingKey("web", store.PluginItemEnabled), "false")

	h.s.dispatchDue(context.Background())
	if got := len(h.syncers["web"].syncedIDs()); got != 0 {
		t.Errorf("停用的源被调度了 %d 次, 期望 0", got)
	}
	if err := h.s.Trigger(context.Background(), "web"); !errors.Is(err, ErrSourceDisabled) {
		t.Errorf("触发停用的源错误 = %v, 期望 ErrSourceDisabled（→ 404）", err)
	}
	if err := h.s.Trigger(context.Background(), "nope"); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("触发不存在的源错误 = %v, 期望 ErrSourceNotFound（→ 404）", err)
	}
	if got := h.status("web"); got.Enabled {
		t.Errorf("停用的源 status.enabled = %v, 期望 false", got.Enabled)
	}
}

// TestEnumerateCursorGuard 覆盖游标不推进的插件 bug：不许死循环，要报 ErrSchema。
func TestEnumerateCursorGuard(t *testing.T) {
	src := &fakeSource{name: "web", pages: []plugin.Page{
		{Refs: refs("web", "a"), Next: &plugin.Cursor{Token: "same"}},
		{Refs: refs("web", "b"), Next: &plugin.Cursor{Token: "same"}},
	}}
	h := newHarness(t, src)
	h.runRound("web")

	st := h.state("web")
	if !strings.Contains(st.LastError, "游标没有推进") {
		t.Errorf("last_error = %q, 期望指出游标没有推进", st.LastError)
	}
}

// refs 造一页作品标识。
func refs(name string, ids ...string) []plugin.WorkRef {
	out := make([]plugin.WorkRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, plugin.WorkRef{Plugin: name, ID: id})
	}
	return out
}

// sameSet 比较两个 id 集合（写库顺序在多 worker 下不确定）。
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, id := range got {
		seen[id]++
	}
	for _, id := range want {
		if seen[id] == 0 {
			return false
		}
		seen[id]--
	}
	return true
}

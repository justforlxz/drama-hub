package segment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
)

// syncBuffer 是并发安全的日志缓冲：预取日志由后台 goroutine 写，测试要边跑边读，
// 普通的 *bytes.Buffer 在 -race 下是数据竞争。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records 把 JSON 日志行解析成 map（非 JSON 行忽略）。
func (b *syncBuffer) records() []map[string]any {
	b.mu.Lock()
	raw := b.buf.String()
	b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// prefetchSource 是预取测试的插件替身：Resolve 的调用序列就是「上游请求」。
//
// 计数放在插件层而不是 Cutter.resolveMany：真实链路里缓存命中的集根本到不了插件，
// 这样「命中缓存零外呼」「单飞只解析一次」才是生产语义的断言。
type prefetchSource struct {
	name string
	url  string

	mu       sync.Mutex
	calls    []string // "<provider_id>/<ep_no>"，按调用顺序
	inflight int
	peak     int
	gate     map[string]chan struct{} // 集 → 等 close 才回包（模拟上游卡住）
	stubborn map[string]bool          // 无视 ctx 取消（验证停机上限）
	failOnce map[string]error         // 第一次调用返回该错误，之后正常
}

func (s *prefetchSource) Name() string { return s.name }

func (s *prefetchSource) Enumerate(context.Context, *plugin.Cursor) (plugin.Page, error) {
	return plugin.Page{}, nil
}

func (s *prefetchSource) Work(context.Context, plugin.WorkRef) (plugin.WorkDetail, error) {
	return plugin.WorkDetail{}, nil
}

func (s *prefetchSource) Resolve(ctx context.Context, ref plugin.WorkRef, epNo int) (plugin.Stream, error) {
	key := fmt.Sprintf("%s/%d", ref.ID, epNo)
	s.mu.Lock()
	s.calls = append(s.calls, key)
	s.inflight++
	if s.inflight > s.peak {
		s.peak = s.inflight
	}
	gate := s.gate[key]
	stubborn := s.stubborn[key]
	err := s.failOnce[key]
	delete(s.failOnce, key)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
	}()

	if gate != nil {
		if stubborn {
			<-gate
		} else {
			select {
			case <-gate:
			case <-ctx.Done():
				return plugin.Stream{}, ctx.Err()
			}
		}
	}
	if err != nil {
		return plugin.Stream{}, fmt.Errorf("剧集 %s: %w", key, err)
	}
	return plugin.Stream{URL: s.url}, nil
}

func (s *prefetchSource) Health(context.Context) error { return nil }

// setGate 让某集的解析卡在 gate 上；stubborn=true 时连 ctx 取消也不理。
func (s *prefetchSource) setGate(key string, stubborn bool) chan struct{} {
	ch := make(chan struct{})
	s.mu.Lock()
	s.gate[key] = ch
	s.stubborn[key] = stubborn
	s.mu.Unlock()
	return ch
}

// failOnceOn 让某集的第一次解析失败。
func (s *prefetchSource) failOnceOn(key string, err error) {
	s.mu.Lock()
	s.failOnce[key] = err
	s.mu.Unlock()
}

// snapshot 返回调用序列、峰值并发与当前在途数。
func (s *prefetchSource) snapshot() (calls []string, peak, inflight int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out, s.peak, s.inflight
}

func (s *prefetchSource) count(key string) int {
	calls, _, _ := s.snapshot()
	n := 0
	for _, c := range calls {
		if c == key {
			n++
		}
	}
	return n
}

func (s *prefetchSource) total() int {
	calls, _, _ := s.snapshot()
	return len(calls)
}

// prefetchRig 是预取测试台：真库（真迁移）+ 真 Resolver（真缓存/单飞/落库）+ 真 Cutter，
// 上游是一个会计数的插件替身。与 video_test.go 的 rig 互不影响（那里注入的是假解析函数）。
type prefetchRig struct {
	c        *Cutter
	st       *store.Store
	settings *store.Settings
	src      *prefetchSource
	logs     *syncBuffer
	dramaID  int64
}

// newPrefetchRig 建库与装配。chunkSize 是 merge_chunk_size（预取的「一批」= 一个块）。
func newPrefetchRig(t *testing.T, chunkSize int) *prefetchRig {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	root := filepath.Join(t.TempDir(), "cache")
	for k, v := range map[store.SettingKey]string{
		store.KeyStreamSecret:        testSecret,
		store.KeyCacheDir:            root,
		store.KeyMergeChunkSize:      strconv.Itoa(chunkSize),
		store.KeyPrefetchConcurrency: "1",
	} {
		if err := settings.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	// 真实 1 秒视频：AC-5 的 ServeSegment 需要真 ffmpeg 转封装，直链一律指它。
	path := filepath.Join(t.TempDir(), "ep.mp4")
	makeVideo(t, path, 1)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 fixture: %v", err)
	}
	cdn := newFakeCDN(t, body)

	src := &prefetchSource{
		name:     "web",
		url:      cdn.url(),
		gate:     map[string]chan struct{}{},
		stubborn: map[string]bool{},
		failOnce: map[string]error{},
	}
	reg := plugin.NewRegistry()
	if err := reg.Register(src); err != nil {
		t.Fatalf("Register: %v", err)
	}
	dramaID, err := st.UpsertDrama(ctx, store.Drama{Provider: "web", ProviderID: "d1", Title: "预取测试剧"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}

	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	resolver := stream.NewResolver(st, settings, reg)
	c := NewCutter(st, settings, logger, cache.New(st, settings, logger),
		resolver.Direct, resolver.DirectMany, resolver.Invalidate, Headers{}, nil)
	c.StartPrefetch(ctx)
	t.Cleanup(c.StopPrefetch)

	return &prefetchRig{c: c, st: st, settings: settings, src: src, logs: logs, dramaID: dramaID}
}

// addDrama 建一部剧与若干集，时长写成 10 秒（预取不触发 ffprobe，测试只关心直链）。
func (r *prefetchRig) addDrama(t *testing.T, providerID string, epNos ...int) (int64, []store.Episode) {
	t.Helper()
	ctx := context.Background()
	dramaID, err := r.st.UpsertDrama(ctx, store.Drama{Provider: "web", ProviderID: providerID, Title: providerID})
	if err != nil {
		t.Fatalf("UpsertDrama %s: %v", providerID, err)
	}
	for _, no := range epNos {
		id, err := r.st.UpsertEpisode(ctx, store.Episode{
			DramaID: dramaID, EpNo: no, Source: "web", Vid: providerID,
		})
		if err != nil {
			t.Fatalf("UpsertEpisode %s/%d: %v", providerID, no, err)
		}
		if _, err := r.st.BackfillEpisodeDuration(ctx, id, 10); err != nil {
			t.Fatalf("BackfillEpisodeDuration %s/%d: %v", providerID, no, err)
		}
	}
	eps, err := r.st.ListEpisodes(ctx, dramaID)
	if err != nil {
		t.Fatalf("ListEpisodes %s: %v", providerID, err)
	}
	return dramaID, eps
}

// addEpisodes 给主剧追加若干集。
func (r *prefetchRig) addEpisodes(t *testing.T, epNos ...int) []store.Episode {
	t.Helper()
	ctx := context.Background()
	for _, no := range epNos {
		id, err := r.st.UpsertEpisode(ctx, store.Episode{
			DramaID: r.dramaID, EpNo: no, Source: "web", Vid: "d1",
		})
		if err != nil {
			t.Fatalf("UpsertEpisode %d: %v", no, err)
		}
		if _, err := r.st.BackfillEpisodeDuration(ctx, id, 10); err != nil {
			t.Fatalf("BackfillEpisodeDuration %d: %v", no, err)
		}
	}
	eps, err := r.st.ListEpisodes(ctx, r.dramaID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	return eps
}

func (r *prefetchRig) id(uid string) string { return stream.ID(testSecret, uid) }

func (r *prefetchRig) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PlaylistRoute, r.c.ServePlaylist)
	mux.HandleFunc(SegmentRoute, r.c.ServeSegment)
	return mux
}

func (r *prefetchRig) do(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.handler().ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// waitLog 等到第一条满足条件的日志出现（后台任务是异步的，断言必须等它落地）。
func (r *prefetchRig) waitLog(t *testing.T, what string, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	var hit map[string]any
	waitFor(t, 10*time.Second, what, func() bool {
		for _, m := range r.logs.records() {
			if pred(m) {
				hit = m
				return true
			}
		}
		return false
	})
	return hit
}

// waitFor 轮询条件直到成立或超时（比固定 sleep 更稳，也避免慢机器上误判）。
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）：%s", timeout, what)
}

// logDrama 判定一条日志是否属于某一部剧（JSON 数字解出来是 float64）。
func logDrama(m map[string]any, dramaID int64) bool {
	v, ok := m["drama"].(float64)
	return ok && int64(v) == dramaID
}

// prefetchEp 造一集纯数据（分块与计划是纯函数，不需要库）。
func prefetchEp(id int64, epNo int) store.Episode {
	return store.Episode{ID: id, DramaID: 1, EpNo: epNo}
}

// batchSummary 把批次压成「tier:集号,集号」便于整段比对。
func batchSummary(t *testing.T, batches []prefetchBatch) []string {
	t.Helper()
	out := make([]string, len(batches))
	for i, b := range batches {
		nos := make([]string, len(b.eps))
		for j, ep := range b.eps {
			nos[j] = strconv.Itoa(ep.EpNo)
		}
		out[i] = strconv.Itoa(b.tier) + ":" + strings.Join(nos, ",")
	}
	return out
}

// TestPrefetchPlanOrderAndBackoff 覆盖 AC-3：当前块 → 下一块 → 其余；
// 退避期内的集被剔除、整块都剔除时该批消失；当前是最后一块时没有「下一块」。
func TestPrefetchPlanOrderAndBackoff(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	future := now.Add(10 * time.Minute).Format(time.RFC3339)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	blocks := [][]store.Episode{
		{prefetchEp(1, 1), prefetchEp(2, 2)},
		{prefetchEp(3, 3), prefetchEp(4, 4)},
		{prefetchEp(5, 5), prefetchEp(6, 6)},
	}

	t.Run("优先级顺序", func(t *testing.T) {
		got := prefetchPlan(blocks, blocks[0], nil, now)
		want := []string{"1:1,2", "2:3,4", "3:5,6"}
		if diff := batchSummary(t, got); strings.Join(diff, "|") != strings.Join(want, "|") {
			t.Fatalf("批次 = %v, 期望 %v", diff, want)
		}
	})

	t.Run("退避期内的集被剔除", func(t *testing.T) {
		fails := map[int64]store.StreamFailure{
			2: {EpisodeID: 2, Reason: stream.FailureRejected, RetryAfter: future},
			4: {EpisodeID: 4, Reason: stream.FailureUpstream, RetryAfter: future},
		}
		got := prefetchPlan(blocks, blocks[0], fails, now)
		want := []string{"1:1", "2:3", "3:5,6"}
		if diff := batchSummary(t, got); strings.Join(diff, "|") != strings.Join(want, "|") {
			t.Fatalf("批次 = %v, 期望 %v", diff, want)
		}
	})

	t.Run("整块退避时该批消失", func(t *testing.T) {
		fails := map[int64]store.StreamFailure{
			2: {EpisodeID: 2, RetryAfter: future},
			3: {EpisodeID: 3, RetryAfter: future},
			4: {EpisodeID: 4, RetryAfter: future},
		}
		got := prefetchPlan(blocks, blocks[0], fails, now)
		want := []string{"1:1", "3:5,6"}
		if diff := batchSummary(t, got); strings.Join(diff, "|") != strings.Join(want, "|") {
			t.Fatalf("批次 = %v, 期望 %v（下一块全在退避期）", diff, want)
		}
	})

	t.Run("退避过期后重新纳入", func(t *testing.T) {
		fails := map[int64]store.StreamFailure{
			2: {EpisodeID: 2, RetryAfter: past},
		}
		got := prefetchPlan(blocks, blocks[0], fails, now)
		want := []string{"1:1,2", "2:3,4", "3:5,6"}
		if diff := batchSummary(t, got); strings.Join(diff, "|") != strings.Join(want, "|") {
			t.Fatalf("批次 = %v, 期望 %v", diff, want)
		}
	})

	t.Run("retry_after 坏值按可重试处理", func(t *testing.T) {
		fails := map[int64]store.StreamFailure{1: {EpisodeID: 1, RetryAfter: "not-a-time"}}
		got := prefetchPlan(blocks, blocks[0], fails, now)
		if diff := batchSummary(t, got); diff[0] != "1:1,2" {
			t.Fatalf("批次首项 = %q, 期望 \"1:1,2\"（坏记录不能让一集永远不预取）", diff[0])
		}
	})

	t.Run("当前是最后一块", func(t *testing.T) {
		got := prefetchPlan(blocks, blocks[2], nil, now)
		want := []string{"1:5,6", "3:1,2", "3:3,4"}
		if diff := batchSummary(t, got); strings.Join(diff, "|") != strings.Join(want, "|") {
			t.Fatalf("批次 = %v, 期望 %v（没有下一块）", diff, want)
		}
	})

	t.Run("定位不到当前块", func(t *testing.T) {
		if got := prefetchPlan(blocks, nil, nil, now); got != nil {
			t.Fatalf("空 current 的批次 = %v, 期望 nil", batchSummary(t, got))
		}
		unknown := []store.Episode{prefetchEp(999, 99)}
		if got := prefetchPlan(blocks, unknown, nil, now); got != nil {
			t.Fatalf("未知集的批次 = %v, 期望 nil", batchSummary(t, got))
		}
	})
}

// TestPrefetchSingleFlightAndDramaSwitch 覆盖 AC-4：同剧重复 kick 只有一个任务；
// 换剧只停旧任务「起新批」，在途批跑完，尚未开始的批次不再外呼。
func TestPrefetchSingleFlightAndDramaSwitch(t *testing.T) {
	r := newPrefetchRig(t, 1)
	eps := r.addEpisodes(t, 1, 2, 3)
	gate := r.src.setGate("d1/1", false)

	r.c.kickPrefetch(r.dramaID, eps[:1])
	r.c.kickPrefetch(r.dramaID, eps[:1])
	waitFor(t, 10*time.Second, "第一个批次进入上游", func() bool {
		return r.src.count("d1/1") == 1
	})
	// 第二次 kick 不能开第二个任务：同剧单飞。
	time.Sleep(100 * time.Millisecond)
	if got := r.src.count("d1/1"); got != 1 {
		t.Fatalf("同剧重复 kick 后 d1/1 解析 %d 次, 期望 1", got)
	}

	// 换剧：旧任务不再起新批，但在途的那一批要跑完（只关 stop，不 cancel）。
	dramaB, epsB := r.addDrama(t, "d2", 1)
	r.c.kickPrefetch(dramaB, epsB[:1])
	close(gate)
	r.waitLog(t, "旧任务被取消", func(m map[string]any) bool {
		return m["msg"] == "prefetch run cancelled" && logDrama(m, r.dramaID)
	})

	calls, _, _ := r.src.snapshot()
	for _, c := range calls {
		if strings.HasPrefix(c, "d1/") && c != "d1/1" {
			t.Fatalf("换剧后旧任务仍在解析 %q（调用序列 %v）", c, calls)
		}
	}
	if got := r.src.count("d1/1"); got != 1 {
		t.Errorf("d1/1 解析 %d 次, 期望 1", got)
	}
	// 换剧不取消在途批，因此这一块解析成功后不该留任何失败记录。
	fails, err := r.st.StreamFailures(context.Background(), r.dramaID)
	if err != nil {
		t.Fatalf("StreamFailures: %v", err)
	}
	if len(fails) != 0 {
		t.Errorf("取消后失败记录 = %v, 期望空", fails)
	}
}

// TestPrefetchDramaSwitchKeepsForegroundFlight 守住 ADR-0017 §7 的硬要求：
// 换剧只停「起新批」，不 cancel 在途批。前台等同一集的请求必须跟在途批跑完，
// 不能读到 context.Canceled（那会被映射成 502）。
func TestPrefetchDramaSwitchKeepsForegroundFlight(t *testing.T) {
	r := newPrefetchRig(t, 1)
	eps := r.addEpisodes(t, 1)
	uid := "e" + strconv.FormatInt(eps[0].ID, 10)
	gate := r.src.setGate("d1/1", false)

	// 预取先拿到这一集的解析所有权（单飞 owner 是预取任务）。
	r.c.kickPrefetch(r.dramaID, eps[:1])
	waitFor(t, 10*time.Second, "预取进入上游", func() bool {
		return r.src.count("d1/1") == 1
	})

	// 前台来等同一集：命中单飞，挂在预取 owner 的 flight 上。
	rec := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec <- r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/0.m4s")
	}()
	time.Sleep(100 * time.Millisecond)
	if got := r.src.count("d1/1"); got != 1 {
		t.Fatalf("前台在预取在途时又发起解析：d1/1 调用 %d 次, 期望 1", got)
	}

	// 换剧：旧任务只关 stop；在途批的解析所有权不能被取消。
	dramaB, epsB := r.addDrama(t, "d2", 1)
	r.c.kickPrefetch(dramaB, epsB[:1])
	close(gate)

	select {
	case got := <-rec:
		if got.Code != http.StatusOK {
			t.Fatalf("换剧后前台分片状态码 = %d, 期望 200（body=%s）", got.Code, got.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("前台分片请求没有返回（换剧取消了在途批）")
	}
}

// TestPrefetchConcurrencyCapAndDisabled 覆盖 AC-7：并发上限真的生效；
// prefetch_concurrency=0 时 kick 不产生任何上游调用。
func TestPrefetchConcurrencyCapAndDisabled(t *testing.T) {
	t.Run("默认串行", func(t *testing.T) {
		r := newPrefetchRig(t, 1)
		eps := r.addEpisodes(t, 1, 2, 3)
		r.c.kickPrefetch(r.dramaID, eps[:1])
		r.waitLog(t, "预取跑完", func(m map[string]any) bool {
			return m["msg"] == "prefetch run finished" && logDrama(m, r.dramaID)
		})
		calls, peak, _ := r.src.snapshot()
		if peak > 1 {
			t.Errorf("峰值并发 = %d, 期望 ≤ 1（prefetch_concurrency=1）", peak)
		}
		if len(calls) != 3 {
			t.Errorf("上游调用 = %v（%d 次）, 期望 3 次（三块各一集）", calls, len(calls))
		}
	})

	t.Run("0 等于关闭", func(t *testing.T) {
		r := newPrefetchRig(t, 1)
		eps := r.addEpisodes(t, 1, 2, 3)
		if err := r.settings.Set(context.Background(), store.KeyPrefetchConcurrency, "0"); err != nil {
			t.Fatalf("Set prefetch_concurrency=0: %v", err)
		}
		r.c.kickPrefetch(r.dramaID, eps[:1])
		time.Sleep(200 * time.Millisecond)
		if got := r.src.total(); got != 0 {
			t.Fatalf("关闭预取后上游调用 %d 次, 期望 0", got)
		}
		for _, m := range r.logs.records() {
			if m["msg"] == "prefetch run started" {
				t.Fatalf("关闭预取不该起任务：%v", m)
			}
		}
	})
}

// TestPrefetchLogsPriorityOrder 覆盖 AC-8：每批一条日志，tier 与集号顺序就是
// 「当前块 → 下一块 → 其余」（默认并发 1 时顺序严格）。
func TestPrefetchLogsPriorityOrder(t *testing.T) {
	r := newPrefetchRig(t, 1)
	eps := r.addEpisodes(t, 1, 2, 3)
	r.c.kickPrefetch(r.dramaID, eps[:1])
	r.waitLog(t, "预取跑完", func(m map[string]any) bool {
		return m["msg"] == "prefetch run finished" && logDrama(m, r.dramaID)
	})

	var got []string
	for _, m := range r.logs.records() {
		if m["msg"] != "prefetch batch" || !logDrama(m, r.dramaID) {
			continue
		}
		tier, _ := m["tier"].(float64)
		from, _ := m["ep_from"].(float64)
		got = append(got, fmt.Sprintf("%d:%d", int(tier), int(from)))
	}
	want := []string{"1:1", "2:2", "3:3"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("批次日志顺序 = %v, 期望 %v", got, want)
	}
}

// TestPrefetchFailureBackoffAndForegroundFallback 覆盖 AC-6：失败落表（分类 + 下次
// 重试时刻）、同一任务不重试、前台播放该集仍同步解析成功。
func TestPrefetchFailureBackoffAndForegroundFallback(t *testing.T) {
	r := newPrefetchRig(t, 1)
	eps := r.addEpisodes(t, 1, 2, 3)
	r.src.failOnceOn("d1/2", plugin.ErrRejected)

	before := time.Now()
	r.c.kickPrefetch(r.dramaID, eps[:1])
	r.waitLog(t, "预取跑完", func(m map[string]any) bool {
		return m["msg"] == "prefetch run finished" && logDrama(m, r.dramaID)
	})

	fails, err := r.st.StreamFailures(context.Background(), r.dramaID)
	if err != nil {
		t.Fatalf("StreamFailures: %v", err)
	}
	f, ok := fails[eps[1].ID]
	if !ok {
		t.Fatalf("集 %d 没有失败记录（全部记录：%v）", eps[1].ID, fails)
	}
	if f.Reason != stream.FailureRejected {
		t.Errorf("reason = %q, 期望 %q", f.Reason, stream.FailureRejected)
	}
	retryAt, err := time.Parse(time.RFC3339, f.RetryAfter)
	if err != nil {
		t.Fatalf("解析 retry_after %q: %v", f.RetryAfter, err)
	}
	// 默认 prefetch_retry_sec=900：下次重试约在 15 分钟后（给足调度抖动余量）。
	if d := retryAt.Sub(before); d < 14*time.Minute || d > 16*time.Minute {
		t.Errorf("retry_after 距开始 %v, 期望约 15 分钟", d)
	}
	if got := r.src.count("d1/2"); got != 1 {
		t.Errorf("同一任务内 d1/2 解析 %d 次, 期望 1（不紧循环重试）", got)
	}

	// 前台不读失败表：播放这一集仍现解析一次并成功（第 2 次上游调用）。
	rec := r.do(t, http.MethodGet, "/video/"+r.id("e"+strconv.FormatInt(eps[1].ID, 10))+"/0.m4s")
	if rec.Code != http.StatusOK {
		t.Fatalf("失败集的前台分片状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if got := r.src.count("d1/2"); got != 2 {
		t.Errorf("前台播放后 d1/2 解析 %d 次, 期望 2", got)
	}
	// 前台成功不写失败表：这张表只有预取器维护（失败记录要等下一次预取成功才清）。
	fails, err = r.st.StreamFailures(context.Background(), r.dramaID)
	if err != nil {
		t.Fatalf("前台播放后 StreamFailures: %v", err)
	}
	if _, ok := fails[eps[1].ID]; !ok {
		t.Error("前台播放不该清除预取失败记录（前台路径不读也不写这张表）")
	}
}

// TestPrefetchForegroundNotBlocked 覆盖 AC-5：预取卡住时，前台播放列表与分片
// 都不等它、不报预取错误。
func TestPrefetchForegroundNotBlocked(t *testing.T) {
	r := newPrefetchRig(t, 1)
	eps := r.addEpisodes(t, 1, 2, 3)
	gate := r.src.setGate("d1/3", false)
	uid := "e" + strconv.FormatInt(eps[0].ID, 10)

	// 第一次 playlist 成功后就起预取；第三块（d1/3）会被卡住。
	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("首次 playlist 状态码 = %d, 期望 200", rec.Code)
	}
	waitFor(t, 10*time.Second, "预取卡在 d1/3", func() bool {
		return r.src.count("d1/3") == 1
	})

	start := time.Now()
	rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8")
	if rec.Code != http.StatusOK {
		t.Fatalf("预取卡住时的 playlist 状态码 = %d, 期望 200", rec.Code)
	}
	seg := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/0.m4s")
	if seg.Code != http.StatusOK {
		t.Fatalf("预取卡住时的分片状态码 = %d, 期望 200（body=%s）", seg.Code, seg.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("前台请求耗时 %v，期望不被预取拖住", elapsed)
	}

	close(gate)
	r.waitLog(t, "预取跑完", func(m map[string]any) bool {
		return m["msg"] == "prefetch run finished" && logDrama(m, r.dramaID)
	})
	if got := r.src.count("d1/3"); got != 1 {
		t.Errorf("d1/3 解析 %d 次, 期望 1（前台请求不该重复触发预取）", got)
	}
}

// TestPrefetchStop 覆盖 AC-9：StopPrefetch 之后不再起新批；在途批未完成时
// 最多等 prefetchStopTimeout 就返回。
func TestPrefetchStop(t *testing.T) {
	t.Run("取消后不再起新批", func(t *testing.T) {
		r := newPrefetchRig(t, 1)
		eps := r.addEpisodes(t, 1, 2, 3)
		gate := r.src.setGate("d1/1", false)
		r.c.kickPrefetch(r.dramaID, eps[:1])
		waitFor(t, 10*time.Second, "第一批进入上游", func() bool { return r.src.count("d1/1") == 1 })

		start := time.Now()
		r.c.StopPrefetch()
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("StopPrefetch 耗时 %v, 期望远小于上限（在途批响应取消）", elapsed)
		}
		close(gate)
		if got := r.src.total(); got != 1 {
			t.Errorf("停止后上游调用 %d 次, 期望 1（未开始的批次被丢弃）", got)
		}

		// 停止之后再 kick 也不该起任务（pfCtx 已清空）。
		r.c.kickPrefetch(r.dramaID, eps[:1])
		time.Sleep(100 * time.Millisecond)
		if got := r.src.total(); got != 1 {
			t.Errorf("StopPrefetch 后 kick 又外呼了：%d 次", got)
		}
	})

	t.Run("在途批不理取消时按上限返回", func(t *testing.T) {
		r := newPrefetchRig(t, 1)
		eps := r.addEpisodes(t, 1, 2, 3)
		gate := r.src.setGate("d1/1", true)
		r.c.kickPrefetch(r.dramaID, eps[:1])
		waitFor(t, 10*time.Second, "第一批进入上游", func() bool { return r.src.count("d1/1") == 1 })

		start := time.Now()
		r.c.StopPrefetch()
		elapsed := time.Since(start)
		if elapsed < prefetchStopTimeout {
			t.Errorf("StopPrefetch 耗时 %v, 期望至少等满 %v（在途批未完成）", elapsed, prefetchStopTimeout)
		}
		if elapsed > prefetchStopTimeout+5*time.Second {
			t.Errorf("StopPrefetch 耗时 %v, 期望不超过 %v + 余量", elapsed, prefetchStopTimeout)
		}

		// 放行在途批，等它收尾（取消后不得写失败记录）。
		close(gate)
		r.waitLog(t, "任务取消", func(m map[string]any) bool {
			return m["msg"] == "prefetch run cancelled" && logDrama(m, r.dramaID)
		})
		fails, err := r.st.StreamFailures(context.Background(), r.dramaID)
		if err != nil {
			t.Fatalf("StreamFailures: %v", err)
		}
		if len(fails) != 0 {
			t.Errorf("停机取消后失败记录 = %v, 期望空", fails)
		}
	})
}

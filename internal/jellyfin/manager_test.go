package jellyfin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// harness 把 Manager 与假 Jellyfin 装在一起：一轮跑完用 Wait，之后断言状态与请求。
type harness struct {
	t           *testing.T
	ff          *fakeJellyfin
	cfg         Config
	manager     *Manager
	cats        []string
	catsErr     error
	disabled    []string
	disabledErr error
}

func newHarness(t *testing.T, libs []Library, categories []string) *harness {
	t.Helper()
	fake := &fakeJellyfin{libraries: libs}
	_, cfg := newFakeJellyfin(t, fake)
	h := &harness{t: t, ff: fake, cfg: cfg, cats: categories}
	h.manager = New(
		func(context.Context) (Config, error) { return h.cfg, nil },
		func(context.Context) ([]string, error) { return h.cats, h.catsErr },
		func(context.Context) ([]string, error) { return h.disabled, h.disabledErr },
		slog.New(slog.DiscardHandler),
	)
	return h
}

// run 触发一轮并等它收尾：Start 只派 goroutine，断言前必须先 Wait（否则读的是半轮状态）。
func (h *harness) run() {
	h.t.Helper()
	if err := h.manager.Start(context.Background()); err != nil {
		h.t.Fatalf("Start: %v", err)
	}
	h.manager.Wait()
}

// state 读一次状态面。
func (h *harness) state() State { return h.manager.State(context.Background()) }

// waitForState 轮询状态面直到 cond 成立（最多 5 秒）：进度是异步写进状态的，测试只能
// 「等到它出现」—— 与页面轮询 GET /api/jellyfin/libraries 走的是同一条读路径。
func (h *harness) waitForState(what string, cond func(State) bool) State {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := h.state(); cond(st) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("等不到状态「%s」（当前 %+v）", what, h.state())
	return State{}
}

// TestManagerCreatesOneLibraryPerCategory 锁定「一分类一库」（AC-2）：每个分类一个
// POST，库名带前缀、路径是 <root>/<分类>，末尾恰好一次扫描。
func TestManagerCreatesOneLibraryPerCategory(t *testing.T) {
	h := newHarness(t, nil, []string{"都市", "爱情", "科幻"})
	h.run()

	writes := h.ff.writes()
	if len(writes) != 4 {
		t.Fatalf("写请求数 = %d, 期望 3 建库 + 1 扫描（%+v）", len(writes), writes)
	}
	for i, category := range []string{"都市", "爱情", "科幻"} {
		req := writes[i]
		if req.method != http.MethodPost || req.path != pathLibraries {
			t.Errorf("第 %d 个请求 = %s %s, 期望 POST %s", i, req.method, req.path, pathLibraries)
		}
		if got := req.query.Get("name"); got != "短剧 - "+category {
			t.Errorf("库名 = %q, 期望 前缀 + %q", got, category)
		}
		if got, want := req.query.Get("paths"), h.cfg.LibraryRoot+"/"+category; got != want {
			t.Errorf("库路径 = %q, 期望 %q", got, want)
		}
		if got := req.query.Get("collectionType"); got != "tvshows" {
			t.Errorf("collectionType = %q, 期望 tvshows", got)
		}
		if got := req.query.Get("refreshLibrary"); got != "false" {
			t.Errorf("refreshLibrary = %q, 期望 false（扫描统一在末尾一次）", got)
		}
	}
	last := writes[len(writes)-1]
	if last.path != pathRefresh {
		t.Errorf("最后一个写请求 = %s, 期望 %s", last.path, pathRefresh)
	}

	state := h.state()
	if state.Created != 3 {
		t.Errorf("State.Created = %d, 期望 3", state.Created)
	}
	if state.LastError != "" {
		t.Errorf("State.LastError = %q, 期望空", state.LastError)
	}
	if state.LastOKAt.IsZero() || state.LastRunAt.IsZero() {
		t.Error("一轮成功后 last_run_at / last_ok_at 都应非零")
	}
	if state.Running {
		t.Error("一轮收尾后 Running 应为 false")
	}
}

// TestManagerIdempotent 锁定「再点一次 0 新增、0 写请求」（AC-3）。
func TestManagerIdempotent(t *testing.T) {
	h := newHarness(t, nil, []string{"都市", "爱情"})
	h.run()

	// 第二轮前把库「建好」：假 Jellyfin 现在返回这两个自管库，与真实状态一致。
	h.ff.libraries = []Library{
		{Name: "短剧 - 都市", Locations: []string{h.cfg.LibraryRoot + "/都市"}, CollectionType: "tvshows"},
		{Name: "短剧 - 爱情", Locations: []string{h.cfg.LibraryRoot + "/爱情"}, CollectionType: "tvshows"},
	}
	before := len(h.ff.seen())
	h.run()

	// 第一轮 = 2 建库 + 1 扫描；第二轮 0 写。
	if writes := h.ff.writes(); len(writes) != 3 {
		t.Fatalf("第二轮后写请求总数 = %d, 期望仍是第一轮的 3 个（幂等不做任何写）", len(writes))
	}
	if got := len(h.ff.seen()) - before; got != 1 {
		t.Errorf("第二轮请求数 = %d, 期望只有 1 次列库", got)
	}
	state := h.state()
	if state.Created != 0 {
		t.Errorf("第二轮 State.Created = %d, 期望 0", state.Created)
	}
	if state.LastError != "" {
		t.Errorf("第二轮 State.LastError = %q, 期望空", state.LastError)
	}
}

// TestManagerReportsOrphanWithoutDeleting 锁定「分类消失只报告」：没有对应的开关行时
// 不产生任何 DELETE（删除只由显式关闭的开关触发，MKACG-668）。
func TestManagerReportsOrphanWithoutDeleting(t *testing.T) {
	root := "/data/video/dramahub"
	h := newHarness(t, []Library{
		{Name: "短剧 - 都市", Locations: []string{root + "/都市"}},
		{Name: "短剧 - 已下架", Locations: []string{root + "/已下架"}},
	}, []string{"都市"})
	h.run()

	for _, req := range h.ff.writes() {
		if req.method == http.MethodDelete {
			t.Errorf("分类消失不该删库，实际发了 DELETE %q", req.query.Get("name"))
		}
	}
	state := h.state()
	if len(state.Orphans) != 1 || state.Orphans[0] != "短剧 - 已下架" {
		t.Errorf("State.Orphans = %v, 期望 [短剧 - 已下架]", state.Orphans)
	}
	if state.LastOKAt.IsZero() {
		t.Error("孤儿只报告、不算失败：last_ok_at 应前进")
	}
}

// TestManagerDeletesOnlyDisabledOwnLibraries 锁定删除边界（MKACG-668）：关掉的分类对应的
// 自管库删掉；别人的库、带前缀但路径不对的库、root 下没前缀的库一个都不碰。
func TestManagerDeletesOnlyDisabledOwnLibraries(t *testing.T) {
	root := "/data/video/dramahub"
	h := newHarness(t, []Library{
		{Name: "短剧 - 都市", Locations: []string{root + "/都市"}},            // 开着：不动
		{Name: "短剧 - 爱情", Locations: []string{root + "/爱情"}},            // 关着：删
		{Name: "Movies", Locations: []string{"/data/video/movies"}},     // 别人的库：不动
		{Name: "短剧 - 别处", Locations: []string{"/data/video/elsewhere"}}, // 前缀但不是我们的路径：不动
		{Name: "合集", Locations: []string{root + "/合集"}},                 // root 下但没前缀：不动
	}, []string{"都市", "爱情"})
	h.disabled = []string{"爱情"}
	h.run()

	var deleted []string
	for _, req := range h.ff.writes() {
		if req.method == http.MethodDelete {
			deleted = append(deleted, req.query.Get("name"))
		}
	}
	if len(deleted) != 1 || deleted[0] != "短剧 - 爱情" {
		t.Fatalf("DELETE 的库 = %v, 期望只有 [短剧 - 爱情]", deleted)
	}
	state := h.state()
	if state.Created != 0 || state.Removed != 1 {
		t.Errorf("created = %d / removed = %d, 期望 0 / 1", state.Created, state.Removed)
	}
	if state.LastError != "" || state.LastOKAt.IsZero() {
		t.Errorf("删库成功：last_error = %q（期望空）/ last_ok_at = %v（期望非零）",
			state.LastError, state.LastOKAt)
	}
	if got := len(h.ff.libraries); got != 4 {
		t.Errorf("假 Jellyfin 剩下 %d 个库, 期望 4（只删了 短剧 - 爱情）", got)
	}
	for _, lib := range h.ff.libraries {
		if lib.Name == "短剧 - 爱情" {
			t.Error("短剧 - 爱情 应该已经被删掉")
		}
	}
}

// TestManagerAllDisabledDeletesAll 锁定全关（MKACG-668 AC-5）：一轮把托管库全删、
// 不报错，用户自己建的库仍在。
func TestManagerAllDisabledDeletesAll(t *testing.T) {
	root := "/data/video/dramahub"
	h := newHarness(t, []Library{
		{Name: "短剧 - 都市", Locations: []string{root + "/都市"}},
		{Name: "短剧 - 爱情", Locations: []string{root + "/爱情"}},
		{Name: "Movies", Locations: []string{"/data/video/movies"}},
	}, []string{"都市", "爱情"})
	h.disabled = []string{"都市", "爱情"}
	h.run()

	state := h.state()
	if state.Removed != 2 || state.Created != 0 {
		t.Errorf("removed = %d / created = %d, 期望 2 / 0", state.Removed, state.Created)
	}
	if state.LastError != "" {
		t.Errorf("全关是正常结局，last_error = %q, 期望空", state.LastError)
	}
	if got := len(h.ff.libraries); got != 1 || h.ff.libraries[0].Name != "Movies" {
		t.Errorf("假 Jellyfin 剩下的库 = %+v, 期望只剩用户手建的 Movies", h.ff.libraries)
	}
}

// TestManagerReenableRebuilds 锁定「重新开启 → 下一轮重建」（AC-4）：不需要重启，
// 开关只是下一轮读到的输入。
func TestManagerReenableRebuilds(t *testing.T) {
	root := "/data/video/dramahub"
	h := newHarness(t, []Library{
		{Name: "短剧 - 都市", Locations: []string{root + "/都市"}},
	}, []string{"都市", "爱情"})
	h.disabled = []string{"爱情"}
	h.run() // 爱情还关着：不建

	if state := h.state(); state.Created != 0 {
		t.Fatalf("关闭的分类不该建库，created = %d", state.Created)
	}

	h.disabled = nil
	before := len(h.ff.writes())
	h.run()
	writes := h.ff.writes()[before:]
	if len(writes) != 2 {
		t.Fatalf("重新开启后的写请求 = %d, 期望 1 建库 + 1 扫描：%+v", len(writes), writes)
	}
	if got := writes[0].query.Get("name"); got != "短剧 - 爱情" {
		t.Errorf("重建的库 = %q, 期望 短剧 - 爱情", got)
	}
	if state := h.state(); state.Created != 1 || state.LastError != "" {
		t.Errorf("created = %d / last_error = %q, 期望 1 / 空", state.Created, state.LastError)
	}
}

// TestManagerDeleteFailureVisibleAndRetried 锁定删库失败不静默、不放弃（AC-6）：
// 一次 500 只让这一项进结果面（last_error 点名 + delete_failed），下一轮自动重试成功。
func TestManagerDeleteFailureVisibleAndRetried(t *testing.T) {
	root := "/data/video/dramahub"
	h := newHarness(t, []Library{
		{Name: "短剧 - 爱情", Locations: []string{root + "/爱情"}},
	}, []string{"爱情"})
	h.disabled = []string{"爱情"}
	h.ff.deleteFail = map[string]bool{"短剧 - 爱情": true}

	h.run()

	state := h.state()
	if state.Removed != 0 {
		t.Errorf("删库失败时 removed = %d, 期望 0", state.Removed)
	}
	if len(state.DeleteFailed) != 1 || state.DeleteFailed[0] != "爱情" {
		t.Errorf("State.DeleteFailed = %v, 期望 [爱情]（结果面要点名）", state.DeleteFailed)
	}
	if !strings.Contains(state.LastError, "爱情") || !strings.Contains(state.LastError, "重试") {
		t.Errorf("State.LastError = %q, 期望点名失败的分类并说明下一轮重试", state.LastError)
	}
	if !state.LastOKAt.IsZero() {
		t.Error("期望状态没达到：last_ok_at 不该前进")
	}

	// 下一轮：Jellyfin 恢复正常，同一项自动重试成功。
	h.ff.mu.Lock()
	h.ff.deleteFail = nil
	h.ff.mu.Unlock()
	h.run()
	if state := h.state(); state.Removed != 1 || len(state.DeleteFailed) != 0 || state.LastError != "" {
		t.Errorf("重试后 removed = %d / delete_failed = %v / last_error = %q, 期望 1 / 空 / 空",
			state.Removed, state.DeleteFailed, state.LastError)
	}
}

// TestManagerDeleteIdempotent 锁定删除幂等（AC-8）：删库回 500 但库里已经没有它了
// （服务端删成了、客户端没收到 204），这一项算完成，不报错。
func TestManagerDeleteIdempotent(t *testing.T) {
	root := "/data/video/dramahub"
	h := newHarness(t, []Library{
		{Name: "短剧 - 爱情", Locations: []string{root + "/爱情"}},
	}, []string{"爱情"})
	h.disabled = []string{"爱情"}
	h.ff.deleteVanish = map[string]bool{"短剧 - 爱情": true}

	h.run()

	state := h.state()
	if state.Removed != 1 {
		t.Errorf("removed = %d, 期望 1（目标库已不存在 = 这一项完成）", state.Removed)
	}
	if len(state.DeleteFailed) != 0 || state.LastError != "" {
		t.Errorf("delete_failed = %v / last_error = %q, 期望都为空（幂等不报错）",
			state.DeleteFailed, state.LastError)
	}
}

// TestManagerConflictSkips 锁定同名冲突：名字被非自管库占着时不建、不删、不接管，
// 但原因要出现在状态面上。
func TestManagerConflictSkips(t *testing.T) {
	h := newHarness(t, []Library{
		{Name: "短剧 - 都市", Locations: []string{"/data/video/other/都市"}},
	}, []string{"都市"})
	h.run()

	if writes := h.ff.writes(); len(writes) != 0 {
		t.Errorf("冲突时不该有任何写请求，实际 %+v", writes)
	}
	state := h.state()
	if len(state.Conflicts) != 1 || state.Conflicts[0] != "短剧 - 都市" {
		t.Errorf("State.Conflicts = %v, 期望 [短剧 - 都市]", state.Conflicts)
	}
	if !strings.Contains(state.LastError, "短剧 - 都市") {
		t.Errorf("State.LastError = %q, 期望点出冲突的库名", state.LastError)
	}
	if state.LastOKAt.IsZero() {
		t.Error("冲突不是失败：last_ok_at 应前进")
	}
}

// TestManagerJellyfinDownIsolated 锁定失败隔离（AC-6）：Jellyfin 不可达/非 2xx 时
// 一轮只留一条 last_error，不 panic、不阻塞；状态面里也没有 api key。
func TestManagerJellyfinDownIsolated(t *testing.T) {
	h := newHarness(t, nil, []string{"都市"})
	h.ff.status = http.StatusInternalServerError
	h.ff.failList = true
	h.ff.body = `{"message":"unauthorized: ` + h.cfg.APIKey + `"}`
	h.run()

	state := h.state()
	if state.LastError == "" {
		t.Fatal("Jellyfin 失败后 State.LastError 应非空")
	}
	if strings.Contains(state.LastError, h.cfg.APIKey) {
		t.Errorf("State.LastError 带出了 api key：%q", state.LastError)
	}
	if !strings.Contains(state.LastError, "500") {
		t.Errorf("State.LastError = %q, 期望含状态码 500", state.LastError)
	}
	if !state.LastOKAt.IsZero() {
		t.Error("从没成功过时 last_ok_at 应保持零值")
	}
	if writes := h.ff.writes(); len(writes) != 0 {
		t.Errorf("列库失败后不该继续建库，实际发了 %d 个写请求", len(writes))
	}
}

// TestManagerCategoriesError 锁定分类读取失败也走同一条失败路径（vfs 挂了不影响主服务）。
func TestManagerCategoriesError(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.catsErr = errors.New("库连接断了")
	h.run()

	if state := h.state(); !strings.Contains(state.LastError, "列分类") {
		t.Errorf("State.LastError = %q, 期望说明是列分类失败（原因 %v）", state.LastError, h.catsErr)
	}
	if writes := h.ff.writes(); len(writes) != 0 {
		t.Errorf("列分类失败后不该建库，实际 %d 个写请求", len(writes))
	}
}

// TestManagerCategories 锁定分类列表与开关状态的合并（MKACG-668 AC-1）：
// 每一行都来自 vfs 的分类口径，enabled = 不在关闭集合里；读开关失败原样上报。
func TestManagerCategories(t *testing.T) {
	h := newHarness(t, nil, []string{"都市", "爱情"})
	h.disabled = []string{"爱情"}

	got, err := h.manager.Categories(context.Background())
	if err != nil {
		t.Fatalf("Categories: %v", err)
	}
	want := []CategoryState{{Name: "都市", Enabled: true}, {Name: "爱情", Enabled: false}}
	if !slices.Equal(got, want) {
		t.Errorf("Categories = %v, 期望 %v", got, want)
	}

	h.disabledErr = errors.New("库连接断了")
	if _, err := h.manager.Categories(context.Background()); err == nil {
		t.Error("读开关失败应上报错误，而不是编一个全开的列表")
	}
}

// TestManagerDisabledErrorAborts 锁定开关表读失败时整轮中止：宁可这一轮不动，
// 也不要把「读不到开关」当成「全部关闭」去删库。
func TestManagerDisabledErrorAborts(t *testing.T) {
	root := "/data/video/dramahub"
	h := newHarness(t, []Library{
		{Name: "短剧 - 都市", Locations: []string{root + "/都市"}},
	}, []string{"都市"})
	h.disabledErr = errors.New("库连接断了")
	h.run()

	if state := h.state(); !strings.Contains(state.LastError, "分类开关") {
		t.Errorf("State.LastError = %q, 期望说明读分类开关失败", state.LastError)
	}
	for _, req := range h.ff.writes() {
		if req.method == http.MethodDelete {
			t.Errorf("读不到开关时不该删库，实际发了 DELETE %q", req.query.Get("name"))
		}
	}
}

// TestManagerBusy 锁定单飞：一轮还在跑时第二次触发返回 ErrBusy，不排队、不重复建库。
func TestManagerBusy(t *testing.T) {
	block := make(chan struct{})
	h := newHarness(t, nil, []string{"都市"})
	h.ff.block = block

	if err := h.manager.Start(context.Background()); err != nil {
		t.Fatalf("第一次 Start: %v", err)
	}
	if err := h.manager.Start(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("撞车时 Start = %v, 期望 ErrBusy", err)
	}
	close(block)
	h.manager.Wait()

	if state := h.state(); state.Running {
		t.Error("收尾后 Running 应为 false")
	}
}

// TestManagerNotConfigured 锁定未配置的语义：三项非空才启用，错误文本点出缺哪个键。
func TestManagerNotConfigured(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantKey string
	}{
		{"缺 url", func(c *Config) { c.URL = "" }, "jellyfin_url"},
		{"缺 key", func(c *Config) { c.APIKey = "" }, "jellyfin_api_key"},
		{"缺 root", func(c *Config) { c.LibraryRoot = "" }, "jellyfin_library_root"},
		{"空前缀", func(c *Config) { c.Prefix = "" }, "jellyfin_library_prefix"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, nil, []string{"都市"})
			c.mutate(&h.cfg)
			err := h.manager.Start(context.Background())
			if !errors.Is(err, ErrNotConfigured) {
				t.Fatalf("Start = %v, 期望 ErrNotConfigured", err)
			}
			if !strings.Contains(err.Error(), c.wantKey) {
				t.Errorf("错误文本 = %q, 期望点名 %s", err, c.wantKey)
			}
			if len(h.ff.seen()) != 0 {
				t.Errorf("未配置时不该发任何请求，实际 %d 个", len(h.ff.seen()))
			}
			state := h.state()
			if state.Configured {
				t.Error("State.Configured 应为 false")
			}
			if got, want := len(state.Missing), len(h.cfg.Missing()); got != want {
				t.Errorf("State.Missing = %v, 期望与 Config.Missing 一致（%v）", state.Missing, h.cfg.Missing())
			}
			if want := h.cfg.APIKey != ""; state.APIKeySet != want {
				t.Errorf("State.APIKeySet = %v, 期望 %v", state.APIKeySet, want)
			}
		})
	}
}

// TestManagerStateHidesAPIKey 锁定状态面不含密钥（AC-8）：State 里只有 APIKeySet。
func TestManagerStateHidesAPIKey(t *testing.T) {
	h := newHarness(t, nil, []string{"都市"})
	state := h.state()
	if !state.APIKeySet {
		t.Error("配了 api key 时 APIKeySet 应为 true")
	}
	if !state.Configured {
		t.Error("三项齐全时 Configured 应为 true")
	}
	// 结构体里没有 key 字段本身就是这条约束的实现；这里再钉一次「值不可达」：
	// 任何字符串字段都不该出现 key 的内容。
	for _, field := range []string{state.URL, state.LibraryRoot, state.LibraryPrefix, state.LastError} {
		if strings.Contains(field, h.cfg.APIKey) {
			t.Errorf("状态面字段带出了 api key：%q", field)
		}
	}
}

// TestManagerTimeoutBounded 锁定整体预算：Jellyfin 卡住时一轮在 2 × 单请求超时左右收尾，
// 不会把 goroutine 永久挂住。
func TestManagerTimeoutBounded(t *testing.T) {
	block := make(chan struct{})
	h := newHarness(t, nil, []string{"都市"})
	h.ff.block = block
	h.cfg.Timeout = 20 * time.Millisecond
	t.Cleanup(func() { close(block) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.manager.Start(context.Background())
		h.manager.Wait()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("一轮没有在预期时间内收尾（整体预算失效）")
	}
	if state := h.state(); state.LastError == "" {
		t.Error("超时也应留下 last_error")
	}
}

// TestRoundBudgetScalesWithRequests 锁定「整轮预算与请求数挂钩」（AC-1 的根因）：
// 固定成 2 × 单请求超时时，58 个分类根本装不进去 —— 实测一次点击只建 15 个就整轮
// 中止（last_error = context deadline exceeded）。预算必须是单请求超时的正倍数，
// 且随请求数线性增长。
func TestRoundBudgetScalesWithRequests(t *testing.T) {
	cases := []struct {
		name     string
		requests int
		timeout  time.Duration
		want     time.Duration
	}{
		{"0 个请求", 0, time.Second, time.Second},
		{"2 个请求（列库 + 列分类）", 2, time.Second, 3 * time.Second},
		{"58 个建库", 58, 10 * time.Second, 590 * time.Second},
		{"负数按 0 处理", -3, time.Second, time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := roundBudget(c.requests, c.timeout); got != c.want {
				t.Errorf("roundBudget(%d, %v) = %v, 期望 %v", c.requests, c.timeout, got, c.want)
			}
		})
	}
}

// TestManagerRoundCoversAllCreates 是 AC-1 的回归：库数多、单次建库偏慢时，一轮仍然
// 把**全部**缺的库建完，不因整轮预算过小而中途放弃。
//
// 构造：8 个分类、单次超时 500ms、每次建库 250ms（远小于单次超时，但 8 次 = 2s）。
// 旧的 2 × 500ms = 1s 预算必然在 4 个左右中止；新预算 (8+1) × 500ms = 4.5s 够跑完。
func TestManagerRoundCoversAllCreates(t *testing.T) {
	categories := []string{"都市", "爱情", "科幻", "乡村", "古装", "悬疑", "年代", "战神"}
	h := newHarness(t, nil, categories)
	h.cfg.Timeout = 500 * time.Millisecond
	h.ff.writeDelay = 250 * time.Millisecond

	h.run()

	state := h.state()
	if state.Created != len(categories) {
		t.Errorf("State.Created = %d, 期望 %d（一轮建满，不因整轮预算中途放弃）",
			state.Created, len(categories))
	}
	if state.LastError != "" {
		t.Errorf("State.LastError = %q, 期望空", state.LastError)
	}
	if got := len(h.ff.writes()); got != len(categories)+1 {
		t.Errorf("写请求 = %d, 期望 %d 建库 + 1 扫描", got, len(categories))
	}
}

// TestManagerCreateTimeoutMarksUncertain 锁定「Jellyfin 已建成但客户端超时」的区分：
// 建库请求超时时我们并不知道 Jellyfin 有没有建成（实测 12.1 常见：扫库期间单次 >10s）。
// 状态面必须把这一项单独报出来，不能读成「一个都没建成」；下一轮列库看到它即幂等收口。
func TestManagerCreateTimeoutMarksUncertain(t *testing.T) {
	h := newHarness(t, nil, []string{"都市"})
	h.cfg.Timeout = 20 * time.Millisecond
	h.ff.writeDelay = 200 * time.Millisecond // 服务端会建成，客户端先超时

	h.run()

	state := h.state()
	if state.Created != 0 {
		t.Errorf("State.Created = %d, 期望 0（超时的那次没收到 204，不能算建成）", state.Created)
	}
	if state.Uncertain != 1 {
		t.Errorf("State.Uncertain = %d, 期望 1（超时 = 结果未知，不能读成「没建成」）", state.Uncertain)
	}
	if !strings.Contains(state.LastError, "结果未知") {
		t.Errorf("State.LastError = %q, 期望点明建库结果未知", state.LastError)
	}
	if !state.LastOKAt.IsZero() {
		t.Error("结果未知不算「成功」：last_ok_at 不该被这一轮推进")
	}

	// 下一轮：Jellyfin 那边已经有这个库了 → 0 新增、0 写请求（幂等接管）。
	h.ff.mu.Lock()
	h.ff.writeDelay = 0
	h.ff.mu.Unlock()
	h.ff.libraries = []Library{{Name: "短剧 - 都市", Locations: []string{h.cfg.LibraryRoot + "/都市"}}}
	before := len(h.ff.writes())
	h.run()
	if got := len(h.ff.writes()) - before; got != 0 {
		t.Errorf("结果未知后的下一轮发了 %d 个写请求, 期望 0（幂等接管已建成的库）", got)
	}
	if state := h.state(); state.Uncertain != 0 || state.LastError != "" {
		t.Errorf("收口后 Uncertain = %d / LastError = %q, 期望 0 / 空", state.Uncertain, state.LastError)
	}
}

// TestManagerBadPathDoesNotAbort 锁定「一个坏分类不能拖垮整轮」。
//
// 实测 12.1：库路径在 Jellyfin 容器里不存在时 POST /Library/VirtualFolders 回 400
// （ArgumentException: The specified path does not exist）。分类目录是 vfs 现算的，
// 与 Jellyfin 容器里的实际挂载之间必然存在窗口（rclone 还没刷出目录、挂载漏了分类、
// 部署把 library_root 填错一层）。早先这里整轮中止 —— 于是**一个**坏路径让其余所有
// 分类都建不出来，且每轮都停在同一处，正是 AC-1「点一次建不满」的第二种成因。
func TestManagerBadPathDoesNotAbort(t *testing.T) {
	h := newHarness(t, nil, []string{"都市", "坏分类", "科幻"})
	h.ff.badPaths = map[string]bool{"/data/video/dramahub/坏分类": true}

	h.run()

	state := h.state()
	if state.Created != 2 {
		t.Errorf("State.Created = %d, 期望 2（坏分类被跳过，其余照建）", state.Created)
	}
	if len(state.Failed) != 1 || state.Failed[0] != "坏分类" {
		t.Errorf("State.Failed = %v, 期望 [坏分类]（坏分类要单独报出来）", state.Failed)
	}
	if !strings.Contains(state.LastError, "坏分类") {
		t.Errorf("State.LastError = %q, 期望点出失败的分类", state.LastError)
	}
	if state.LastOKAt.IsZero() {
		t.Error("部分成功不算整轮失败：last_ok_at 应前进")
	}

	// 下一轮路径好了就补上，且已建的两个库不动（幂等）。
	h.ff.mu.Lock()
	h.ff.badPaths = nil
	h.ff.mu.Unlock()
	h.ff.libraries = []Library{
		{Name: "短剧 - 都市", Locations: []string{h.cfg.LibraryRoot + "/都市"}},
		{Name: "短剧 - 科幻", Locations: []string{h.cfg.LibraryRoot + "/科幻"}},
	}
	before := len(h.ff.writes())
	h.run()
	writes := h.ff.writes()[before:]
	if len(writes) != 2 { // 1 建库（坏分类补上）+ 1 扫描
		t.Fatalf("第二轮写请求 = %d, 期望 2（1 建库 + 1 扫描）：%+v", len(writes), writes)
	}
	if got := writes[0].query.Get("name"); got != "短剧 - 坏分类" {
		t.Errorf("第二轮建的库 = %q, 期望只补 短剧 - 坏分类", got)
	}
	if state := h.state(); len(state.Failed) != 0 || state.LastError != "" {
		t.Errorf("补上后 Failed = %v / LastError = %q, 期望空", state.Failed, state.LastError)
	}
}

// TestManagerStateReportsProgress 锁定「运行中能看到跑到哪一步」（MKACG-664 AC-1）：
// 建库阶段状态面给出当前阶段与本轮总数，处理过的条数随建库推进；收尾时阶段归空、
// 计数与耗时落定。
func TestManagerStateReportsProgress(t *testing.T) {
	categories := []string{"都市", "爱情", "科幻"}
	h := newHarness(t, nil, categories)
	h.ff.writeDelay = 200 * time.Millisecond // 让建库阶段的窗口足够长，能稳定读到中间态

	if err := h.manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mid := h.waitForState("建库阶段的进度", func(st State) bool {
		return st.Phase == PhaseCreate && st.Processed > 0
	})
	if !mid.Running {
		t.Error("建库过程中 Running 应为 true")
	}
	if mid.StartedAt.IsZero() {
		t.Error("运行中 StartedAt 应非零（页面靠它算已用时间）")
	}
	if mid.Total != len(categories) {
		t.Errorf("进度 Total = %d, 期望 %d（本轮要建的库数）", mid.Total, len(categories))
	}
	if mid.Processed < 1 || mid.Processed > len(categories) {
		t.Errorf("进度 Processed = %d, 期望落在 1..%d", mid.Processed, len(categories))
	}
	h.manager.Wait()

	done := h.state()
	if done.Running || done.Phase != "" {
		t.Errorf("收尾后 Running=%v / Phase=%q, 期望 false / 空", done.Running, done.Phase)
	}
	if done.Created != len(categories) || done.Unchanged != 0 {
		t.Errorf("收尾后 created=%d / unchanged=%d, 期望 %d / 0", done.Created, done.Unchanged, len(categories))
	}
	if done.DurationMS <= 0 {
		t.Errorf("DurationMS = %d, 期望 > 0（本轮耗时）", done.DurationMS)
	}
	if want := done.LastRunAt.Sub(done.StartedAt).Milliseconds(); done.DurationMS != want {
		t.Errorf("DurationMS = %d, 期望等于 LastRunAt - StartedAt = %d", done.DurationMS, want)
	}
}

// TestManagerStateReportsUnchanged 锁定「再点一次也要说清楚这次做了什么」（AC-2）：
// 分类都已有自管库时本轮 created=0、unchanged=总数，状态面能看出这一轮是幂等命中
// 而不是没跑。
func TestManagerStateReportsUnchanged(t *testing.T) {
	root := "/data/video/dramahub"
	h := newHarness(t, []Library{
		{Name: "短剧 - 都市", Locations: []string{root + "/都市"}},
		{Name: "短剧 - 爱情", Locations: []string{root + "/爱情"}},
	}, []string{"都市", "爱情"})
	h.run()

	state := h.state()
	if state.Created != 0 {
		t.Errorf("created = %d, 期望 0", state.Created)
	}
	if state.Unchanged != 2 {
		t.Errorf("unchanged = %d, 期望 2（两个分类都已建好、这一轮什么都没动）", state.Unchanged)
	}
	if state.Removed != 0 {
		t.Errorf("removed = %d, 期望 0（没有关闭的分类）", state.Removed)
	}
	if state.LastOKAt.IsZero() {
		t.Error("幂等命中不算失败：last_ok_at 应前进")
	}
}

// TestManagerProgressClearedOnFailure 锁定失败收尾也不留半截进度（AC-3）：
// 失败后页面读到的是「没在跑 + 一条 last_error」，不是一条永远停在某个阶段的进度。
func TestManagerProgressClearedOnFailure(t *testing.T) {
	h := newHarness(t, nil, []string{"都市"})
	h.ff.failList = true
	h.ff.status = http.StatusInternalServerError
	h.run()

	state := h.state()
	if state.Running {
		t.Error("失败收尾后 Running 应为 false")
	}
	if state.Phase != "" {
		t.Errorf("Phase = %q, 期望空（收尾后没有「正在做什么」）", state.Phase)
	}
	if state.LastError == "" {
		t.Error("失败必须留下 last_error，页面靠它显示原因")
	}
}

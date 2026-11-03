package webdav

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/vfs"
)

// 本文件是 MKACG-651 AC-4/AC-5 的实测：去掉分片层后，一个分类目录的 PROPFIND
// 到底花多少墙钟、多少响应体、多少内存。
//
// 数字要进 docs/DIRECTORY-CONTRACT.md「单目录子项上限」，所以这里不做任何 mock：
// 真 PostgreSQL + 真 vfs.Tree + 真 handler（含请求内快照），只是把响应体写进一个
// 只数字节的 ResponseWriter。
//
// 默认跳过（`DRAMA_HUB_BENCH=1` 才跑）：它不是回归门禁，是一次性实测。跑法：
//
//	DRAMA_HUB_BENCH=1 DRAMA_HUB_BENCH_MAX=100000 DRAMA_HUB_BENCH_GENRES=68 \
//	  go test ./internal/webdav/ -run TestPropfindDirectoryScale -v
//
// 每个 N 必须单独起一个测试进程再读 VmHWM：同一进程连跑会被前一个大 N 的峰值污染。
func TestPropfindDirectoryScale(t *testing.T) {
	if os.Getenv("DRAMA_HUB_BENCH") != "1" {
		t.Skip("实测用例：设 DRAMA_HUB_BENCH=1 才跑（数字进 docs/DIRECTORY-CONTRACT.md）")
	}
	maxDramas := envInt(t, "DRAMA_HUB_BENCH_MAX", 100000)
	genres := envInt(t, "DRAMA_HUB_BENCH_GENRES", 68)
	// 其余分类各放这么多部，让根目录列举（DISTINCT genre）也是真实规模。
	const perOtherGenre = 20
	total := maxDramas + (genres-1)*perOtherGenre

	ctx := context.Background()
	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	for k, v := range map[store.SettingKey]string{
		store.KeyWebDAVUser:   "bench",
		store.KeyWebDAVPass:   "bench",
		store.KeyStreamSecret: "0123456789abcdef0123456789abcdef",
	} {
		if err := settings.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	// 一个大分类 + 若干小分类，一个事务灌完（要测的是列举，不是写路径）。
	seedStart := time.Now()
	err = st.InTx(ctx, func(tx *store.Store) error {
		for i := 0; i < maxDramas; i++ {
			if _, err := tx.UpsertDrama(ctx, store.Drama{
				Provider: "web", ProviderID: "big-" + strconv.Itoa(i),
				Title: "大分类剧" + strconv.Itoa(i), Year: 2024, Genre: "剧情",
			}); err != nil {
				return err
			}
		}
		for g := 0; g < genres-1; g++ {
			for i := 0; i < perOtherGenre; i++ {
				if _, err := tx.UpsertDrama(ctx, store.Drama{
					Provider: "web", ProviderID: fmt.Sprintf("small-%d-%d", g, i),
					Title: "小分类剧" + strconv.Itoa(i), Year: 2024, Genre: "分类" + strconv.Itoa(g),
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("灌库 %d 部: %v", total, err)
	}
	seedElapsed := time.Since(seedStart)
	if _, err := db.DB.ExecContext(ctx, `ANALYZE drama`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	tree := vfs.NewTree(st, settings, stubID, nil, newDiscardLogger())
	mux := http.NewServeMux()
	mux.Handle(MountPath, NewHandler(tree, settings, newDiscardLogger()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// 1) 纯 DB 列举：耗时、累计分配、堆峰值（去掉 HTTP 与 XML 的开销）。
	//    对照组是「全库全列（ListDramas）」→ 展示窄列（id/provider/provider_id/
	//    title/year/genre）省掉多少：SHOULD 项要求知道 Drama.Plot/Tags 物化占多少峰值。
	fullFn := func() int {
		d, err := st.ListDramas(ctx)
		if err != nil {
			t.Fatalf("ListDramas: %v", err)
		}
		return len(d)
	}
	narrowFn := func() int {
		d, err := st.ListDramaDirsByGenre(ctx, []string{"剧情"})
		if err != nil {
			t.Fatalf("ListDramaDirsByGenre: %v", err)
		}
		return len(d)
	}

	// 窄列对照的顺序可翻转（DRAMA_HUB_BENCH_NARROW_FIRST=1）：确认两次测量的峰值
	// 差异不是「谁先跑谁干净」造成的（measureList 每次先 GC，两次测量的基线一致）。
	narrowFirst := os.Getenv("DRAMA_HUB_BENCH_NARROW_FIRST") == "1"
	var (
		dbElapsed, narrowElapsed                 time.Duration
		dbAlloc, dbPeak, narrowAlloc, narrowPeak uint64
		dbCount, narrowCount                     int
	)
	if narrowFirst {
		narrowElapsed, narrowAlloc, narrowPeak, narrowCount = measureList(t, narrowFn)
		dbElapsed, dbAlloc, dbPeak, dbCount = measureList(t, fullFn)
	} else {
		dbElapsed, dbAlloc, dbPeak, dbCount = measureList(t, fullFn)
		narrowElapsed, narrowAlloc, narrowPeak, narrowCount = measureList(t, narrowFn)
	}
	if narrowCount != maxDramas {
		t.Fatalf("窄列列举到 %d 部, 期望 %d", narrowCount, maxDramas)
	}
	if dbCount != total {
		t.Fatalf("全列列举到 %d 部, 期望 %d（全库）", dbCount, total)
	}

	// 3) 脏标题的 Render 回退路径（去掉分片层后它要扫整个分类，PL 标记的风险项）。
	//    标题含 `/` → 目录名 sanitize 过 → title 点查必然不中 → 退到分类全扫。
	const dirtyCount = 32
	err = st.InTx(ctx, func(tx *store.Store) error {
		for i := 0; i < dirtyCount; i++ {
			if _, err := tx.UpsertDrama(ctx, store.Drama{
				Provider: "web", ProviderID: fmt.Sprintf("dirty-%d", i),
				Title: fmt.Sprintf("脏/标题%d", i), Year: 2025, Genre: "剧情",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("灌脏标题: %v", err)
	}
	renderStart := time.Now()
	body, err := tree.Render(ctx, "短剧/剧情/脏_标题0 (2025)/tvshow.nfo")
	if err != nil {
		t.Fatalf("Render(脏标题): %v", err)
	}
	renderElapsed := time.Since(renderStart)
	if len(body) == 0 {
		t.Fatalf("Render(脏标题) 返回空")
	}

	// 2) 真 handler 的 PROPFIND Depth:1：墙钟 + 响应体字节 + VmHWM 增量。
	//    同一请求连发两次：第一次必然未命中（进程里还没有这份结构），第二次走缓存。
	//    MKACG-669 的验收口径就是「命中 ≤50ms」，所以两段都要有数字。
	propfindURL := srv.URL + "/webdav/" + "剧情"

	hwmBefore := vmHWM()
	coldStatus, coldBytes, coldHead, coldElapsed := runPropfind(ctx, srv.Config.Handler, propfindURL)
	hwmAfter := vmHWM()
	warmStatus, warmBytes, _, warmElapsed := runPropfind(ctx, srv.Config.Handler, propfindURL)

	if coldStatus != http.StatusMultiStatus || warmStatus != http.StatusMultiStatus {
		t.Fatalf("PROPFIND 状态码 = %d / %d, 期望两者都是 207", coldStatus, warmStatus)
	}
	// 响应体必须是合法 multistatus：前 4 KB 里能看到根元素（整份不进内存）。
	if !strings.Contains(string(coldHead), "<D:multistatus") && !strings.Contains(string(coldHead), "<multistatus") {
		t.Fatalf("响应体开头不像 multistatus：%q", coldHead)
	}
	// 命中必须回放同一份字节，而不是重新序列化出「差不多」的结果。
	if coldBytes != warmBytes {
		t.Fatalf("命中响应体 %d 字节 != 未命中 %d 字节", warmBytes, coldBytes)
	}

	// 命中分布的 P50/P99：能进响应缓存（单条 ≤16MB）时连发 50 次取分位；
	// 超过上限的大响应本来就不缓存，这里只报「未命中」与「结构命中」两段。
	p50, p99 := time.Duration(-1), time.Duration(-1)
	if coldBytes <= propfindCacheMaxEntry {
		const samples = 50
		lats := make([]time.Duration, 0, samples)
		for i := 0; i < samples; i++ {
			_, n, _, d := runPropfind(ctx, srv.Config.Handler, propfindURL)
			if n != coldBytes {
				t.Fatalf("第 %d 次命中响应体 %d 字节 != 未命中 %d 字节", i+1, n, coldBytes)
			}
			lats = append(lats, d)
		}
		slices.Sort(lats)
		p50 = lats[len(lats)/2]
		p99 = lats[len(lats)*99/100]
	}

	t.Logf("BENCH max_genre=%-7d genres=%-3d total=%-7d | 灌库 %v | DB 全列 %v (alloc %.0f MB, heap peak %.0f MB) | DB 窄列 %v (alloc %.0f MB, heap peak %.0f MB) | PROPFIND 未命中 %v / 命中 %v (P50 %v, P99 %v) | body %.1f MB | VmHWM %.0f → %.0f MB (Δ%.0f MB) | Render(脏标题) %v",
		maxDramas, genres, total, seedElapsed.Round(time.Millisecond),
		dbElapsed.Round(time.Millisecond), float64(dbAlloc)/1e6, float64(dbPeak)/1e6,
		narrowElapsed.Round(time.Millisecond), float64(narrowAlloc)/1e6, float64(narrowPeak)/1e6,
		coldElapsed.Round(time.Microsecond), warmElapsed.Round(time.Microsecond), p50.Round(time.Microsecond), p99.Round(time.Microsecond),
		float64(coldBytes)/1e6,
		float64(hwmBefore)/1024, float64(hwmAfter)/1024, float64(hwmAfter-hwmBefore)/1024,
		renderElapsed.Round(time.Millisecond))
}

// runPropfind 对真 handler 发一次 PROPFIND Depth:1，返回状态码、body 字节数、
// 前 4 KB 与墙钟。连发两次即可比较未命中与命中。
func runPropfind(ctx context.Context, h http.Handler, url string) (int, int64, []byte, time.Duration) {
	w := &countingWriter{header: http.Header{}}
	req, err := http.NewRequestWithContext(ctx, methodPropfind, url, http.NoBody)
	if err != nil {
		return 0, 0, nil, 0
	}
	req.SetBasicAuth("bench", "bench")
	req.Header.Set("Depth", "1")

	start := time.Now()
	h.ServeHTTP(w, req)
	elapsed := time.Since(start)
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	return status, w.n, w.head, elapsed
}

// measureList 跑一次列表查询，返回耗时、累计分配、堆峰值与行数。
//
// 堆峰值由 1 ms 采样 goroutine 给出：只读 runtime.MemStats 的 HeapAlloc，
// 不参与业务、不改变被测路径；比读 VmHWM 精确（后者含测试自身与 GC 保留的页）。
func measureList(t *testing.T, fn func() int) (elapsed time.Duration, alloc, peak uint64, n int) {
	t.Helper()
	// 先归零：上一次测量的残留堆会让峰值基线抬高，两次测量的峰值就不可比。
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	stop := make(chan struct{})
	var maxHeap uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > maxHeap {
				maxHeap = ms.HeapAlloc
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()

	start := time.Now()
	n = fn()
	elapsed = time.Since(start)
	close(stop)
	<-done

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	return elapsed, after.TotalAlloc - before.TotalAlloc, maxHeap, n
}

// countingWriter 是只数字节的 ResponseWriter。
//
// 不用 httptest.ResponseRecorder：它自己缓存整份响应体（10 万子项约 66 MB），
// 那部分内存会被算到服务端头上，把结论带偏。
type countingWriter struct {
	header http.Header
	status int
	n      int64
	head   []byte // 只留前 4 KB，用来判 XML 是否像样
}

func (w *countingWriter) Header() http.Header { return w.header }

func (w *countingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if room := 4096 - len(w.head); room > 0 {
		w.head = append(w.head, p[:min(room, len(p))]...)
	}
	w.n += int64(len(p))
	return len(p), nil
}

func (w *countingWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

// vmHWM 读进程峰值 RSS（KB）；非 linux 返回 0。
func vmHWM() int64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	body, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(body), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmHWM:"); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")), 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}

// envInt 读一个整数环境变量，缺省或非法时用 def。
func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q 不是正整数", key, raw)
	}
	return n
}

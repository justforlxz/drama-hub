package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/store"
)

// testClock 是可推进的假时钟：artifact.last_used_at 与淘汰都按它算，测试不依赖
// 真实时间（docs/TEST-STANDARDS.md「禁止的测试」）。
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testCache 是一套「真数据库 + 临时 cache_dir + 可注入时钟」的缓存实例。
type testCache struct {
	c        *Cache
	st       *store.Store
	settings *store.Settings
	root     string
	clock    *testClock
}

func newTestCache(t *testing.T) *testCache {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	root := filepath.Join(t.TempDir(), "cache")
	settings := store.NewSettings(db.DB)
	if err := settings.Set(ctx, store.KeyCacheDir, root); err != nil {
		t.Fatalf("设置 cache_dir: %v", err)
	}
	clock := newTestClock()
	st := store.NewStore(db.DB)
	c := New(st, settings, slog.New(slog.DiscardHandler))
	c.now = clock.now
	return &testCache{c: c, st: st, settings: settings, root: root, clock: clock}
}

const (
	testUID = "e1"
	// testChannel 是分片键里的通道段（web 明文源）。
	testChannel = "web"
	// testSegmentBody 冒充一片自包含 fMP4：内容只要非空且可断言即可。
	testSegmentBody = "\x00\x00\x00\x18ftypiso5 fake fmp4 payload"
)

// segSpec 是某一集的缓存位置（键按契约规则算：集 uid + 通道）。
func segSpec(uid string) Spec {
	return Spec{Kind: store.ArtifactKindSegment, Key: SegmentKey(uid, testChannel), Ext: ".m4s"}
}

// genOf 返回一个总是产出 body 的生成函数。
func genOf(body string) GenerateFunc {
	return func(context.Context) ([]byte, error) { return []byte(body), nil }
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// findTree 按 `find <cache_dir>` 的形式列出全部条目（目录带尾部 /），排序后返回。
func findTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if path == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历缓存目录 %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// partialFiles 取 tmp/ 下的半成品文件：流式生成失败/被取消后必须一个不留。
func partialFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, rel := range findTree(t, root) {
		if strings.HasPrefix(rel, dirTmp+"/") && !strings.HasSuffix(rel, "/") {
			out = append(out, rel)
		}
	}
	return out
}

// contentFiles 只取内容目录（cover/seg）下的普通文件，不含 tmp。
func contentFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, rel := range findTree(t, root) {
		if strings.HasSuffix(rel, "/") || strings.HasPrefix(rel, dirTmp+"/") {
			continue
		}
		out = append(out, rel)
	}
	return out
}

// AC：目录布局与键规则与 docs/CACHE-MODEL.md「落盘布局与键」一致
// （seg/<hash 前 2 位>/<hash>.m4s，键 = sha256("e<id>" \x1f channel)）。
func TestLayoutAndKeys(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()

	seg := segSpec(testUID)
	got, err := tc.c.GetOrGenerate(ctx, seg, genOf(testSegmentBody))
	if err != nil {
		t.Fatalf("GetOrGenerate(%s/%s): %v", seg.Kind, seg.Key, err)
	}
	if string(got) != testSegmentBody {
		t.Fatalf("返回字节 = %q, 期望 %q", got, testSegmentBody)
	}

	// 键规则：分片 = sha256("e<id>" \x1f channel)，seq 不进键。
	wantSegHash := sha256Hex(testUID + "\x1f" + testChannel)
	if seg.Key != wantSegHash {
		t.Fatalf("分片键 = %q, 期望 %q", seg.Key, wantSegHash)
	}
	// 同一集换通道 → 键必变（否则两条通道的字节会互相冒充）。
	if SegmentKey(testUID, "app") == seg.Key {
		t.Fatalf("同一集的 app 通道算出了与 web 相同的键")
	}

	row, err := tc.st.ArtifactByKey(ctx, seg.Kind, seg.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey(%s/%s): %v", seg.Kind, seg.Key, err)
	}
	wantPath := filepath.Join(tc.root, dirSegment, wantSegHash[:2], wantSegHash+".m4s")
	if row.Path != wantPath {
		t.Errorf("分片落盘路径 = %q, 期望 %q", row.Path, wantPath)
	}
	if row.Bytes != int64(len(testSegmentBody)) {
		t.Errorf("分片.Bytes = %d, 期望 %d", row.Bytes, len(testSegmentBody))
	}
	if row.SHA256 != sha256Hex(testSegmentBody) {
		t.Errorf("分片.SHA256 = %q, 期望内容哈希", row.SHA256)
	}
	if row.LastUsedAt != stamp(tc.clock.now()) {
		t.Errorf("分片.LastUsedAt = %q, 期望 %q", row.LastUsedAt, stamp(tc.clock.now()))
	}
	onDisk, err := os.ReadFile(row.Path)
	if err != nil {
		t.Fatalf("读缓存文件 %s: %v", row.Path, err)
	}
	if string(onDisk) != testSegmentBody {
		t.Errorf("分片落盘内容 = %q, 期望 %q", onDisk, testSegmentBody)
	}

	// 目录布局：贴出 find 形式的完整清单并逐条断言（一条内容 + tmp/）。
	want := []string{
		dirSegment + "/", filepath.Join(dirSegment, wantSegHash[:2]) + "/",
		filepath.Join(dirSegment, wantSegHash[:2], wantSegHash+".m4s"),
		dirTmp + "/",
	}
	sort.Strings(want)
	gotTree := findTree(t, tc.root)
	for _, line := range gotTree {
		t.Logf("find %s/%s", tc.root, line)
	}
	if strings.Join(gotTree, "\n") != strings.Join(want, "\n") {
		t.Fatalf("目录清单 =\n%s\n期望\n%s", strings.Join(gotTree, "\n"), strings.Join(want, "\n"))
	}
}

// AC：命中不回源（生成次数不变）、命中/未命中字节一致；命中后 last_used_at 前进。
func TestHitDoesNotRegenerateAndTouches(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	spec := segSpec(testUID)

	var gens atomic.Int32
	gen := func(context.Context) ([]byte, error) {
		gens.Add(1)
		return []byte(testSegmentBody), nil
	}

	first, err := tc.c.GetOrGenerate(ctx, spec, gen)
	if err != nil {
		t.Fatalf("首次 GetOrGenerate: %v", err)
	}
	before, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}

	// 一小时后再请求同一片：命中本地文件，不调生成。
	tc.clock.advance(time.Hour)
	second, err := tc.c.GetOrGenerate(ctx, spec, gen)
	if err != nil {
		t.Fatalf("第二次 GetOrGenerate: %v", err)
	}
	if got := gens.Load(); got != 1 {
		t.Fatalf("生成次数 = %d, 期望 1（命中不回源）", got)
	}
	if !bytes.Equal(first, second) || sha256Hex(string(first)) != sha256Hex(string(second)) {
		t.Fatalf("命中与未命中的字节不一致：%q vs %q", first, second)
	}

	after, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
	if err != nil {
		t.Fatalf("第二次 ArtifactByKey: %v", err)
	}
	t.Logf("last_used_at: %q → %q", before.LastUsedAt, after.LastUsedAt)
	if after.LastUsedAt != stamp(tc.clock.now()) {
		t.Fatalf("命中后 last_used_at = %q, 期望 %q（命中即 touch）", after.LastUsedAt, stamp(tc.clock.now()))
	}
	if after.Bytes != before.Bytes || after.Path != before.Path || after.SHA256 != before.SHA256 {
		t.Fatalf("命中改了行里不该变的字段：%+v → %+v", before, after)
	}
}

// 行在、文件没了：按未命中重新生成，并把脏行换成新行（不留下指向不存在文件的字节）。
func TestMissingFileRegenerates(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	spec := segSpec("e3")

	var gens atomic.Int32
	gen := func(context.Context) ([]byte, error) {
		gens.Add(1)
		return []byte(testSegmentBody), nil
	}

	if _, err := tc.c.GetOrGenerate(ctx, spec, gen); err != nil {
		t.Fatalf("首次 GetOrGenerate: %v", err)
	}
	row, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if err := os.Remove(row.Path); err != nil {
		t.Fatalf("删缓存文件: %v", err)
	}

	got, err := tc.c.GetOrGenerate(ctx, spec, gen)
	if err != nil {
		t.Fatalf("文件缺失后 GetOrGenerate: %v", err)
	}
	if string(got) != testSegmentBody {
		t.Fatalf("返回字节 = %q, 期望重新生成的内容", got)
	}
	if n := gens.Load(); n != 2 {
		t.Fatalf("生成次数 = %d, 期望 2（文件缺失要重生成）", n)
	}
	if _, err := os.Stat(row.Path); err != nil {
		t.Fatalf("重生成后文件不在: %v", err)
	}
}

// AC：生成失败 → 不写缓存。错误原样透出（边界包据此映射 502），artifact 无该行、
// 内容目录里没有文件。
//
// 流式生成会在 tmp/ 里建半成品（失败即删）并在内容目录建空分桶目录：失败留下的是
// 空目录，不是可被服务的字节 —— 半成品永远叫 .partial-*、只存在于 tmp/，且不进
// artifact 表，所以重启后绝不会被当成完整产物（MKACG-686 验收第 6 条）。
func TestGenerateFailureWritesNothing(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	spec := segSpec("e2")
	genErr := errors.New("假 CDN 返回 500")

	before := contentFiles(t, tc.root)
	_, err := tc.c.GetOrGenerate(ctx, spec, func(context.Context) ([]byte, error) {
		return nil, genErr
	})
	if !errors.Is(err, genErr) {
		t.Fatalf("错误 = %v, 期望原样返回生成错误（%v）", err, genErr)
	}
	if _, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("失败后 artifact 行 = %v, 期望 ErrNotFound", err)
	}
	if files := contentFiles(t, tc.root); len(files) != len(before) {
		t.Fatalf("失败后内容目录多了文件：%v → %v", before, files)
	}
	if partials := partialFiles(t, tc.root); len(partials) != 0 {
		t.Fatalf("失败后 tmp/ 里还留着半成品：%v", partials)
	}
}

// AC：10 个并发请求同一分片 → 生成次数 = 1；10 份返回字节一致。
func TestSingleFlightTenConcurrent(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	spec := segSpec(testUID)

	var gens atomic.Int32
	gen := func(context.Context) ([]byte, error) {
		gens.Add(1)
		time.Sleep(20 * time.Millisecond) // 让 10 个请求都落进同一个 flight
		return []byte(testSegmentBody), nil
	}

	const n = 10
	start := make(chan struct{})
	bodies := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			bodies[i], errs[i] = tc.c.GetOrGenerate(ctx, spec, gen)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个并发请求: %v", i, errs[i])
		}
		if string(bodies[i]) != testSegmentBody {
			t.Fatalf("第 %d 个并发请求的字节 = %q", i, bodies[i])
		}
	}
	t.Logf("并发 %d 次请求，生成次数 = %d", n, gens.Load())
	if got := gens.Load(); got != 1 {
		t.Fatalf("生成次数 = %d, 期望 1（single-flight）", got)
	}
	if files := contentFiles(t, tc.root); len(files) != 1 {
		t.Fatalf("内容目录文件数 = %d, 期望 1：%v", len(files), files)
	}
}

// 回归（MKACG-640）：结果先落 artifact 行、再从在途表摘除，且 GetOrStream 先查缓存
// 再查在途表 —— 因此「查不到在途、又命中不了缓存」的窗口不存在，晚到者不会成为第二个
// 领跑者。这里用并发压这个窗口：生成次数恒为 1，结束后在途表为空。
//
// 多轮不同键跑同一场景（MKACG-686 代码审查 blocker）：领跑者「rename 半成品 → 写
// artifact 行 → 摘在途表」之间有一段极薄的时间，单轮只有很小的概率被读者的 open 撞上；
// 重复多轮把「读者打开已 rename 走的半成品」压到必现，修好之前这个测试会直接失败。
func TestSingleFlightLeavesNoWindow(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()

	const rounds = 8
	for round := 0; round < rounds; round++ {
		spec := segSpec(fmt.Sprintf("%s-r%d", testUID, round))

		var gens atomic.Int32
		gate := make(chan struct{})
		gen := func(context.Context) ([]byte, error) {
			gens.Add(1)
			<-gate
			return []byte(testSegmentBody), nil
		}

		const n = 8
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				body, err := tc.c.GetOrGenerate(ctx, spec, gen)
				if err != nil {
					errs <- err
					return
				}
				if string(body) != testSegmentBody {
					errs <- errors.New("字节不一致")
				}
			}()
		}
		// 让所有读者都挂到同一个在途生成上，再放行。
		for i := 0; i < 100 && len(tc.buildsSnapshot()) == 0; i++ {
			time.Sleep(5 * time.Millisecond)
		}
		close(gate)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("第 %d 轮并发取同一份内容: %v", round, err)
		}
		if got := gens.Load(); got != 1 {
			t.Fatalf("第 %d 轮生成次数 = %d, 期望 1（single-flight）", round, got)
		}
		if left := tc.buildsSnapshot(); len(left) != 0 {
			t.Fatalf("第 %d 轮结束后在途表还剩 %d 条，期望空", round, len(left))
		}
	}
}

// buildsSnapshot 读在途表（加锁），测试用。
func (tc *testCache) buildsSnapshot() []string {
	tc.c.buildMu.Lock()
	defer tc.c.buildMu.Unlock()
	out := make([]string, 0, len(tc.c.builds))
	for id := range tc.c.builds {
		out = append(out, id)
	}
	return out
}

// 不同键互不阻塞：A 的生成等 B 开始，若被串行化就会超时。
func TestDifferentKeysDoNotBlock(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	aSpec, bSpec := segSpec("e1"), segSpec("e2")
	bStarted := make(chan struct{})

	aGen := func(context.Context) ([]byte, error) {
		select {
		case <-bStarted:
			return []byte(testSegmentBody), nil
		case <-time.After(2 * time.Second):
			return nil, errors.New("分片 A 的生成被分片 B 阻塞了")
		}
	}
	bGen := func(context.Context) ([]byte, error) {
		close(bStarted)
		return []byte(testSegmentBody), nil
	}

	var wg sync.WaitGroup
	var aErr, bErr error
	wg.Add(2)
	go func() { defer wg.Done(); _, aErr = tc.c.GetOrGenerate(ctx, aSpec, aGen) }()
	go func() { defer wg.Done(); _, bErr = tc.c.GetOrGenerate(ctx, bSpec, bGen) }()
	wg.Wait()

	if aErr != nil {
		t.Fatalf("分片 A: %v", aErr)
	}
	if bErr != nil {
		t.Fatalf("分片 B: %v", bErr)
	}
}

// AC：m3u8 文本与源整片不落缓存 —— 契约里没有对应的 kind，也没有别的后缀能冒充。
func TestRejectsNonContractContent(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()

	genCalled := false
	gen := func(context.Context) ([]byte, error) {
		genCalled = true
		return []byte("#EXTM3U\n"), nil
	}
	segKey := SegmentKey("e1", testChannel)
	bad := []Spec{
		{Kind: "playlist", Key: "e1", Ext: ".m3u8"},                              // m3u8 文本没有 kind
		{Kind: "source", Key: "e1", Ext: ".mp4"},                                 // 源整片没有 kind
		{Kind: store.ArtifactKindSegment, Key: segKey, Ext: ".m3u8"},             // m3u8 冒充分片
		{Kind: store.ArtifactKindSegment, Key: segKey, Ext: ".mp4"},              // 整片冒充分片
		{Kind: store.ArtifactKindSegment, Key: segKey, Ext: ".ts"},               // 旧的 TS 后缀不再接受
		{Kind: store.ArtifactKindCover, Key: CoverKey("web", "p1"), Ext: ".mp4"}, // 整片冒充海报
		{Kind: store.ArtifactKindSegment, Key: "", Ext: ".m4s"},                  // 空键
		{Kind: store.ArtifactKindSegment, Key: "not-a-hash", Ext: ".m4s"},        // 键不是 sha256
	}
	for _, spec := range bad {
		if _, err := tc.c.GetOrGenerate(ctx, spec, gen); err == nil {
			t.Errorf("非法 Spec %+v 被接受了", spec)
		}
	}
	if genCalled {
		t.Fatal("非法 Spec 仍调用了生成函数")
	}
	if files := findTree(t, tc.root); len(files) != 0 {
		t.Fatalf("非法 Spec 在缓存目录留下了东西：%v", files)
	}
}

// 键规则（纯函数）：分片键只由「集 uid + 通道」决定；海报键是 <provider>/<id>。
func TestKeyRules(t *testing.T) {
	base := SegmentKey(testUID, testChannel)
	if want := sha256Hex(testUID + "\x1f" + testChannel); base != want {
		t.Fatalf("SegmentKey = %q, 期望 sha256(uid|channel) = %q", base, want)
	}
	distinct := map[string]bool{
		base:                          true,
		SegmentKey("e2", testChannel): true, // 换集
		SegmentKey(testUID, "app"):    true, // 换通道
	}
	if len(distinct) != 3 {
		t.Fatalf("三种输入只得到 %d 个不同键，期望 3", len(distinct))
	}
	if CoverKey("web", "7503745441910017049") != "web/7503745441910017049" {
		t.Fatalf("CoverKey = %q", CoverKey("web", "7503745441910017049"))
	}
}

package segment

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/store"
)

// 预热计划：当前块里当前集之后的成员，然后是后面 ahead 个块（MKACG-682）。
func TestSegmentPrefetchPlan(t *testing.T) {
	block := func(ids ...int64) []store.Episode {
		out := make([]store.Episode, len(ids))
		for i, id := range ids {
			out[i] = store.Episode{ID: id, DramaID: 1, EpNo: i + 1}
		}
		return out
	}
	blocks := [][]store.Episode{
		block(1, 2, 3),
		block(4, 5, 6),
		block(7, 8, 9),
	}

	got := func(eps []store.Episode) []int64 {
		out := make([]int64, len(eps))
		for i, ep := range eps {
			out[i] = ep.ID
		}
		return out
	}

	cases := []struct {
		name    string
		current []store.Episode
		ahead   int
		want    []int64
	}{
		{"块首集：块内剩余 + 下一块", blocks[0], 1, []int64{2, 3, 4, 5, 6}},
		{"块中间：块内剩余 + 下一块", blocks[0][1:], 1, []int64{3, 4, 5, 6}},
		{"最后一块：只剩块内剩余", blocks[2][:1], 1, []int64{8, 9}},
		{"提前量 0 = 关闭", blocks[0], 0, nil},
		{"提前量 2：再往后一块", blocks[0], 2, []int64{2, 3, 4, 5, 6, 7, 8, 9}},
		{"末块末集：没有可预热", blocks[2][2:], 1, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotIDs := got(segmentPrefetchPlan(blocks, tc.current, tc.ahead))
			if len(gotIDs) != len(tc.want) {
				t.Fatalf("计划 = %v, 期望 %v", gotIDs, tc.want)
			}
			for i := range tc.want {
				if gotIDs[i] != tc.want[i] {
					t.Fatalf("计划 = %v, 期望 %v", gotIDs, tc.want)
				}
			}
		})
	}
}

// 播放列表成功后，后台把块内剩余成员生成成分片并标 prefetched；前台一播就清标记。
func TestSegmentPrefetchWarmsRemainingBlock(t *testing.T) {
	r := newPrefetchRig(t, 3)
	// 预热触发点钉在播放列表（MKACG-687 的默认值已改成首个分片）：
	// 这个用例验的是「playlist 之后的预热内容」，不是触发时机。
	setPrefetchOnPlaylist(t, r)
	dramaID, eps := r.addDrama(t, "d1", 1, 2, 3)
	if len(eps) != 3 {
		t.Fatalf("成员数 = %d, 期望 3", len(eps))
	}
	uid := "m" + strconv.FormatInt(dramaID, 10) + "_1_3"

	target := "/video/" + r.id(uid) + ".m3u8"
	if rec := r.do(t, http.MethodGet, target); rec.Code != http.StatusOK {
		t.Fatalf("playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}

	// 块内剩余的第 2、3 集都会被预热（顺序执行，等两台都落地）。
	for _, ep := range eps[1:] {
		key := cache.SegmentKey(episodeUID(ep.ID), "web")
		waitFor(t, 20*time.Second, "等待分片预热落地", func() bool {
			row, err := r.st.ArtifactByKey(context.Background(), store.ArtifactKindSegment, key)
			return err == nil && row.Prefetched
		})
	}

	// 前台真的播第 2 集：命中缓存，标记清掉。
	seg := "/video/" + r.id(uid) + "/1.m4s"
	if rec := r.do(t, http.MethodGet, seg); rec.Code != http.StatusOK {
		t.Fatalf("分片状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	row, err := r.st.ArtifactByKey(context.Background(), store.ArtifactKindSegment,
		cache.SegmentKey(episodeUID(eps[1].ID), "web"))
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if row.Prefetched {
		t.Fatalf("前台播放过的分片仍带 prefetched 标记")
	}
}

// segment_prefetch_blocks = 0 时整段关闭：播放列表不生成任何分片。
func TestSegmentPrefetchDisabled(t *testing.T) {
	r := newPrefetchRig(t, 1)
	setPrefetchOnPlaylist(t, r)
	if err := r.settings.Set(context.Background(), store.KeySegmentPrefetchBlocks, "0"); err != nil {
		t.Fatalf("设置 segment_prefetch_blocks: %v", err)
	}
	_, eps := r.addDrama(t, "d1", 1, 2)

	target := "/video/" + r.id(episodeUID(eps[0].ID)) + ".m3u8"
	if rec := r.do(t, http.MethodGet, target); rec.Code != http.StatusOK {
		t.Fatalf("playlist 状态码 = %d, 期望 200", rec.Code)
	}
	// ahead=0 时任务在起跑前就返回，不会产生任何预热日志或分片。
	for _, m := range r.logs.records() {
		if m["msg"] == "segment prefetch run started" {
			t.Fatalf("关闭时仍起了预热任务: %v", m)
		}
	}
	if _, err := r.st.ArtifactByKey(context.Background(), store.ArtifactKindSegment,
		cache.SegmentKey(episodeUID(eps[1].ID), "web")); err == nil {
		t.Fatalf("关闭时仍生成了分片")
	}
}

// MKACG-687 AC-3：默认 prefetch_on=first_segment 时，只拉播放列表（探测）不触发任何
// 预热；首个分片请求之后才起整剧预取与分片预热。
func TestPrefetchWaitsForFirstSegment(t *testing.T) {
	r := newPrefetchRig(t, 3)
	dramaID, eps := r.addDrama(t, "d1", 1, 2, 3)
	uid := "m" + strconv.FormatInt(dramaID, 10) + "_1_3"

	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	for _, m := range r.logs.records() {
		if m["msg"] == "prefetch run started" || m["msg"] == "segment prefetch run started" {
			t.Fatalf("只读播放列表就起了预热: %v", m)
		}
	}
	for _, ep := range eps[1:] {
		if _, err := r.st.ArtifactByKey(context.Background(), store.ArtifactKindSegment,
			cache.SegmentKey(episodeUID(ep.ID), "web")); err == nil {
			t.Fatalf("只读播放列表就生成了分片 %d", ep.ID)
		}
	}

	// 真正播放：首个分片出流之后预热才启动。
	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/0.m4s"); rec.Code != http.StatusOK {
		t.Fatalf("分片状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	r.waitLog(t, "分片预热启动", func(m map[string]any) bool {
		return m["msg"] == "segment prefetch run started"
	})
	r.waitLog(t, "整剧预取启动", func(m map[string]any) bool {
		return m["msg"] == "prefetch run started" && logDrama(m, dramaID)
	})
	for _, ep := range eps[1:] {
		key := cache.SegmentKey(episodeUID(ep.ID), "web")
		waitFor(t, 20*time.Second, "等待分片预热落地", func() bool {
			row, err := r.st.ArtifactByKey(context.Background(), store.ArtifactKindSegment, key)
			return err == nil && row.Prefetched
		})
	}
}

// 前台静默（播放结束 / 客户端断开）后不再生成新分片，日志可见取消（MKACG-682 AC）。
func TestSegmentPrefetchStopsWhenIdle(t *testing.T) {
	r := newPrefetchRig(t, 1)
	_, eps := r.addDrama(t, "d1", 1, 2)
	// 模拟「上一次前台请求在很久以前」。
	r.c.lastPlay.Store(time.Now().Add(-time.Hour).UnixNano())

	run := &segmentPrefetchRun{unitID: episodeUID(eps[0].ID), dramaID: r.dramaID, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	go r.c.runSegmentPrefetch(ctx, run, eps)

	select {
	case <-run.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("预热任务没有在空闲判定后退出")
	}
	hit := r.waitLog(t, "取消日志", func(m map[string]any) bool {
		return m["msg"] == "segment prefetch run cancelled" && m["reason"] == "idle"
	})
	if hit == nil {
		t.Fatalf("没有 idle 取消日志")
	}
	if _, err := r.st.ArtifactByKey(context.Background(), store.ArtifactKindSegment,
		cache.SegmentKey(episodeUID(eps[1].ID), "web")); err == nil {
		t.Fatalf("空闲取消后仍生成了分片")
	}
}

// 同一播放单元重复请求播放列表不重启预热任务（单飞）。
func TestSegmentPrefetchSingleFlightPerUnit(t *testing.T) {
	r := newPrefetchRig(t, 1)
	setPrefetchOnPlaylist(t, r)
	_, eps := r.addDrama(t, "d1", 1, 2, 3)
	target := "/video/" + r.id(episodeUID(eps[0].ID)) + ".m3u8"
	for i := 0; i < 3; i++ {
		if rec := r.do(t, http.MethodGet, target); rec.Code != http.StatusOK {
			t.Fatalf("playlist 状态码 = %d, 期望 200", rec.Code)
		}
	}
	r.waitLog(t, "预热任务启动", func(m map[string]any) bool {
		return m["msg"] == "segment prefetch run started"
	})
	starts := 0
	for _, m := range r.logs.records() {
		if m["msg"] == "segment prefetch run started" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("预热启动次数 = %d, 期望 1（同单元单飞）", starts)
	}
}

// 前台要槽位时，在跑的后台预热被取消并让出唯一槽位（MKACG-682 AC：预热不拖慢前台）。
func TestGateForegroundPreemptsBackground(t *testing.T) {
	g := newFFmpegGate(1)
	bg, release, ok := g.acquireBackground(context.Background())
	if !ok {
		t.Fatalf("后台没拿到空槽位")
	}
	got := make(chan error, 1)
	go func() { got <- g.acquire(context.Background()) }()

	select {
	case <-bg.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("前台取槽位时没有取消后台预热")
	}
	release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("前台取槽位: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("前台没拿到槽位")
	}
	g.release()
}

// 播放列表带 ETag / Cache-Control；内容不变时条件请求得到 304，不同播放单元 ETag 不同。
func TestPlaylistETagAndConditionalRequest(t *testing.T) {
	r := newRig(t)
	ep := r.addEpisode(t, 1, 1)
	target := "/video/" + r.id(episodeUID(ep.ID)) + ".m3u8"

	first := r.do(t, http.MethodGet, target)
	if first.Code != http.StatusOK {
		t.Fatalf("playlist 状态码 = %d, 期望 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("playlist 响应缺少 ETag")
	}
	if cc := first.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, 期望 no-cache", cc)
	}

	second := r.do(t, http.MethodGet, target)
	if second.Header().Get("ETag") != etag {
		t.Fatalf("同一播放单元的 ETag 不稳定: %q vs %q", etag, second.Header().Get("ETag"))
	}

	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	r.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("条件请求状态码 = %d, 期望 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 响应带 body: %q", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("If-None-Match", `"stale"`)
	rec = httptest.NewRecorder()
	r.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ETag 不匹配时状态码 = %d, 期望 200", rec.Code)
	}

	// 另一个播放单元（内容不同）→ ETag 必须不同。
	other := r.addEpisode(t, 2, 1)
	otherTarget := "/video/" + r.id(episodeUID(other.ID)) + ".m3u8"
	if got := r.do(t, http.MethodGet, otherTarget).Header().Get("ETag"); got == etag {
		t.Fatalf("不同播放单元的 ETag 相同: %q", got)
	}
}

// 纯函数：If-None-Match 的解析（多个标签、弱标签、*）。
func TestETagMatches(t *testing.T) {
	const etag = `"abc123"`
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{etag, true},
		{`"other", ` + etag, true},
		{`W/` + etag, true},
		{"*", true},
		{`"other"`, false},
	}
	for _, tc := range cases {
		if got := etagMatches(tc.header, etag); got != tc.want {
			t.Fatalf("etagMatches(%q) = %v, 期望 %v", tc.header, got, tc.want)
		}
	}
}

// 取消真实 remux 的领跑者时，前台等同一个键的请求必须重新领跑并成功，
// 不能拿到上游错误（会被映成 502）——MKACG-682 代码审查 blocker 的回归钉。
func TestSegmentPrefetchCancelDoesNotFailForeground(t *testing.T) {
	r := newPrefetchRig(t, 1)
	_, eps := r.addDrama(t, "d1", 1, 2)
	ep := eps[1]
	key := cache.SegmentKey(episodeUID(ep.ID), "web")
	spec := cache.Spec{Kind: store.ArtifactKindSegment, Key: key, Ext: segmentSuffix}

	r.cdn.setHang()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prefetchErr := make(chan error, 1)
	go func() { prefetchErr <- r.c.prefetchSegment(ctx, ep) }()
	// 等到预热真的开始拉媒体（ffmpeg 已经在跑，说明它是这个键的 flight 领跑者）。
	waitFor(t, 15*time.Second, "预热开始拉取媒体", func() bool { return r.cdn.hits() > 0 })

	// 先把直链取好（挂起只影响媒体请求），再让之后的新请求恢复正常：
	// 领跑者那条挂起的请求仍会等自己的 ctx 结束。
	direct, err := r.c.resolve(context.Background(), ep)
	if err != nil {
		t.Fatalf("解析直链: %v", err)
	}
	r.cdn.releaseHang()

	type result struct {
		body []byte
		err  error
	}
	fg := make(chan result, 1)
	go func() {
		body, err := r.c.caches.GetOrGenerate(context.Background(), spec,
			func(ctx context.Context) ([]byte, error) {
				return remux(ctx, discardLogger, direct, "")
			})
		fg <- result{body: body, err: err}
	}()
	// 给前台留出加入同一个 flight 的时间；即使它先成了领跑者，本用例也必须成功。
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case got := <-fg:
		if got.err != nil {
			t.Fatalf("前台请求失败（会被映射成 502）: %v", got.err)
		}
		if len(got.body) == 0 {
			t.Fatalf("前台拿到空分片")
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("前台请求没有返回")
	}
	if err := <-prefetchErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("预热取消错误 = %v, 期望 context.Canceled", err)
	}
}

// resolve 阶段被取消时同样不能把前台映射成 502（第二轮代码审查 blocker）。
// 与上一条的区别：segment 行已存在（playlist 会建），直链仍在闭包里现解析。
func TestSegmentPrefetchCancelAtResolveKeepsForeground(t *testing.T) {
	r := newPrefetchRig(t, 1)
	_, eps := r.addDrama(t, "d1", 1, 2)
	ep := eps[1]
	ctx0 := context.Background()
	if err := r.st.UpsertSegment(ctx0, store.Segment{EpisodeID: ep.ID, Channel: "web"}); err != nil {
		t.Fatalf("UpsertSegment: %v", err)
	}
	spec := cache.Spec{
		Kind: store.ArtifactKindSegment,
		Key:  cache.SegmentKey(episodeUID(ep.ID), "web"),
		Ext:  segmentSuffix,
	}
	gate := r.src.setGate("d1/2", false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prefetchErr := make(chan error, 1)
	go func() { prefetchErr <- r.c.prefetchSegment(ctx, ep) }()
	waitFor(t, 15*time.Second, "预热进入 resolve", func() bool { return r.src.count("d1/2") >= 1 })

	type result struct {
		body []byte
		err  error
	}
	fg := make(chan result, 1)
	go func() {
		body, err := r.c.caches.GetOrGenerate(ctx0, spec,
			func(ctx context.Context) ([]byte, error) {
				direct, err := r.c.resolve(ctx, ep)
				if err != nil {
					return nil, err
				}
				return remux(ctx, discardLogger, direct, "")
			})
		fg <- result{body: body, err: err}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	close(gate) // 解除上游卡住：前台重试领跑时要能解析成功

	select {
	case got := <-fg:
		if got.err != nil {
			t.Fatalf("前台请求失败（会被映射成 502）: %v", got.err)
		}
		if len(got.body) == 0 {
			t.Fatalf("前台拿到空分片")
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("前台请求没有返回")
	}
	if err := <-prefetchErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("预热取消错误 = %v, 期望 context.Canceled", err)
	}
}

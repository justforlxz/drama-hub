package segment

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/store"
)

// 首片耗时基准（MKACG-682 AC-1 / AC-2 / AC-3）。
//
// 只有在 DRAMA_HUB_BENCH=1 时运行（不进 CI 的常规门禁，参考
// internal/webdav/bench_propfind_test.go）。同一个构建里用
// `segment_prefetch_blocks=0` 复现「改造前」（没有分片预热）的路径，
// 默认值复现「改造后」——口径写进 docs/adr/0018 与 PR。
//
//	DRAMA_HUB_BENCH=1 go test ./internal/segment -run TestBenchSegmentPrefetch -v
func TestBenchSegmentPrefetch(t *testing.T) {
	if os.Getenv("DRAMA_HUB_BENCH") != "1" {
		t.Skip("设置 DRAMA_HUB_BENCH=1 运行首片耗时基准")
	}
	const rounds = 12
	type result struct {
		cold, jump []time.Duration
	}

	run := func(ahead string) result {
		var out result
		for i := 0; i < rounds; i++ {
			r := newPrefetchRig(t, 3) // 一块 3 集
			if ahead != "" {
				if err := r.settings.Set(context.Background(), store.KeySegmentPrefetchBlocks, ahead); err != nil {
					t.Fatalf("设置 segment_prefetch_blocks=%s: %v", ahead, err)
				}
			}
			dramaID, eps := r.addDrama(t, fmt.Sprintf("bench%d", i), 1, 2, 3, 4, 5, 6)
			if len(eps) != 6 {
				t.Fatalf("成员数 = %d, 期望 6", len(eps))
			}
			uid := "m" + strconv.FormatInt(dramaID, 10) + "_1_3"
			if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusOK {
				t.Fatalf("playlist 状态码 = %d", rec.Code)
			}

			start := time.Now()
			if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/0.m4s"); rec.Code != http.StatusOK {
				t.Fatalf("冷启动首片状态码 = %d", rec.Code)
			}
			out.cold = append(out.cold, time.Since(start))

			// 「改造后」等预热把第 2 集备好再跳（模拟用户点下一集所需的时间）；
			// 「改造前」没有预热，直接请求，就是冷启动转封装。
			if ahead != "0" {
				key := cache.SegmentKey(episodeUID(eps[1].ID), "web")
				waitFor(t, 30*time.Second, "等待第 2 集预热完成", func() bool {
					row, err := r.st.ArtifactByKey(context.Background(), store.ArtifactKindSegment, key)
					return err == nil && row.Prefetched
				})
			}
			start = time.Now()
			if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/1.m4s"); rec.Code != http.StatusOK {
				t.Fatalf("跳转首片状态码 = %d", rec.Code)
			}
			out.jump = append(out.jump, time.Since(start))
		}
		return out
	}

	before := run("0")
	after := run("")

	report := func(name string, d []time.Duration) {
		sorted := append([]time.Duration(nil), d...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		p := func(q float64) time.Duration {
			idx := int(float64(len(sorted)-1) * q)
			return sorted[idx]
		}
		t.Logf("%s n=%d P50=%v P95=%v", name, len(sorted), p(0.50), p(0.95))
	}
	t.Logf("首片耗时（同机同网、1 秒 fixture、每轮新建库与缓存目录，n=%d）", rounds)
	report("冷启动首片 · 改造前(segment_prefetch_blocks=0)", before.cold)
	report("冷启动首片 · 改造后(默认 1)", after.cold)
	report("拖到下一集首片 · 改造前", before.jump)
	report("拖到下一集首片 · 改造后", after.jump)
}

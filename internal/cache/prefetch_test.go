package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

// 预热落盘的分片带 prefetched 标记；前台一播到它，标记清掉（MKACG-682）。
func TestPrefetchMarksUntilPlayed(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	spec := segSpec("e7")

	calls := 0
	gen := func(_ context.Context, w io.Writer) error {
		calls++
		_, err := w.Write([]byte(testSegmentBody))
		return err
	}
	if err := tc.c.Prefetch(ctx, spec, gen); err != nil {
		t.Fatalf("Prefetch: %v", err)
	}
	row, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if !row.Prefetched {
		t.Fatalf("预热生成的分片 prefetched = false, 期望 true")
	}

	// 已有本地字节就不再生成，也不改标记。
	if err := tc.c.Prefetch(ctx, spec, gen); err != nil {
		t.Fatalf("第二次 Prefetch: %v", err)
	}
	if calls != 1 {
		t.Fatalf("生成次数 = %d, 期望 1（已有字节不该重做）", calls)
	}

	// 前台命中：读回同一份字节，并清掉预热标记。
	body, err := tc.c.GetOrGenerate(ctx, spec, func(context.Context) ([]byte, error) {
		t.Fatalf("前台命中不该再生成")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if string(body) != testSegmentBody {
		t.Fatalf("字节不一致: %q", body)
	}
	played, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if played.Prefetched {
		t.Fatalf("前台播放过的分片仍带 prefetched 标记")
	}
}

// 容量吃紧时先淘汰「预热但没播过」的分片，哪怕它是最新的（MKACG-682 AC）。
func TestEvictPrefersPrefetchedOverPlayed(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	if err := tc.settings.Set(ctx, store.KeyCacheMaxMB, "1"); err != nil {
		t.Fatalf("设置 cache_max_mb: %v", err)
	}

	const chunk = 400 << 10
	// 两条播过的分片（较早），一条预热的（最新）。
	var played []Spec
	for seq := 0; seq < 2; seq++ {
		spec := segSpec("e" + string(rune('1'+seq)))
		if _, err := tc.c.GetOrGenerate(ctx, spec, func(context.Context) ([]byte, error) {
			return blob(chunk, byte('a'+seq)), nil
		}); err != nil {
			t.Fatalf("写播过的分片: %v", err)
		}
		played = append(played, spec)
		tc.clock.advance(time.Minute)
	}
	prefetched := segSpec("e9")
	if err := tc.c.Prefetch(ctx, prefetched, func(_ context.Context, w io.Writer) error {
		_, err := w.Write(blob(chunk, 'z'))
		return err
	}); err != nil {
		t.Fatalf("预热水片: %v", err)
	}

	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	// 预热的那条（最新）先走：LRU 会删最早的两条之一，优先级淘汰不会。
	if _, err := tc.st.ArtifactByKey(ctx, prefetched.Kind, prefetched.Key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("预热分片行 = %v, 期望被优先淘汰（ErrNotFound）", err)
	}
	for _, spec := range played {
		row, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
		if err != nil {
			t.Fatalf("播过的分片被误删: %v", err)
		}
		if _, err := os.Stat(row.Path); err != nil {
			t.Fatalf("播过的分片文件被误删: %v", err)
		}
	}
}

// 领跑者被取消（后台预热让路）时，等待者重新领跑，而不是把取消当成内容失败。
func TestFlightWaiterRetriesCancelledLeader(t *testing.T) {
	g := newFlightGroup()
	f := &flight{done: make(chan struct{})}
	f.err = context.Canceled
	close(f.done)
	g.flights["k"] = f

	body, err := g.do(context.Background(), "k", func() ([]byte, error) {
		return []byte("fresh"), nil
	}, nil)
	if err != nil {
		t.Fatalf("等待者拿到取消错误: %v", err)
	}
	if string(body) != "fresh" {
		t.Fatalf("等待者拿到的字节 = %q, 期望重新生成的结果", body)
	}

	// 非取消类错误仍原样传播（真实生成失败不该被重试掩盖）。
	boom := errors.New("ffmpeg failed")
	f2 := &flight{done: make(chan struct{})}
	f2.err = boom
	close(f2.done)
	g.flights["k2"] = f2
	if _, err := g.do(context.Background(), "k2", func() ([]byte, error) {
		t.Fatalf("非取消错误不该重新生成")
		return nil, nil
	}, nil); !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, 期望原样返回 %v", err, boom)
	}
}

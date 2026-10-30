package cache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

// blob 生成 n 字节的可断言内容：首字节标记来源，其余填同一个值。
func blob(n int, marker byte) []byte {
	body := bytes.Repeat([]byte{marker}, n)
	return body
}

// AC：cache_max_mb 调到 1 MB 造超限 → 淘汰后容量落回上限内，且被淘汰的是
// last_used_at 最旧的那些（docs/CACHE-MODEL.md「测试要求」第 4 条）。
func TestEvictBySizeLimit(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	if err := tc.settings.Set(ctx, store.KeyCacheMaxMB, "1"); err != nil {
		t.Fatalf("设置 cache_max_mb: %v", err)
	}

	// 三个 400 KB 的分片（共 1.2 MB > 1 MB），写入时间依次前进。
	const chunk = 400 << 10
	specs := make([]Spec, 0, 3)
	for seq := 0; seq < 3; seq++ {
		spec := segSpec(testUID, seq)
		if _, err := tc.c.GetOrGenerate(ctx, spec, func(context.Context) ([]byte, error) {
			return blob(chunk, byte('a'+seq)), nil
		}); err != nil {
			t.Fatalf("写第 %d 个分片: %v", seq, err)
		}
		specs = append(specs, spec)
		tc.clock.advance(time.Minute)
	}

	before, err := tc.st.SumArtifactBytes(ctx, store.ArtifactKindSegment)
	if err != nil {
		t.Fatalf("SumArtifactBytes: %v", err)
	}
	if before != 3*chunk {
		t.Fatalf("淘汰前总量 = %d, 期望 %d", before, 3*chunk)
	}

	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	after, err := tc.st.SumArtifactBytes(ctx, store.ArtifactKindSegment)
	if err != nil {
		t.Fatalf("SumArtifactBytes: %v", err)
	}
	t.Logf("淘汰前 %d 字节 → 淘汰后 %d 字节（上限 %d）", before, after, 1<<20)
	if after > 1<<20 {
		t.Fatalf("淘汰后总量 = %d, 仍超过上限 1 MB", after)
	}

	// 最久未用的 seq 0 先走：行与文件都不在。
	if _, err := tc.st.ArtifactByKey(ctx, specs[0].Kind, specs[0].Key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("最久未用的分片行 = %v, 期望 ErrNotFound", err)
	}
	for _, spec := range specs[1:] {
		row, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
		if err != nil {
			t.Fatalf("较新的分片行被误删: %v", err)
		}
		if _, err := os.Stat(row.Path); err != nil {
			t.Fatalf("较新的分片文件被误删: %v", err)
		}
	}
	if files := contentFiles(t, tc.root); len(files) != 2 {
		t.Fatalf("淘汰后内容文件数 = %d, 期望 2：%v", len(files), files)
	}

	// 再跑一轮：已在上限内，不该再删东西。
	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("第二轮 Evict: %v", err)
	}
	if files := contentFiles(t, tc.root); len(files) != 2 {
		t.Fatalf("第二轮淘汰删多了：%v", files)
	}
}

// cache_max_mb = 0 表示不按大小淘汰：只受 cache_lru_days 约束。
func TestEvictSizeDisabled(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	if err := tc.settings.Set(ctx, store.KeyCacheMaxMB, "0"); err != nil {
		t.Fatalf("设置 cache_max_mb: %v", err)
	}
	const chunk = 2 << 20 // 2 MB，已远超「0 MB 上限」的字面意思
	spec := segSpec(testUID, 0)
	if _, err := tc.c.GetOrGenerate(ctx, spec, func(context.Context) ([]byte, error) {
		return blob(chunk, 'z'), nil
	}); err != nil {
		t.Fatalf("写分片: %v", err)
	}
	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key); err != nil {
		t.Fatalf("cache_max_mb=0 仍做了大小淘汰: %v", err)
	}
}

// AC：cache_lru_days 过期 —— last_used_at 改到 40 天前的条目，淘汰后行与文件都不在，
// 没到期的条目原样保留（docs/CACHE-MODEL.md「测试要求」第 5 条）。
func TestEvictExpired(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	now := tc.clock.now()

	// 40 天前写下的三种 kind 各一条。
	oldTime := now.AddDate(0, 0, -40)
	tc.clock.set(oldTime)
	oldSpecs := []Spec{
		segSpec(testUID, 0),
		{Kind: store.ArtifactKindIndex, Key: IndexKey("e1"), Ext: ".json"},
		{Kind: store.ArtifactKindCover, Key: CoverKey("web", "p1"), Ext: ".jpg"},
	}
	for _, spec := range oldSpecs {
		if _, err := tc.c.GetOrGenerate(ctx, spec, genOf("old")); err != nil {
			t.Fatalf("写 %s: %v", spec.Kind, err)
		}
	}
	// 10 天前写下的一条分片：不该被删。
	tc.clock.set(now.AddDate(0, 0, -10))
	fresh := segSpec(testUID, 1)
	if _, err := tc.c.GetOrGenerate(ctx, fresh, genOf("fresh")); err != nil {
		t.Fatalf("写新分片: %v", err)
	}
	freshRow, err := tc.st.ArtifactByKey(ctx, fresh.Kind, fresh.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}

	tc.clock.set(now)
	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	for _, spec := range oldSpecs {
		if _, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s 过期后行 = %v, 期望 ErrNotFound", spec.Kind, err)
		}
	}
	if _, err := os.Stat(freshRow.Path); err != nil {
		t.Fatalf("未过期的分片文件被删了: %v", err)
	}
	if _, err := tc.st.ArtifactByKey(ctx, fresh.Kind, fresh.Key); err != nil {
		t.Fatalf("未过期的分片行被删了: %v", err)
	}
	if files := contentFiles(t, tc.root); len(files) != 1 {
		t.Fatalf("过期淘汰后内容文件数 = %d, 期望 1：%v", len(files), files)
	}
}

// RunEviction 启动即跑一轮，ctx 取消后退出（不留 goroutine）。
func TestRunEvictionRunsThenStops(t *testing.T) {
	tc := newTestCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 先造一条 40 天前写入的分片，循环的第一轮就该清掉它。
	tc.clock.set(tc.clock.now().AddDate(0, 0, -40))
	spec := segSpec(testUID, 0)
	if _, err := tc.c.GetOrGenerate(context.Background(), spec, genOf(testSegmentBody)); err != nil {
		t.Fatalf("写分片: %v", err)
	}
	tc.clock.set(tc.clock.now().AddDate(0, 0, 40)) // 回到「现在」

	done := make(chan struct{})
	go func() {
		defer close(done)
		tc.c.RunEviction(ctx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := tc.st.ArtifactByKey(context.Background(), spec.Kind, spec.Key)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("第一轮淘汰没有清掉过期条目（最后错误：%v）", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunEviction 在 ctx 取消后没有退出")
	}
}

// 淘汰先删文件再删行；文件已经不在时算成功（自愈），行也一并清掉。
func TestEvictSelfHealsMissingFile(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	spec := segSpec(testUID, 0)
	if _, err := tc.c.GetOrGenerate(ctx, spec, genOf(testSegmentBody)); err != nil {
		t.Fatalf("写分片: %v", err)
	}
	row, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if err := os.Remove(row.Path); err != nil {
		t.Fatalf("删文件: %v", err)
	}

	tc.clock.advance(40 * 24 * time.Hour)
	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, err := tc.st.ArtifactByKey(ctx, spec.Kind, spec.Key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("文件缺失的过期行 = %v, 期望 ErrNotFound", err)
	}
}

// tmp 里的临时文件超过 tmpMaxAge 由淘汰清扫（写盘被 kill 打断留下的半截文件）。
func TestEvictSweepsStaleTmp(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	tmpDir := filepath.Join(tc.root, dirTmp)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatalf("建 tmp: %v", err)
	}
	now := tc.clock.now()
	stale := filepath.Join(tmpDir, ".tmp-stale")
	if err := os.WriteFile(stale, []byte("half written"), 0o644); err != nil {
		t.Fatalf("写半截文件: %v", err)
	}
	// mtime 显式钉住，不依赖真实时间：stale 比注入时钟早 2 小时（超过 tmpMaxAge），
	// fresh 就是「现在」。
	if err := os.Chtimes(stale, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("改半截文件的 mtime: %v", err)
	}
	fresh := filepath.Join(tmpDir, ".tmp-fresh")
	if err := os.WriteFile(fresh, []byte("in flight"), 0o644); err != nil {
		t.Fatalf("写临时文件: %v", err)
	}
	if err := os.Chtimes(fresh, now, now); err != nil {
		t.Fatalf("改临时文件的 mtime: %v", err)
	}

	if err := tc.c.Evict(ctx); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("过期临时文件没被清扫: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("仍在写入的临时文件被误删: %v", err)
	}
}

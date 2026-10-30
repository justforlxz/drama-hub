package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

const (
	// evictInterval 是淘汰循环的间隔：5 分钟，固定值不做成配置项
	// （docs/CACHE-MODEL.md「淘汰」）。与请求路径无关。
	evictInterval = 5 * time.Minute
	// evictBatch 是一轮淘汰里一次取的行数：取一批删一批，避免长事务。
	evictBatch = 100
	// tmpMaxAge 是 tmp 里临时文件的保留上限：进程被 kill 打断留下的半截文件超过
	// 这个时间就删掉。
	tmpMaxAge = time.Hour
)

// RunEviction 阻塞跑淘汰循环：先跑一轮（补上次退出前欠下的），之后每 evictInterval
// 一轮，ctx 结束后返回。调用方是装配根：`go caches.RunEviction(ctx)`。
func (c *Cache) RunEviction(ctx context.Context) {
	c.runEvictionOnce(ctx)
	ticker := time.NewTicker(evictInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runEvictionOnce(ctx)
		}
	}
}

// runEvictionOnce 跑一轮并记录失败：淘汰失败不该让服务退出，下一轮重试即可。
func (c *Cache) runEvictionOnce(ctx context.Context) {
	if err := c.Evict(ctx); err != nil && ctx.Err() == nil {
		c.logger.WarnContext(ctx, "缓存淘汰失败", "err", err)
	}
}

// Evict 跑一轮淘汰：清扫 tmp 遗留 → 过期（cache_lru_days）→ 超限（cache_max_mb）。
//
// 两项配置每轮现读（硬约束 5）。cache_max_mb = 0 表示只按天数过期、不按大小淘汰。
func (c *Cache) Evict(ctx context.Context) error {
	root, err := cacheRoot(ctx, c.settings)
	if err != nil {
		return err
	}
	now := c.now()
	if _, err := sweepTmp(layoutOf(root).tmpDir(), now); err != nil {
		return err
	}

	days, err := c.settings.Int(ctx, store.KeyCacheLRUDays)
	if err != nil {
		return err
	}
	if err := c.evictExpired(ctx, now.AddDate(0, 0, -days)); err != nil {
		return err
	}

	maxMB, err := c.settings.Int(ctx, store.KeyCacheMaxMB)
	if err != nil {
		return err
	}
	if maxMB == 0 {
		return nil
	}
	return c.evictOverLimit(ctx, int64(maxMB)<<20)
}

// evictExpired 删掉 last_used_at 早于 before 的条目，三种 kind 都算：过期是「多久
// 没命中」，与类型无关（docs/CACHE-MODEL.md「淘汰」第 1 步）。
func (c *Cache) evictExpired(ctx context.Context, before time.Time) error {
	cutoff := stamp(before)
	for {
		rows, err := c.st.ListExpiredArtifacts(ctx, cutoff, evictBatch)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			if err := c.deleteOne(ctx, row); err != nil {
				return err
			}
		}
	}
}

// evictOverLimit 按 last_used_at 升序删分片，直到 SUM(bytes) 落回 maxBytes 内
// （docs/CACHE-MODEL.md「淘汰」第 2 步）。
//
// 每批重新问一次总量（不用本地累加）：行里的 bytes 与实际文件大小可能已经被外部
// 改动过，累加会漂移；重问一次是同一张表的一次聚合，代价可忽略。
func (c *Cache) evictOverLimit(ctx context.Context, maxBytes int64) error {
	for {
		total, err := c.st.SumArtifactBytes(ctx, store.ArtifactKindSegment)
		if err != nil {
			return err
		}
		if total <= maxBytes {
			return nil
		}
		rows, err := c.st.ListArtifactsOldest(ctx, store.ArtifactKindSegment, evictBatch)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil // 没有行可删了：总量只来自这张表，走到这里说明并发写入刚把它清空
		}
		// 一批可能有 100 条，远多于此刻需要删的条数：删到上限以内就停，别把还在用的
		// 当「一批」一起删掉。外层循环会重新问一次总量做校验。
		for _, row := range rows {
			if total <= maxBytes {
				return nil
			}
			if err := c.deleteOne(ctx, row); err != nil {
				return err
			}
			total -= row.Bytes
		}
	}
}

// deleteOne 删一条缓存资产：先删文件、再删行。文件已不存在也算成功（自愈），
// 行已不存在同样算成功（并发淘汰）。
func (c *Cache) deleteOne(ctx context.Context, row store.Artifact) error {
	if err := os.Remove(row.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("删除缓存文件 %s: %w", row.Path, err)
	}
	if err := c.st.DeleteArtifact(ctx, row.Kind, row.Key); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// sweepTmp 删掉 tmp 目录里超过 tmpMaxAge 的文件，返回删掉的数量。
//
// 写盘被 kill 打断会在 tmp 留下半截文件（三个内容目录里不会有）。它不是缓存内容，
// 留着只白占磁盘，所以在淘汰里一起清扫。
func sweepTmp(dir string, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("列临时目录 %s: %w", dir, err)
	}
	cutoff := now.Add(-tmpMaxAge)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // 刚被 rename 走了
			}
			return removed, fmt.Errorf("读临时文件信息 %s: %w", entry.Name(), err)
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil &&
			!errors.Is(err, fs.ErrNotExist) {
			return removed, fmt.Errorf("删除临时文件 %s: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}

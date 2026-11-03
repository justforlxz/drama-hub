package store

import (
	"context"
	"fmt"
	"strings"
)

// 分类开关表（MKACG-668）是「哪些分类不要同步到 Jellyfin」的唯一真值。
//
// 行不存在 = 启用：默认全部开启，升级与新增分类都不需要写行。开关只改库，不触发
// 任何 Jellyfin 请求 —— 删除发生在下一轮同步（或手动「同步媒体库」）里。
const (
	// jellyfinDisabledListSQL 列出关闭的分类，顺序稳定（页面按名字排序显示）。
	jellyfinDisabledListSQL = `SELECT category FROM jellyfin_category_switch ORDER BY category`
	// jellyfinDisableSQL 关掉一个分类（重复关闭是幂等的，只刷新 updated_at）。
	jellyfinDisableSQL = `
INSERT INTO jellyfin_category_switch (category, updated_at)
VALUES ($1, now())
ON CONFLICT (category) DO UPDATE SET updated_at = excluded.updated_at`
	// jellyfinEnableSQL 重新开启：删行即是「启用」（行不存在 = 启用）。
	jellyfinEnableSQL = `DELETE FROM jellyfin_category_switch WHERE category = $1`
)

// DisabledJellyfinCategories 返回被关闭的分类名，按字典序升序。
//
// 每次现读库、不缓存：与 settings 一样，改完下一轮同步就按新状态执行（硬约束 5）。
func (s *Store) DisabledJellyfinCategories(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, jellyfinDisabledListSQL)
	if err != nil {
		return nil, fmt.Errorf("读 jellyfin 分类开关: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []string{}
	for rows.Next() {
		var category string
		if err := rows.Scan(&category); err != nil {
			return nil, fmt.Errorf("读 jellyfin 分类开关: %w", err)
		}
		out = append(out, category)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读 jellyfin 分类开关: %w", err)
	}
	return out, nil
}

// SetJellyfinCategoryEnabled 打开 / 关闭一个分类的媒体库同步。
//
// enabled=true 删行（行不存在 = 启用），false 写行。只改库：不在这里碰 Jellyfin，
// 也不打断正在跑的一轮（下一轮从库里现读）。
//
// 分类名不能为空：空名既不是任何分类，关掉它也没有意义（也没法从页面点出来）。
func (s *Store) SetJellyfinCategoryEnabled(ctx context.Context, category string, enabled bool) error {
	if strings.TrimSpace(category) == "" {
		return fmt.Errorf("%w: 分类名不能为空", ErrInvalidSetting)
	}
	query := jellyfinDisableSQL
	if enabled {
		query = jellyfinEnableSQL
	}
	if _, err := s.db.ExecContext(ctx, query, category); err != nil {
		return fmt.Errorf("写 jellyfin 分类开关: %w", err)
	}
	return nil
}

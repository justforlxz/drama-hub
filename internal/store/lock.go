package store

import (
	"context"
	"fmt"
)

// lockDrama 是写事务的统一加锁点：锁住一部剧的 `drama` 行，把锁序固定成
// 「drama → episode / merge_* / emby_item」。
//
// 为什么必须统一：PostgreSQL 用行锁做并发控制，两条事务若以相反顺序请求同两行，
// 就会互相等待（40P01 死锁，一方被中止）。换 PG 之前是单写者，这条竞争不存在，
// 所以这把锁是新机制，不是补丁。现有写路径的锁序：
//
//	同步完成    UpsertDrama（锁 drama）→ 逐集 UpsertEpisode → 重建派生表
//	首播回填    lockDrama → UPDATE episode → 重建派生表
//	派生表重建  lockDrama → 删/插 emby_item（顺带读 merge_*）
//	合并编排    lockDrama → 写/删 merge_group + merge_item → 重建派生表
//	删剧        DELETE drama（锁 drama）→ 级联删子表
//
// 新增任何「按剧写入子表」的事务都必须先调用它。文档见 docs/DEV-STANDARDS.md
// 「数据库」；锁序由外部持锁 + NOWAIT 探针的测试钉住
// （TestBackfillLocksDramaBeforeEpisode）。
//
// 剧目不存在时这里是 0 行更新、不加锁：后续写子表会以外键错误失败，不会静默成功。
func (s *Store) lockDrama(ctx context.Context, dramaID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`SELECT id FROM drama WHERE id = $1 FOR UPDATE`, dramaID); err != nil {
		return wrapWriteErr(fmt.Sprintf("锁定剧目 %d", dramaID), err)
	}
	return nil
}

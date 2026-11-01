package store

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// 写层哨兵错误。调用方用 errors.Is 判断，不比较错误字符串。
var (
	// ErrNotFound 表示按主键或唯一键要查/要删的记录不存在。
	ErrNotFound = errors.New("记录不存在")
	// ErrConflict 表示写入触发唯一或主键约束冲突。
	ErrConflict = errors.New("唯一约束冲突")
	// ErrInvalid 表示入参违反业务约束（合并编排的集数、归属、重叠等）。
	ErrInvalid = errors.New("入参非法")
)

// PostgreSQL SQLSTATE：23505 = unique_violation。
//
// 唯一索引与主键冲突在 PostgreSQL 里都是这一个码（不像 SQLite 分 UNIQUE /
// PRIMARYKEY 两个扩展码），因此映射到 ErrConflict 只需要一条规则。
const pgUniqueViolation = "23505"

// wrapWriteErr 把写路径的驱动错误映射为 store 的哨兵错误。
//
// 唯一/主键约束冲突统一映射为 ErrConflict；其余错误保持原样并带上 op 上下文。
// 所有写方法都必须经过它，保证 store 不向调用方泄漏裸露的 *pgconn.PgError。
func wrapWriteErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return fmt.Errorf("%s: %w", op, ErrConflict)
	}
	return fmt.Errorf("%s: %w", op, err)
}

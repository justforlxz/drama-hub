package store

import (
	"errors"
	"fmt"

	"modernc.org/sqlite"
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

// SQLite 扩展结果码。modernc.org/sqlite 在每条连接上启用扩展结果码
// （conn.go:130 调用 sqlite3_extended_result_codes），所以约束冲突带回子码
// 而不是基码 SQLITE_CONSTRAINT(19)。驱动没有导出这两个常量的名字，这里按
// lib/sqlite.go 的数值固化；实测（真实唯一/主键冲突）与之一致。
const (
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
)

// wrapWriteErr 把写路径的驱动错误映射为 store 的哨兵错误。
//
// 唯一/主键约束冲突统一映射为 ErrConflict；其余错误保持原样并带上 op 上下文。
// 所有写方法都必须经过它，保证 store 不向调用方泄漏裸露的 *sqlite.Error。
func wrapWriteErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
			return fmt.Errorf("%s: %w", op, ErrConflict)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

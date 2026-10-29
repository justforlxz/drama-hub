package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// DB 是一个 SQLite 连接池，嵌入 *sql.DB 以便调用方直接使用标准库 API。
//
// 用 Open 构造；零值不可用。连接参数（WAL / busy_timeout / foreign_keys）
// 由 Open 的 DSN 施加到每一条新连接上。
type DB struct {
	*sql.DB
}

// Open 打开（必要时创建）path 处的 SQLite 库，应用连接参数并迁移到最新版本。
//
// 连接参数必须写在 DSN 里：database/sql 是连接池，Open 之后用 Exec 设 PRAGMA
// 只影响当时那一条连接，池里其余连接拿不到 foreign_keys=ON；DSN 的 _pragma
// 由驱动在每条新连接上应用。
//
// 不创建 path 的父目录（由调用方保证）。任一步失败返回
// fmt.Errorf("open %s: %w", path, err)：打开、Ping、迁移共用同一层上下文。
func Open(ctx context.Context, path string) (*DB, error) {
	const pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", "file:"+path+"?"+pragmas)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &DB{DB: db}, nil
}

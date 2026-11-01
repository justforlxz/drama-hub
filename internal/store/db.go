package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// 启动期读取的环境变量名。连接串是必填项，池参数有默认值。
//
// 名字只在这里定义：cmd 的启动日志与「未知 DRAMA_HUB_*」提示都引用这些常量，
// 不在别处再抄一份字符串。
const (
	// EnvDBURL 是 PostgreSQL 连接串（postgres://user:pass@host:5432/db?sslmode=…）。
	EnvDBURL = "DRAMA_HUB_DB_URL"
	// EnvDBMaxOpenConns / EnvDBMaxIdleConns / EnvDBConnMaxLifetimeSec 是连接池参数。
	EnvDBMaxOpenConns       = "DRAMA_HUB_DB_MAX_OPEN_CONNS"
	EnvDBMaxIdleConns       = "DRAMA_HUB_DB_MAX_IDLE_CONNS"
	EnvDBConnMaxLifetimeSec = "DRAMA_HUB_DB_CONN_MAX_LIFETIME_SEC"
)

// 连接池默认值：单机部署下 10 条连接足以覆盖 HTTP 请求 + 调度器 + 缓存淘汰，
// 同时给 PostgreSQL 的 max_connections 留足余量。
const (
	defaultMaxOpenConns       = 10
	defaultMaxIdleConns       = 5
	defaultConnMaxLifetimeSec = 1800
)

// DB 是一个 PostgreSQL 连接池，嵌入 *sql.DB 以便调用方直接使用标准库 API。
//
// 用 Open 构造；零值不可用。连接池参数由 Open 从 env 施加。
type DB struct {
	*sql.DB

	// SchemaVersion 是 Open 完成后的 schema 版本（schema_migrations 的最大 version）。
	SchemaVersion int
	// Applied 是本次 Open 实际执行的迁移文件数：稳态启动为 0。
	Applied int
}

// Open 连接 dsn 指定的 PostgreSQL，施加连接池参数并迁移到最新版本。
//
// 连接池参数从 env 读（非法值直接报错，不静默退回默认：配错了要让部署方看见）。
// 迁移失败返回错误而不是继续提供半迁移服务——调用方必须据此退出。
//
// 错误文本会带失败文件名与失败前版本，但绝不带 dsn 本身：连接串里有密码。
func Open(ctx context.Context, dsn string) (*DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("open: %s 为空", EnvDBURL)
	}
	pool, err := poolSettingsFromEnv()
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	db.SetMaxOpenConns(pool.maxOpen)
	db.SetMaxIdleConns(pool.maxIdle)
	db.SetConnMaxLifetime(time.Duration(pool.maxLifetimeSec) * time.Second)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open: %w", err)
	}
	applied, err := migrate(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open: %w", err)
	}
	version, err := schemaVersion(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open: %w", err)
	}
	return &DB{DB: db, SchemaVersion: version, Applied: applied}, nil
}

// poolSettings 是 Open 施加到连接池上的三个参数。
type poolSettings struct {
	maxOpen        int
	maxIdle        int
	maxLifetimeSec int
}

// poolSettingsFromEnv 读连接池参数：未设置取默认值，设置了必须是非负整数。
func poolSettingsFromEnv() (poolSettings, error) {
	out := poolSettings{
		maxOpen:        defaultMaxOpenConns,
		maxIdle:        defaultMaxIdleConns,
		maxLifetimeSec: defaultConnMaxLifetimeSec,
	}
	for _, item := range []struct {
		env string
		dst *int
	}{
		{EnvDBMaxOpenConns, &out.maxOpen},
		{EnvDBMaxIdleConns, &out.maxIdle},
		{EnvDBConnMaxLifetimeSec, &out.maxLifetimeSec},
	} {
		raw, ok := os.LookupEnv(item.env)
		if !ok || raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return poolSettings{}, fmt.Errorf("open: %s=%q 不是非负整数", item.env, raw)
		}
		*item.dst = n
	}
	return out, nil
}

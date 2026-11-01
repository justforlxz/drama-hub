package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

// migrationFS 持有全部迁移文件。新增迁移只需把文件放进 migrations/，无需注册。
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationDir 是迁移文件在 embed FS 内的目录名。
const migrationDir = "migrations"

// migrationNameRE 校验迁移文件名 NNNN_<verb>_<subject>.sql，捕获十进制版本号。
var migrationNameRE = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

// migrationLockKey 是 advisory lock 的键（"drama_mg" 的 ASCII）。
//
// 锁是数据库级的：多副本同时启动时，只有一个实例真正执行迁移，
// 其余实例在拿到锁后读到已推进的版本、全部跳过。键值必须全仓唯一。
const migrationLockKey int64 = 0x6472616d615f6d67

// createSchemaMigrations 是版本表的唯一 DDL。版本真值只在这里，不再用 PRAGMA。
//
// 版本表本身也走 IF NOT EXISTS：空库第一次迁移时由 runner 建出来。
const createSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER     PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// migration 是一个已解析的迁移文件。
type migration struct {
	version int
	name    string
	sql     string
}

// migrations 读出并按版本升序返回全部迁移文件。
//
// 文件名不合规、版本重复、版本不连续（须从 1 起逐个 +1）都视为部署错误。
// 校验集中在这里，runner 与测试走同一条路径。
func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, migrationDir)
	if err != nil {
		return nil, fmt.Errorf("读取迁移目录: %w", err)
	}

	seen := make(map[int]string)
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		m := migrationNameRE.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("迁移文件名不合规: %q", name)
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("解析迁移版本 %q: %w", name, err)
		}
		if dup, ok := seen[version]; ok {
			return nil, fmt.Errorf("迁移版本 %d 重复: %q 与 %q", version, dup, name)
		}
		seen[version] = name

		body, err := fs.ReadFile(migrationFS, migrationDir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("读取迁移 %q: %w", name, err)
		}
		out = append(out, migration{version: version, name: name, sql: string(body)})
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("迁移目录 %s 内没有迁移文件", migrationDir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("迁移版本必须从 1 连续递增，第 %d 个是 %d (%s)", i+1, m.version, m.name)
		}
	}
	return out, nil
}

// queryRower 是 schemaVersion 需要的最小句柄：*sql.DB 与 *sql.Conn 都满足。
type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// txBeginner 是 applyMigration 需要的最小句柄：*sql.DB 与 *sql.Conn 都满足。
type txBeginner interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

// schemaVersion 读当前 schema 版本：schema_migrations 的最大 version，空表为 0。
func schemaVersion(ctx context.Context, q queryRower) (int, error) {
	var version int
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("读取 schema 版本: %w", err)
	}
	return version, nil
}

// migrate 把库迁移到最新版本，返回本次实际执行的迁移文件数。
//
// 并发保护用会话级 advisory lock：只迁移的那条连接持锁，多副本同时启动时
// 后到的实例拿锁后读到新版本、applied == 0。锁随连接关闭自动释放。
//
// 每个文件在独立事务内整体执行，末尾把版本号插进 schema_migrations；
// 任一文件失败即回滚并返回带文件名与「失败前版本」的错误，已提交的迁移保持不变
// （只向前，不做 down 迁移）。
//
// 幂等：第二次调用时全部跳过，applied == 0、版本不变。
func migrate(ctx context.Context, db *sql.DB) (int, error) {
	files, err := migrations()
	if err != nil {
		return 0, err
	}
	return migrateFiles(ctx, db, files)
}

// migrateFiles 是 migrate 的实现：按给定清单迁移。清单与 runner 分离，坏 SQL
// 的失败路径才能在测试里复现（生产清单只来自 embed）。
func migrateFiles(ctx context.Context, db *sql.DB, files []migration) (int, error) {
	// 锁必须与迁移在同一条连接上：会话级锁不跟着连接池游走。
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("获取迁移连接: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return 0, fmt.Errorf("获取迁移锁: %w", err)
	}
	defer func() {
		// 正常路径在这里显式释放。ctx 已用 WithoutCancel 包住，所以这条语句失败只可能是
		// 连接已坏；坏连接不会以可用状态回到池里（database/sql 会丢弃它），服务端在断开时
		// 释放会话级锁。也就是说：「解锁失败」与「锁被服务端释放」是同一时刻。
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := conn.ExecContext(ctx, createSchemaMigrations); err != nil {
		return 0, fmt.Errorf("建 schema_migrations: %w", err)
	}
	current, err := schemaVersion(ctx, conn)
	if err != nil {
		return 0, err
	}

	applied := 0
	for _, m := range files {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, conn, m); err != nil {
			return applied, fmt.Errorf("当前 schema 版本 %d，执行 %s 失败: %w", current, m.name, err)
		}
		current = m.version
		applied++
	}
	return applied, nil
}

// applyMigration 在单个事务里执行一个迁移文件，并把版本号写进 schema_migrations。
//
// 迁移文件是多条 DDL，pgx 在无参数时走 simple 协议，一次 Exec 即可整体执行。
func applyMigration(ctx context.Context, db txBeginner, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始迁移 %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }() // 提交成功后 Rollback 是 no-op

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("执行迁移 %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version) VALUES ($1)`, m.version); err != nil {
		return fmt.Errorf("记录迁移版本 %d (%s): %w", m.version, m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移 %s: %w", m.name, err)
	}
	return nil
}

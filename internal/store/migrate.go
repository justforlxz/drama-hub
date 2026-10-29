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

// migrate 把库迁移到最新版本，返回本次实际执行的迁移文件数。
//
// 版本记录在 PRAGMA user_version 里（因此不建版本表）；空库视为 0。
// 每个文件在独立事务内整体执行，末尾把 user_version 推进到该文件版本号；
// 任一文件失败即回滚并返回带上下文的错误，已提交的迁移保持不变。
//
// 幂等：第二次调用时全部跳过，applied == 0、user_version 不变。
// 迁移 runner 内不设置任何连接级 PRAGMA（事务内切换 foreign_keys 无效且无必要，
// 连接参数由 Open 的 DSN 负责）。
func migrate(ctx context.Context, db *sql.DB) (applied int, err error) {
	files, err := migrations()
	if err != nil {
		return 0, err
	}

	var current int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return 0, fmt.Errorf("读取 user_version: %w", err)
	}

	for _, m := range files {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

// applyMigration 在单个事务里执行一个迁移文件，并把 user_version 推进到它的版本号。
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始迁移 %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }() // 提交成功后 Rollback 是 no-op

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("执行迁移 %s: %w", m.name, err)
	}
	// user_version 不支持参数占位；版本号来自文件名的十进制解析，非用户输入，拼接安全。
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("设置 user_version=%d (%s): %w", m.version, m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移 %s: %w", m.name, err)
	}
	return nil
}

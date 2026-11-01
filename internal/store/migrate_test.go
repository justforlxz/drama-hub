package store

import (
	"context"
	"database/sql"
	"io/fs"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/justforlxz/drama-hub/internal/pgtest"
)

// openRaw 打开一个尚未迁移的测试库（独立 schema，空库），由测试自己决定跑不跑 migrate。
func openRaw(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", pgtest.DSN(t))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openTestDB 走生产路径（Open = 连接 + 迁移）。
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), pgtest.DSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// tableNames 返回当前 schema 内全部用户表的表名（升序）。
func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		t.Fatalf("查询 information_schema.tables: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("扫描表名: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历表名: %v", err)
	}
	return names
}

// userVersion 读 schema_migrations 的当前版本（空表为 0）。
func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	v, err := schemaVersion(context.Background(), db)
	if err != nil {
		t.Fatalf("读 schema 版本: %v", err)
	}
	return v
}

// latestVersion 返回迁移文件里的最大版本号。
func latestVersion(t *testing.T) int {
	t.Helper()
	files, err := migrations()
	if err != nil {
		t.Fatalf("读取迁移清单: %v", err)
	}
	return files[len(files)-1].version
}

// AC：空库迁到最新，applied / schema 版本 / 表集合都符合预期。
func TestMigrateToLatest(t *testing.T) {
	db := openRaw(t)
	ctx := context.Background()
	latest := latestVersion(t)

	applied, err := migrate(ctx, db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if applied != latest {
		t.Fatalf("applied = %d, 期望 %d（空库应执行全部迁移）", applied, latest)
	}
	if got := userVersion(t, db); got != latest {
		t.Fatalf("schema 版本 = %d, 期望 %d", got, latest)
	}

	want := []string{
		"artifact", "drama", "emby_item", "episode",
		"merge_group", "merge_item", "schema_migrations", "segment", "settings", "source_state",
	}
	if got := tableNames(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("表集合 = %v, 期望 %v", got, want)
	}
}

// AC：幂等。第二次迁移不执行任何文件、不动版本、不清数据。
func TestMigrateIdempotent(t *testing.T) {
	db := openRaw(t)
	ctx := context.Background()

	if _, err := migrate(ctx, db); err != nil {
		t.Fatalf("首次 migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO settings(key, value_json, updated_at) VALUES('seg_target_sec', '10', '2026-10-29T00:00:00Z')`); err != nil {
		t.Fatalf("写入 settings 行: %v", err)
	}
	before := userVersion(t, db)

	applied, err := migrate(ctx, db)
	if err != nil {
		t.Fatalf("第二次 migrate: %v", err)
	}
	if applied != 0 {
		t.Fatalf("第二次 applied = %d, 期望 0", applied)
	}
	if got := userVersion(t, db); got != before {
		t.Fatalf("schema 版本从 %d 变为 %d", before, got)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM settings WHERE key = 'seg_target_sec'`).Scan(&n); err != nil {
		t.Fatalf("统计 settings: %v", err)
	}
	if n != 1 {
		t.Fatalf("settings 行数 = %d, 期望 1（迁移不得清数据）", n)
	}
}

// AC：Open 是「连接 + 迁移」，并把本次结果报给调用方（启动日志用它）。
// 同一 schema 二次 Open：applied=0、版本不变。
func TestOpenReportsSchemaVersion(t *testing.T) {
	dsn := pgtest.DSN(t)
	latest := latestVersion(t)
	ctx := context.Background()

	first, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("首次 Open: %v", err)
	}
	if first.SchemaVersion != latest || first.Applied != latest {
		t.Fatalf("首次 Open = (版本 %d, applied %d), 期望 (%d, %d)",
			first.SchemaVersion, first.Applied, latest, latest)
	}
	_ = first.Close()

	second, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("二次 Open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if second.SchemaVersion != latest {
		t.Fatalf("二次 Open 版本 = %d, 期望 %d（不变）", second.SchemaVersion, latest)
	}
	if second.Applied != 0 {
		t.Fatalf("二次 Open applied = %d, 期望 0（迁移全部已执行）", second.Applied)
	}
}

// AC：多实例同时启动只有一个真正执行迁移，另一个等锁后跳过；无重复执行、无报错。
func TestMigrateConcurrentSingleApplier(t *testing.T) {
	dsn := pgtest.DSN(t)
	latest := latestVersion(t)
	ctx := context.Background()

	pools := make([]*sql.DB, 2)
	for i := range pools {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("打开第 %d 个连接池: %v", i+1, err)
		}
		pools[i] = db
		t.Cleanup(func() { _ = db.Close() })
	}

	applied := make([]int, len(pools))
	errs := make([]error, len(pools))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, db := range pools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			applied[i], errs[i] = migrate(ctx, db)
		}()
	}
	close(start)
	wg.Wait()

	total := 0
	for i := range pools {
		if errs[i] != nil {
			t.Fatalf("第 %d 个实例迁移报错: %v", i+1, errs[i])
		}
		total += applied[i]
	}
	if total != latest {
		t.Fatalf("两个实例的 applied 合计 = %d, 期望 %d（迁移被重复执行或漏执行）", total, latest)
	}
	if applied[0] != 0 && applied[1] != 0 {
		t.Fatalf("两个实例都执行了迁移（applied = %v），advisory lock 未生效", applied)
	}
	for i, db := range pools {
		if got := userVersion(t, db); got != latest {
			t.Fatalf("第 %d 个实例看到的版本 = %d, 期望 %d", i+1, got, latest)
		}
	}
	t.Logf("并发启动 applied = %v，合计 %d", applied, total)
}

// AC：迁移中途失败（坏 SQL）——runner 返回错误、版本停在失败前、错误里带文件名
// 与失败前版本（启动日志只有这一条依据）。
func TestMigrateFailureKeepsVersion(t *testing.T) {
	ctx := context.Background()
	files, err := migrations()
	if err != nil {
		t.Fatalf("读取迁移清单: %v", err)
	}
	before := len(files)

	db := openRaw(t)
	badList := append(append([]migration{}, files...), migration{
		version: before + 1,
		name:    "0005_bad_sql.sql",
		sql:     "THIS IS NOT SQL;",
	})
	applied, err := migrateFiles(ctx, db, badList)
	if err == nil {
		t.Fatal("坏 SQL 迁移未报错")
	}
	if applied != before {
		t.Fatalf("失败前执行了 %d 个迁移, 期望 %d（坏文件之前的全部成功）", applied, before)
	}
	for _, want := range []string{"0005_bad_sql.sql", "当前 schema 版本 " + strconv.Itoa(before)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文本缺 %q: %v", want, err)
		}
	}
	if got := userVersion(t, db); got != before {
		t.Fatalf("失败后版本 = %d, 期望停在 %d", got, before)
	}
	t.Logf("坏 SQL 失败后的可读错误: %v", err)
}

// AC：settings 双源（EnsureSettings 与迁移）表结构一致，且两条路径任一顺序共存都不报错。
func TestSettingsDDLDrift(t *testing.T) {
	ctx := context.Background()

	onlyEnsure := openRaw(t)
	if err := EnsureSettings(ctx, onlyEnsure); err != nil {
		t.Fatalf("EnsureSettings: %v", err)
	}
	onlyMigrate := openRaw(t)
	if _, err := migrate(ctx, onlyMigrate); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ensureCols := tableInfo(t, onlyEnsure, "settings")
	migrateCols := tableInfo(t, onlyMigrate, "settings")
	if !reflect.DeepEqual(ensureCols, migrateCols) {
		t.Fatalf("settings 表结构漂移:\n  EnsureSettings = %+v\n  migrate        = %+v", ensureCols, migrateCols)
	}

	ensureFirst := openRaw(t)
	if err := EnsureSettings(ctx, ensureFirst); err != nil {
		t.Fatalf("先 EnsureSettings: %v", err)
	}
	if _, err := migrate(ctx, ensureFirst); err != nil {
		t.Fatalf("EnsureSettings 后 migrate: %v", err)
	}

	migrateFirst := openRaw(t)
	if _, err := migrate(ctx, migrateFirst); err != nil {
		t.Fatalf("先 migrate: %v", err)
	}
	if err := EnsureSettings(ctx, migrateFirst); err != nil {
		t.Fatalf("migrate 后 EnsureSettings: %v", err)
	}
}

// 迁移文件名规约：NNNN_<verb>_<subject>.sql，集合非空、版本唯一且从 1 连续。
func TestMigrationFileNaming(t *testing.T) {
	re := regexp.MustCompile(`^[0-9]{4}_[a-z0-9_]+\.sql$`)
	entries, err := fs.ReadDir(migrationFS, migrationDir)
	if err != nil {
		t.Fatalf("读取迁移目录: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("迁移目录为空")
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("迁移目录内不应有子目录: %s", e.Name())
		}
		if !re.MatchString(e.Name()) {
			t.Errorf("迁移文件名不合规: %q", e.Name())
		}
	}

	// migrations() 已集中校验唯一与连续；复用它保证与 runner 同一条路径。
	files, err := migrations()
	if err != nil {
		t.Fatalf("migrations(): %v", err)
	}
	got := make([]int, len(files))
	for i, m := range files {
		got[i] = m.version
	}
	want := make([]int, len(files))
	for i := range want {
		want[i] = i + 1
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("迁移版本 = %v, 期望 %v（从 1 连续）", got, want)
	}
}

// AC：连不上库 / 参数非法时 Open 返回错误而非 panic（启动路径必须能报错退出）。
func TestOpenRejectsBadConfig(t *testing.T) {
	ctx := context.Background()

	if _, err := Open(ctx, ""); err == nil {
		t.Fatal("Open 空连接串未报错")
	}
	if _, err := Open(ctx, "this is not a dsn"); err == nil {
		t.Fatal("Open 非法连接串未报错")
	}

	t.Setenv(EnvDBMaxOpenConns, "-1")
	if _, err := Open(ctx, pgtest.DSN(t)); err == nil {
		t.Fatal("Open 非法池参数未报错")
	}
}

// column 是 information_schema.columns 参与双源比对的一行。
type column struct {
	name    string
	typ     string
	notnull bool
}

// tableInfo 读 (column_name, data_type, is_nullable)，按列序返回。
func tableInfo(t *testing.T, db *sql.DB, table string) []column {
	t.Helper()
	rows, err := db.Query(`SELECT column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1
		ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatalf("information_schema.columns(%s): %v", table, err)
	}
	defer rows.Close()

	var cols []column
	for rows.Next() {
		var (
			c        column
			nullable string
		)
		if err := rows.Scan(&c.name, &c.typ, &nullable); err != nil {
			t.Fatalf("扫描 columns(%s): %v", table, err)
		}
		c.notnull = nullable == "NO"
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 columns(%s): %v", table, err)
	}
	return cols
}

// sortedMapKeys 把 map 的键排成稳定顺序，供 t.Logf 输出。
func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedNames 把索引名集合排成稳定顺序，供 t.Logf 输出。
func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// indexNames 返回某张表上的索引名集合（走 pg_indexes，只看当前 schema）。
func indexNames(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT indexname FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = $1`, table)
	if err != nil {
		t.Fatalf("pg_indexes(%s): %v", table, err)
	}
	defer rows.Close()

	names := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("扫描 pg_indexes(%s): %v", table, err)
		}
		names[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 pg_indexes(%s): %v", table, err)
	}
	return names
}

// columnDefaults 返回 information_schema.columns 的 column_name → column_default（NULL 记为 ""）。
func columnDefaults(t *testing.T, db *sql.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT column_name, column_default
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1`, table)
	if err != nil {
		t.Fatalf("information_schema.columns(%s): %v", table, err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var name string
		var dflt sql.NullString
		if err := rows.Scan(&name, &dflt); err != nil {
			t.Fatalf("扫描 columns(%s): %v", table, err)
		}
		out[name] = dflt.String
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 columns(%s): %v", table, err)
	}
	return out
}

// AC：4 份迁移全部生效（0002 genre/tags/shard + 两索引 + source_state、
// 0003 artifact 扩 kind、0004 删 episode.play_url / play_url_at）。
func TestMigrateAllFiles(t *testing.T) {
	db := openRaw(t)
	ctx := context.Background()

	applied, err := migrate(ctx, db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	latest := latestVersion(t)
	if applied != latest {
		t.Fatalf("首次 applied = %d, 期望 %d", applied, latest)
	}
	t.Logf("空库首次 applied=%d 版本=%d", applied, userVersion(t, db))

	// 0004：直链两列必须消失（information_schema 是唯一真值，不看迁移文本）。
	episodeCols := columnDefaults(t, db, "episode")
	for _, gone := range []string{"play_url", "play_url_at"} {
		if _, ok := episodeCols[gone]; ok {
			t.Errorf("episode 仍有列 %s（0004 没生效）", gone)
		}
	}
	for _, kept := range []string{"id", "drama_id", "ep_no", "title", "duration_sec", "source", "vid"} {
		if _, ok := episodeCols[kept]; !ok {
			t.Errorf("episode 缺列 %s（删列时丢多了）", kept)
		}
	}
	t.Logf("episode 列: %v", sortedMapKeys(episodeCols))

	applied, err = migrate(ctx, db)
	if err != nil {
		t.Fatalf("第二次 migrate: %v", err)
	}
	if applied != 0 {
		t.Fatalf("第二次 applied = %d, 期望 0", applied)
	}
	t.Logf("二次执行 applied=%d 版本=%d", applied, userVersion(t, db))
}

// AC：drama 三列就位——只给必填列插入时取 schema 默认值（” / '[]' / 0），
// 两条目录索引与淘汰索引必须就位（索引名是 docs/DATA-MODEL.md 的契约）。
func TestDramaGenreTagsShardColumns(t *testing.T) {
	db := openRaw(t)
	ctx := context.Background()
	if _, err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	defaults := columnDefaults(t, db, "drama")
	for _, col := range []string{"genre", "tags", "shard"} {
		if _, ok := defaults[col]; !ok {
			t.Fatalf("drama 缺列 %s", col)
		}
	}

	// 只给必填列 → 三列取 schema 默认值（不是 NULL，也不是空串以外的形态）。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO drama(provider, provider_id, title) VALUES('web', 'p1', '剧')`); err != nil {
		t.Fatalf("插入 drama: %v", err)
	}
	var genre, tags string
	var shard int
	if err := db.QueryRowContext(ctx,
		`SELECT genre, tags, shard FROM drama WHERE provider_id = 'p1'`).Scan(&genre, &tags, &shard); err != nil {
		t.Fatalf("读三列: %v", err)
	}
	if genre != "" || tags != "[]" || shard != 0 {
		t.Fatalf("默认值 = (%q, %q, %d), 期望 (\"\", \"[]\", 0)", genre, tags, shard)
	}

	dramaIdx := indexNames(t, db, "drama")
	for _, name := range []string{"idx_drama_genre_shard_title", "idx_drama_title"} {
		if !dramaIdx[name] {
			t.Errorf("drama 缺索引 %s（现有 %v）", name, dramaIdx)
		}
	}
	artifactIdx := indexNames(t, db, "artifact")
	if !artifactIdx["idx_artifact_kind_last_used"] {
		t.Errorf("artifact 缺索引 idx_artifact_kind_last_used（现有 %v）", artifactIdx)
	}
	t.Logf("drama 索引: %v", sortedNames(dramaIdx))
	t.Logf("artifact 索引: %v", sortedNames(artifactIdx))
	t.Logf("drama 默认值: genre=%s tags=%s shard=%s", defaults["genre"], defaults["tags"], defaults["shard"])
}

// AC：artifact 扩 kind 只加一类，已有 cover/index 两行逐字段保留；三类 kind 可写，
// CHECK 拒绝未知 kind。
func TestArtifactRebuildKeepsRows(t *testing.T) {
	db := openRaw(t)
	ctx := context.Background()

	files, err := migrations()
	if err != nil {
		t.Fatalf("读取迁移清单: %v", err)
	}
	// 手工跑到 0002，制造「扩 kind 前」的 artifact（旧 CHECK 只有 cover/index）。
	if _, err := db.ExecContext(ctx, createSchemaMigrations); err != nil {
		t.Fatalf("建 schema_migrations: %v", err)
	}
	for _, m := range files {
		if m.version > 2 {
			break
		}
		if err := applyMigration(ctx, db, m); err != nil {
			t.Fatalf("预置迁移 %s: %v", m.name, err)
		}
	}

	before := []Artifact{
		{Kind: ArtifactKindCover, Key: "web/p1", Path: "/data/cache/cover/aa/aa.jpg", Bytes: 1234,
			SHA256: "c0ffee", LastUsedAt: "2026-10-29T10:00:00Z"},
		{Kind: ArtifactKindIndex, Key: "e42", Path: "/data/cache/index/bb/bb.json", Bytes: 4321,
			SHA256: "", LastUsedAt: "2026-10-29T11:00:00Z"},
	}
	s := NewStore(db)
	for _, a := range before {
		if err := s.PutArtifact(ctx, a); err != nil {
			t.Fatalf("预置 artifact %s/%s: %v", a.Kind, a.Key, err)
		}
	}

	applied, err := migrate(ctx, db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if want := len(files) - 2; applied != want {
		t.Fatalf("扩 kind 段 applied = %d, 期望 %d", applied, want)
	}

	for _, want := range before {
		got, err := s.ArtifactByKey(ctx, want.Kind, want.Key)
		if err != nil {
			t.Fatalf("扩 kind 后读 %s/%s: %v", want.Kind, want.Key, err)
		}
		if got != want {
			t.Errorf("扩 kind 后 %s/%s = %+v, 期望逐字段 %+v", want.Kind, want.Key, got, want)
		}
	}
	if n := countRows(t, db, "artifact"); n != len(before) {
		t.Fatalf("扩 kind 后 artifact 行数 = %d, 期望 %d", n, len(before))
	}

	// 三类 kind 都能写：cover/index 已在，补 segment。
	seg := Artifact{Kind: ArtifactKindSegment, Key: "sha256:abc", Path: "/data/cache/seg/cc/cc.ts",
		Bytes: 650000, SHA256: "abc", LastUsedAt: "2026-10-30T09:00:00Z"}
	if err := s.PutArtifact(ctx, seg); err != nil {
		t.Fatalf("写入 kind=segment: %v", err)
	}
	if _, err := s.ArtifactByKey(ctx, ArtifactKindSegment, seg.Key); err != nil {
		t.Fatalf("读回 kind=segment: %v", err)
	}

	// 未知 kind 被 CHECK 拒绝，且不留行。
	if err := s.PutArtifact(ctx, Artifact{Kind: "poster", Key: "k", Path: "/p", LastUsedAt: "x"}); err == nil {
		t.Fatal("未知 kind 写入未报错（CHECK 未生效）")
	}
	if n := countRows(t, db, "artifact"); n != len(before)+1 {
		t.Fatalf("拒绝未知 kind 后行数 = %d, 期望 %d", n, len(before)+1)
	}
}

// AC：外键约束真的生效，且 ON DELETE CASCADE 定义正确。
func TestForeignKeyEnforced(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 引用不存在的 drama 必须失败。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO episode(drama_id, ep_no, source, vid) VALUES(999, 1, 'web', 'v1')`); err == nil {
		t.Fatal("引用不存在 drama_id 的 episode 插入未报错")
	}

	var dramaID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO drama(provider, provider_id, title) VALUES('web', 'p1', 't') RETURNING id`).Scan(&dramaID); err != nil {
		t.Fatalf("插入 drama: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO episode(drama_id, ep_no, source, vid) VALUES($1, 1, 'web', 'v1')`, dramaID); err != nil {
		t.Fatalf("插入合法 episode: %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM drama WHERE id = $1`, dramaID); err != nil {
		t.Fatalf("删除 drama: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM episode WHERE drama_id = $1`, dramaID).Scan(&n); err != nil {
		t.Fatalf("统计 episode: %v", err)
	}
	if n != 0 {
		t.Fatalf("删除 drama 后残留 %d 条 episode, 期望 0（ON DELETE CASCADE 未生效）", n)
	}
}

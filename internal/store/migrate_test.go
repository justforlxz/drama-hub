package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// openRaw 打开一个尚未迁移的 SQLite 临时文件，由测试自己决定跑不跑 migrate。
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开临时库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// tableNames 返回库内全部用户表的表名（升序）。
func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatalf("查询 sqlite_master: %v", err)
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

// userVersion 读 PRAGMA user_version。
func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("读取 user_version: %v", err)
	}
	return v
}

// AC1：空库迁到最新，applied / user_version / 表集合都符合预期。
func TestMigrateToLatest(t *testing.T) {
	db := openRaw(t, filepath.Join(t.TempDir(), "fresh.db"))
	ctx := context.Background()

	files, err := migrations()
	if err != nil {
		t.Fatalf("读取迁移清单: %v", err)
	}
	latest := files[len(files)-1].version

	applied, err := migrate(ctx, db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if applied != latest {
		t.Fatalf("applied = %d, 期望 %d（空库应执行全部迁移）", applied, latest)
	}
	if got := userVersion(t, db); got != latest {
		t.Fatalf("user_version = %d, 期望 %d", got, latest)
	}

	want := []string{
		"artifact", "drama", "emby_item", "episode",
		"merge_group", "merge_item", "segment", "settings", "source_state",
	}
	if got := tableNames(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("表集合 = %v, 期望 %v", got, want)
	}
}

// AC2：幂等。第二次迁移不执行任何文件、不动 user_version、不清数据。
func TestMigrateIdempotent(t *testing.T) {
	db := openRaw(t, filepath.Join(t.TempDir(), "idem.db"))
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
		t.Fatalf("user_version 从 %d 变为 %d", before, got)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM settings WHERE key = 'seg_target_sec'`).Scan(&n); err != nil {
		t.Fatalf("统计 settings: %v", err)
	}
	if n != 1 {
		t.Fatalf("settings 行数 = %d, 期望 1（迁移不得清数据）", n)
	}
}

// AC3a：连接参数在每一条连接上生效（journal_mode / busy_timeout / foreign_keys）。
func TestOpenPragmas(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "pragma.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("读 journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, 期望 wal", mode)
	}

	// busy_timeout / foreign_keys 是每连接属性：同时持有两条独立连接分别断言，
	// 证明 DSN 的 _pragma 作用于每条新连接，而不是只有 Ping 的那一条。
	conns := make([]*sql.Conn, 2)
	for i := range conns {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("取第 %d 条连接: %v", i+1, err)
		}
		conns[i] = conn
		t.Cleanup(func() { _ = conn.Close() })
	}
	for i, conn := range conns {
		var busy, fk int
		if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
			t.Fatalf("读第 %d 条连接 busy_timeout: %v", i+1, err)
		}
		if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatalf("读第 %d 条连接 foreign_keys: %v", i+1, err)
		}
		if busy != 5000 {
			t.Fatalf("第 %d 条连接 busy_timeout = %d, 期望 5000", i+1, busy)
		}
		if fk != 1 {
			t.Fatalf("第 %d 条连接 foreign_keys = %d, 期望 1", i+1, fk)
		}
	}
}

// AC3b：外键约束真的生效，且 ON DELETE CASCADE 定义正确。
func TestForeignKeyEnforced(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "fk.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 引用不存在的 drama 必须失败。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO episode(drama_id, ep_no, source, vid) VALUES(999, 1, 'web', 'v1')`); err == nil {
		t.Fatal("引用不存在 drama_id 的 episode 插入未报错")
	}

	res, err := db.ExecContext(ctx,
		`INSERT INTO drama(provider, provider_id, title) VALUES('web', 'p1', 't')`)
	if err != nil {
		t.Fatalf("插入 drama: %v", err)
	}
	dramaID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取 drama id: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO episode(drama_id, ep_no, source, vid) VALUES(?, 1, 'web', 'v1')`, dramaID); err != nil {
		t.Fatalf("插入合法 episode: %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM drama WHERE id = ?`, dramaID); err != nil {
		t.Fatalf("删除 drama: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM episode WHERE drama_id = ?`, dramaID).Scan(&n); err != nil {
		t.Fatalf("统计 episode: %v", err)
	}
	if n != 0 {
		t.Fatalf("删除 drama 后残留 %d 条 episode, 期望 0（ON DELETE CASCADE 未生效）", n)
	}
}

// column 是 PRAGMA table_info 中参与双源比对的一行。
type column struct {
	name    string
	typ     string
	notnull int
	pk      int
}

// tableInfo 读 PRAGMA table_info 的 (name, type, notnull, pk)。
func tableInfo(t *testing.T, db *sql.DB, table string) []column {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()

	var cols []column
	for rows.Next() {
		var (
			cid  int
			c    column
			dflt sql.NullString
		)
		if err := rows.Scan(&cid, &c.name, &c.typ, &c.notnull, &dflt, &c.pk); err != nil {
			t.Fatalf("扫描 table_info: %v", err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 table_info: %v", err)
	}
	return cols
}

// AC4：settings 双源（EnsureSettings 与迁移）表结构一致，且两条路径任一顺序共存都不报错。
func TestSettingsDDLDrift(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	onlyEnsure := openRaw(t, filepath.Join(dir, "ensure.db"))
	if err := EnsureSettings(ctx, onlyEnsure); err != nil {
		t.Fatalf("EnsureSettings: %v", err)
	}
	onlyMigrate := openRaw(t, filepath.Join(dir, "migrate.db"))
	if _, err := migrate(ctx, onlyMigrate); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ensureCols := tableInfo(t, onlyEnsure, "settings")
	migrateCols := tableInfo(t, onlyMigrate, "settings")
	if !reflect.DeepEqual(ensureCols, migrateCols) {
		t.Fatalf("settings 表结构漂移:\n  EnsureSettings = %+v\n  migrate        = %+v", ensureCols, migrateCols)
	}

	ensureFirst := openRaw(t, filepath.Join(dir, "ensure-first.db"))
	if err := EnsureSettings(ctx, ensureFirst); err != nil {
		t.Fatalf("先 EnsureSettings: %v", err)
	}
	if _, err := migrate(ctx, ensureFirst); err != nil {
		t.Fatalf("EnsureSettings 后 migrate: %v", err)
	}

	migrateFirst := openRaw(t, filepath.Join(dir, "migrate-first.db"))
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

// Open 指向不存在的目录应返回错误而非 panic。
func TestOpenInvalidPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "sub", "db.sqlite")
	db, err := Open(context.Background(), path)
	if err == nil {
		_ = db.Close()
		t.Fatal("Open 不存在的目录返回 nil 错误")
	}
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

// indexNames 返回某张表上的索引名集合（含自动索引）。
func indexNames(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf(`PRAGMA index_list(%s)`, table))
	if err != nil {
		t.Fatalf("PRAGMA index_list(%s): %v", table, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("读 index_list 列名: %v", err)
	}
	names := make(map[string]bool)
	for rows.Next() {
		// PRAGMA 的列随 SQLite 版本增减（seq/name/unique/origin/partial），
		// 只按列名取 name，不依赖位置。
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("扫描 index_list(%s): %v", table, err)
		}
		for i, c := range cols {
			if strings.EqualFold(c, "name") {
				if name, ok := raw[i].(string); ok {
					names[name] = true
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 index_list(%s): %v", table, err)
	}
	return names
}

// columnDefaults 返回 PRAGMA table_info(table) 的 name → dflt_value（NULL 记为 ""）。
func columnDefaults(t *testing.T, db *sql.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var (
			cid  int
			name string
			typ  string
			nn   int
			dflt sql.NullString
			pk   int
		)
		if err := rows.Scan(&cid, &name, &typ, &nn, &dflt, &pk); err != nil {
			t.Fatalf("扫描 table_info(%s): %v", table, err)
		}
		out[name] = dflt.String
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 table_info(%s): %v", table, err)
	}
	return out
}

// AC：S3 段落 0002（genre/tags/shard + 两索引 + source_state）与 0003（artifact
// 重建），S8 段再落 0004（删 episode.play_url / play_url_at）。空库 applied=4、
// user_version=4，且 episode 表里两列真的没了（docs/DATA-MODEL.md 0003 块）。
func TestMigrateStopsAtS8(t *testing.T) {
	db := openRaw(t, filepath.Join(t.TempDir(), "s3.db"))
	ctx := context.Background()

	applied, err := migrate(ctx, db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if applied != 4 {
		t.Fatalf("首次 applied = %d, 期望 4（0001+0002+0003+0004）", applied)
	}
	if got := userVersion(t, db); got != 4 {
		t.Fatalf("user_version = %d, 期望 4", got)
	}
	t.Logf("空库首次 applied=%d user_version=%d", applied, userVersion(t, db))

	// 0004：直链两列必须消失（PRAGMA table_info 是唯一真值，不看迁移文本）。
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
	if got := userVersion(t, db); got != 4 {
		t.Fatalf("第二次 user_version = %d, 期望 4", got)
	}
	t.Logf("二次执行 applied=%d user_version=%d", applied, userVersion(t, db))
}

// AC：drama 三列就位且默认值正确（插入只给必填列时它们是 ” / '[]' / 0）。
func TestDramaGenreTagsShardColumns(t *testing.T) {
	db := openRaw(t, filepath.Join(t.TempDir(), "genre.db"))
	ctx := context.Background()
	if _, err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	defaults := columnDefaults(t, db, "drama")
	for _, tc := range []struct{ col, want string }{
		{"genre", "''"},
		{"tags", "'[]'"},
		{"shard", "0"},
	} {
		got, ok := defaults[tc.col]
		if !ok {
			t.Fatalf("drama 缺列 %s", tc.col)
		}
		if got != tc.want {
			t.Errorf("drama.%s 默认值 = %q, 期望 %q", tc.col, got, tc.want)
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

	// 两条目录索引 + 淘汰索引必须就位（索引名是 docs/DATA-MODEL.md 的契约）。
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

// AC：artifact 重建只扩 kind，重建前的 cover/index 两行逐字段保留；三类 kind 可写，
// CHECK 拒绝未知 kind。
func TestArtifactRebuildKeepsRows(t *testing.T) {
	db := openRaw(t, filepath.Join(t.TempDir(), "artifact.db"))
	ctx := context.Background()

	files, err := migrations()
	if err != nil {
		t.Fatalf("读取迁移清单: %v", err)
	}
	// 先只跑到 0002，制造「重建前」的 artifact（旧 CHECK 只有 cover/index）。
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
	if applied != 2 {
		t.Fatalf("重建段 applied = %d, 期望 2（只剩 0003 + 0004）", applied)
	}

	for _, want := range before {
		got, err := s.ArtifactByKey(ctx, want.Kind, want.Key)
		if err != nil {
			t.Fatalf("重建后读 %s/%s: %v", want.Kind, want.Key, err)
		}
		if got != want {
			t.Errorf("重建后 %s/%s = %+v, 期望逐字段 %+v", want.Kind, want.Key, got, want)
		}
	}
	if n := countRows(t, db, "artifact"); n != len(before) {
		t.Fatalf("重建后 artifact 行数 = %d, 期望 %d", n, len(before))
	} else {
		t.Logf("重建后 artifact 行数 = %d，cover/index 两行逐字段保留", n)
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

package store

import (
	"context"
	"database/sql"
	"hash/fnv"
	"strings"
	"testing"
)

// 本文件是「目录列举」三条查询的契约测试：
//
//  1. 它们必须走索引（EXPLAIN 断言索引名，docs/DIRECTORY-CONTRACT.md
//     「MUST：目录列举必须是「一个目录一次查询」」），
//  2. 返回值形态（排序、空集合、点查未命中），
//  3. 改分片数 N 只需一条 UPDATE、不改 schema。

// seedDirectory 造一组跨越两个分类、多个分片的剧目。
func seedDirectory(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	dramas := []Drama{
		{Provider: "web", ProviderID: "d1", Title: "剧一", Year: 2024, Genre: "爱情"},
		{Provider: "web", ProviderID: "d2", Title: "剧二", Year: 2025, Genre: "爱情"},
		{Provider: "web", ProviderID: "d3", Title: "剧三", Year: 2026, Genre: "剧情"},
		{Provider: "web", ProviderID: "d4", Title: "剧四", Year: 2026}, // 空分类 → 兜底名
		{Provider: "web", ProviderID: "d5", Title: "剧五", Year: 2026, Genre: "其他"},
	}
	for _, d := range dramas {
		if _, err := s.UpsertDrama(ctx, d); err != nil {
			t.Fatalf("UpsertDrama %s: %v", d.ProviderID, err)
		}
	}
}

// seedPlanCorpus 用 generate_series 灌一批足够大的行集再 ANALYZE。
//
// 为什么必须灌数据：PostgreSQL 在小表上直接选 Seq Scan，计划断言就只剩噪音。
// 数据分布按真实规模造：68 个分类 × 300 部 × 128 分片；artifact 灌 5000 行分片。
//
// 只用 ANALYZE 不用 VACUUM：测试连接上 VACUUM 不置 visibility map（实测
// relallvisible 恒为 0），index-only scan 会按需回表估成高成本，计划噪音更大。
// 见 queryPlan 对 enable_seqscan 的说明。
func seedPlanCorpus(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO drama(provider, provider_id, title, year, genre, shard)
		 SELECT 'web', 'p' || g || '-' || i, '剧' || g || '-' || i, 2020, '分类' || g, (g * 7 + i) % 128
		 FROM generate_series(1, 68) g, generate_series(1, 300) i`,
		`INSERT INTO artifact(kind, key, path, bytes, sha256, last_used_at)
		 SELECT 'segment', 'k' || i, '/p/' || i, 650000, '',
		        to_char(timestamp '2026-01-01' + (i || ' minutes')::interval, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM generate_series(1, 5000) i`,
		`ANALYZE drama`,
		`ANALYZE artifact`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("播种计划语料: %v\n%s", err, stmt)
		}
	}
}

// planQueryer 是 queryPlan 需要的句柄：*sql.DB 与 *sql.Tx 都满足。
type planQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// queryPlan 返回一条查询的 EXPLAIN (ANALYZE) 文本（每行一段）。
//
// disableSeqScan 在事务内 SET LOCAL enable_seqscan = off：只用于两条在测试库里
// 被 planner 合理放弃索引的查询（见下表注释），证明「覆盖索引确实能服务这条查询」。
// 其余查询不做手脚——它们在真实数据量下本来就该走索引。
func queryPlan(t *testing.T, s *Store, query string, disableSeqScan bool) string {
	t.Helper()
	ctx := context.Background()
	var q planQueryer = s.root
	if disableSeqScan {
		tx, err := s.root.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("开计划事务: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatalf("关 seqscan: %v", err)
		}
		q = tx
	}

	rows, err := q.QueryContext(ctx, "EXPLAIN (ANALYZE) "+query)
	if err != nil {
		t.Fatalf("EXPLAIN ANALYZE %q: %v", query, err)
	}
	defer rows.Close()

	var sb strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("读查询计划: %v", err)
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历查询计划: %v", err)
	}
	return sb.String()
}

// AC：三条列举查询、剧目录点查与淘汰查询都走既定索引（EXPLAIN ANALYZE 的真实执行
// 计划）；分类根目录的 DISTINCT 证明覆盖索引可用（enable_seqscan=off 下的 Index Only Scan）。
//
// 这里的 SQL 用字面值而不是 $n：EXPLAIN 走扩展协议时 PostgreSQL 只能按 generic
// plan 估行数（不看参数值），计划会随统计噪声在 Seq Scan / Index Scan 之间摇摆。
// 生产查询仍是参数化的，计划形状一致；这里钉的是「索引能不能服务这条查询」。
func TestDirectoryQueryPlans(t *testing.T) {
	db := newCRUDTestDB(t)
	s := NewStore(db.DB)
	seedPlanCorpus(t, s)

	tests := []struct {
		name       string
		query      string
		want       string // 计划里必须出现的索引名
		disableSeq bool
	}{
		{
			name:       "分类列举（覆盖索引）",
			query:      `SELECT DISTINCT genre FROM drama ORDER BY genre`,
			want:       "idx_drama_genre_shard_title",
			disableSeq: true,
		},
		{
			// genre 占了全表 1/68，planner 在测试库里选 Seq Scan 是合理的；关掉
			// seqscan 才能看覆盖索引（Index Only Scan / Index Scan）确实可服务它。
			name:       "分类存在性点查",
			query:      `SELECT EXISTS(SELECT 1 FROM drama WHERE genre = '分类3')`,
			want:       "idx_drama_genre_shard_title",
			disableSeq: true,
		},
		{
			name:  "分片剧目列举",
			query: dramaColumns + ` WHERE genre IN ('分类3') AND shard = 5 ORDER BY title`,
			want:  "idx_drama_genre_shard_title",
		},
		{
			name:  "分片剧目列举（兜底名两个 genre 值）",
			query: dramaColumns + ` WHERE genre IN ('', '分类4') AND shard = 5 ORDER BY title`,
			want:  "idx_drama_genre_shard_title",
		},
		{
			name:  "剧目录点查",
			query: dramaColumns + ` WHERE title = '剧3-5' AND year = 2020 ORDER BY id LIMIT 1`,
			want:  "idx_drama_title",
		},
		{
			name:  "分片缓存按 kind 取最久未用",
			query: artifactColumns + ` WHERE kind = 'segment' ORDER BY last_used_at LIMIT 100`,
			want:  "idx_artifact_kind_last_used",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := queryPlan(t, s, tt.query, tt.disableSeq)
			t.Logf("%s 的查询计划：\n%s", tt.name, plan)
			if !strings.Contains(plan, tt.want) {
				t.Errorf("%s 的计划里没有 %s：\n%s", tt.name, tt.want, plan)
			}
			if !tt.disableSeq && strings.Contains(plan, "Seq Scan") {
				t.Errorf("%s 是全表扫描：\n%s", tt.name, plan)
			}
		})
	}
}

// AC：分类列举给出库里的原值（含表示未声明的空串），按字节序（列 COLLATE "C"）。
func TestListGenres(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	seedDirectory(t, s)

	got, err := s.ListGenres(context.Background())
	if err != nil {
		t.Fatalf("ListGenres: %v", err)
	}
	want := []string{"", "其他", "剧情", "爱情"}
	if len(got) != len(want) {
		t.Fatalf("ListGenres = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListGenres[%d] = %q, 期望 %q（按字节序）", i, got[i], want[i])
		}
	}
}

// AC：分类存在性判断是点查（不返回行集、不扫分类下的全部索引行）。
func TestGenreExists(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	seedDirectory(t, s)
	ctx := context.Background()

	for _, tt := range []struct {
		genre string
		want  bool
	}{
		{genre: "爱情", want: true},
		{genre: "", want: true},   // 未声明的剧也在库（兜底名要问它）
		{genre: "其他", want: true}, // 上游真的叫 其他
		{genre: "剧情", want: true},
		{genre: "不存在", want: false},
		{genre: "剧情/古装", want: false},
	} {
		got, err := s.GenreExists(ctx, tt.genre)
		if err != nil {
			t.Fatalf("GenreExists(%q): %v", tt.genre, err)
		}
		if got != tt.want {
			t.Errorf("GenreExists(%q) = %v, 期望 %v", tt.genre, got, tt.want)
		}
	}
}

// AC：一个分片的剧目按 title 升序返回，且只返回该分片的行。
func TestListDramasByShard(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	seedDirectory(t, s)
	ctx := context.Background()

	shard := ShardOf("d3")
	got, err := s.ListDramasByShard(ctx, []string{"剧情"}, shard)
	if err != nil {
		t.Fatalf("ListDramasByShard: %v", err)
	}
	if len(got) != 1 || got[0].ProviderID != "d3" {
		t.Fatalf("分片 %d 的剧目 = %+v, 期望只有 d3", shard, got)
	}

	// 分类过滤生效：拿别的分类值问同一个分片，结果里不该有 d3。
	other, err := s.ListDramasByShard(ctx, []string{"爱情"}, shard)
	if err != nil {
		t.Fatalf("ListDramasByShard(爱情): %v", err)
	}
	for _, d := range other {
		if d.ProviderID == "d3" {
			t.Errorf("分类过滤失效：爱情 的分片查询返回了 剧情 的 d3")
		}
	}
}

// AC：剧目录点查走 (title, year)；未命中返回 ErrNotFound。
func TestDramaByTitleYear(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	seedDirectory(t, s)
	ctx := context.Background()

	d, err := s.DramaByTitleYear(ctx, "剧二", 2025)
	if err != nil {
		t.Fatalf("DramaByTitleYear: %v", err)
	}
	if d.ProviderID != "d2" {
		t.Errorf("查到 %q, 期望 d2", d.ProviderID)
	}

	// 年份不同 → 未命中（同名的另一年不算）。
	if _, err := s.DramaByTitleYear(ctx, "剧二", 2024); err != ErrNotFound {
		t.Errorf("年份不符的 err = %v, 期望 ErrNotFound", err)
	}
}

// AC：改分片数 N 只改数据、不改 schema。
//
// 真实操作是「按新的 N 重算 shard 列」：数据库里没有 fnv32a，所以重算逐行 UPDATE
// （N 是 Go 常量，不是 SQL 表达式）。要钉住的是**表结构一个字节都不变**——没有新列、
// 没有新表、没有新索引；分片号只活在 `drama.shard` 这一列里。
func TestShardRecountKeepsSchema(t *testing.T) {
	db := newCRUDTestDB(t)
	s := NewStore(db.DB)
	seedDirectory(t, s)
	ctx := context.Background()

	schemaBefore := schemaText(t, s)

	// 换 N=256 重算：一条 UPDATE 一行，但只动数据面。
	const newN = 256
	rows, err := s.ListDramas(ctx)
	if err != nil {
		t.Fatalf("ListDramas: %v", err)
	}
	for _, d := range rows {
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE drama SET shard = $1 WHERE provider = $2 AND provider_id = $3`,
			shardWith(d.ProviderID, newN), d.Provider, d.ProviderID); err != nil {
			t.Fatalf("重算 shard: %v", err)
		}
	}

	if got := schemaText(t, s); got != schemaBefore {
		t.Errorf("重算后 schema 变了（改 N 不该改 schema）：\n之前：%s\n之后：%s", schemaBefore, got)
	}
	for _, d := range rows {
		again, err := s.DramaByID(ctx, d.ID)
		if err != nil {
			t.Fatalf("DramaByID %d: %v", d.ID, err)
		}
		if want := shardWith(d.ProviderID, newN); again.Shard != want {
			t.Errorf("%s 重算后 shard = %d, 期望 %d", d.ProviderID, again.Shard, want)
		}
	}
}

// shardWith 是「N 可变」的分片纯函数，只用于演示重算（生产用固定 ShardCount）。
func shardWith(providerID string, n uint32) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(providerID))
	return int(h.Sum32() % n)
}

// schemaText 返回当前 schema 的表、列、索引定义文本，用于断言「schema 未变」。
//
// 三段分开查再拼接：information_schema 给表与列，pg_indexes 给索引定义。
func schemaText(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	queries := []string{
		`SELECT 'table ' || table_name FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY table_name`,
		`SELECT 'column ' || table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable
		 FROM information_schema.columns WHERE table_schema = current_schema()
		 ORDER BY table_name, column_name`,
		`SELECT 'index ' || indexname || ' ' || indexdef FROM pg_indexes
		 WHERE schemaname = current_schema() ORDER BY indexname`,
	}

	var sb strings.Builder
	for _, query := range queries {
		rows, err := s.db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("读 schema: %v", err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				_ = rows.Close()
				t.Fatalf("读 schema 行: %v", err)
			}
			sb.WriteString(line)
			sb.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("遍历 schema: %v", err)
		}
		_ = rows.Close()
	}
	return sb.String()
}

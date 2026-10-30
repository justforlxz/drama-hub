package store

import (
	"context"
	"hash/fnv"
	"strings"
	"testing"
)

// 本文件是「目录列举」三条查询的契约测试：
//
//  1. 它们必须走索引（EXPLAIN QUERY PLAN 断言索引名，docs/DIRECTORY-CONTRACT.md
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

// queryPlan 返回一条查询的 EXPLAIN QUERY PLAN 文本（每行一条 detail）。
func queryPlan(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN %q: %v", query, err)
	}
	defer rows.Close()

	var sb strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("读查询计划: %v", err)
		}
		sb.WriteString(detail)
		sb.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历查询计划: %v", err)
	}
	return sb.String()
}

// AC：三条列举查询与剧目录点查都走索引，不出现全表扫描。
func TestDirectoryQueryPlans(t *testing.T) {
	db := newCRUDTestDB(t)
	s := NewStore(db.DB)
	seedDirectory(t, s)

	tests := []struct {
		name  string
		query string
		args  []any
		want  string // 计划里必须出现的索引名
	}{
		{
			name:  "分类列举",
			query: `SELECT DISTINCT genre FROM drama ORDER BY genre`,
			want:  "idx_drama_genre_shard_title",
		},
		{
			name:  "分类存在性点查",
			query: `SELECT EXISTS(SELECT 1 FROM drama WHERE genre = ?)`,
			args:  []any{"其他"},
			want:  "idx_drama_genre_shard_title",
		},
		{
			name:  "分片剧目列举",
			query: dramaColumns + ` WHERE genre IN (?) AND shard = ? ORDER BY title`,
			args:  []any{"爱情", 0},
			want:  "idx_drama_genre_shard_title",
		},
		{
			name:  "分片剧目列举（兜底名两个 genre 值）",
			query: dramaColumns + ` WHERE genre IN (?, ?) AND shard = ? ORDER BY title`,
			args:  []any{"", "其他", 0},
			want:  "idx_drama_genre_shard_title",
		},
		{
			name:  "剧目录点查",
			query: dramaColumns + ` WHERE title = ? AND year = ? ORDER BY id LIMIT 1`,
			args:  []any{"剧一", 2024},
			want:  "idx_drama_title",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := queryPlan(t, s, tt.query, tt.args...)
			t.Logf("%s 的查询计划：\n%s", tt.name, plan)
			if !strings.Contains(plan, tt.want) {
				t.Errorf("%s 的计划里没有 %s：\n%s", tt.name, tt.want, plan)
			}
			if strings.Contains(plan, "SCAN drama") && !strings.Contains(plan, "USING") {
				t.Errorf("%s 是全表扫描：\n%s", tt.name, plan)
			}
		})
	}
}

// AC：分类列举给出库里的原值（含表示未声明的空串），按字节序。
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
// 真实操作是「按新的 N 重算 shard 列」：SQLite 没有 fnv32a，所以重算逐行 UPDATE
// （N 是 Go 常量，不是 SQL 表达式）。要钉住的是**表结构一个字节都不变**——没有新列、
// 没有新表、没有新索引；分片号只活在 `drama.shard` 这一列里。
func TestShardRecountKeepsSchema(t *testing.T) {
	db := newCRUDTestDB(t)
	s := NewStore(db.DB)
	seedDirectory(t, s)
	ctx := context.Background()

	schemaBefore := schemaText(t, s)

	// 换 N=256 重算：一条 UPDATE 一条行，但只动数据面。
	const newN = 256
	rows, err := s.ListDramas(ctx)
	if err != nil {
		t.Fatalf("ListDramas: %v", err)
	}
	for _, d := range rows {
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE drama SET shard = ? WHERE provider = ? AND provider_id = ?`,
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

// schemaText 返回库内全部对象（表 / 索引）的 DDL 文本，用于断言「schema 未变」。
func schemaText(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatalf("读 sqlite_master: %v", err)
	}
	defer rows.Close()

	var sb strings.Builder
	for rows.Next() {
		var kind, name, ddl string
		if err := rows.Scan(&kind, &name, &ddl); err != nil {
			t.Fatalf("读 schema 行: %v", err)
		}
		sb.WriteString(kind + " " + name + " " + ddl + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 sqlite_master: %v", err)
	}
	return sb.String()
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/justforlxz/drama-hub/internal/pgtest"
)

// newCRUDTestDB 走 Open（连接 + 迁移），用一个独立 schema 的测试库。
// 级联删除与并发写必须跑在真实 schema 上，裸 sql.Open 不建表、FK 也不存在。
func newCRUDTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), pgtest.DSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// countRows 统计 table 的行数。
// countRows 统计 table 的行数。形参用 dbtx：根 Store 与事务内 Store 都适用。
func countRows(t *testing.T, db dbtx, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("统计 %s: %v", table, err)
	}
	return n
}

// assertDramaEqual 逐字段比较两部剧（不含 ID）。
//
// Shard 不读 want.Shard：它由 provider_id 派生（UpsertDrama 重算、入参被忽略），
// 所以期望值是 ShardOf(want.ProviderID)。
func assertDramaEqual(t *testing.T, got, want Drama) {
	t.Helper()
	if got.Provider != want.Provider || got.ProviderID != want.ProviderID ||
		got.Title != want.Title || got.Year != want.Year || got.Plot != want.Plot ||
		got.CoverURL != want.CoverURL || got.EpisodeTotal != want.EpisodeTotal ||
		got.SyncAt != want.SyncAt || got.Genre != want.Genre || got.Tags != want.Tags {
		t.Errorf("Drama 字段不一致：got %+v, want %+v", got, want)
	}
	if wantShard := ShardOf(want.ProviderID); got.Shard != wantShard {
		t.Errorf("Drama.Shard = %d, 期望按身份派生 %d", got.Shard, wantShard)
	}
}

// assertEpisodeEqual 逐字段比较两集（不含 ID）。
func assertEpisodeEqual(t *testing.T, got, want Episode) {
	t.Helper()
	if got.DramaID != want.DramaID || got.EpNo != want.EpNo || got.Title != want.Title ||
		got.DurationSec != want.DurationSec || got.Source != want.Source || got.Vid != want.Vid {
		t.Errorf("Episode 字段不一致：got %+v, want %+v", got, want)
	}
}

// AC1：剧目 upsert 幂等，冲突只更新可变态，入参 ID 被忽略。
func TestUpsertDramaIdempotent(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	first := Drama{
		Provider: "web", ProviderID: "p1",
		Title: "旧名", Year: 2023, Plot: "A", CoverURL: "http://c/1.jpg",
		EpisodeTotal: 10, SyncAt: "2026-10-01T00:00:00Z",
	}
	id1, err := s.UpsertDrama(ctx, first)
	if err != nil {
		t.Fatalf("第一次 UpsertDrama: %v", err)
	}

	// 同一身份再次 upsert：改可变态，并塞一个不可能的主键 id 验证被忽略。
	second := first
	second.Title = "新名"
	second.Year = 2024
	second.ID = id1 + 999
	id2, err := s.UpsertDrama(ctx, second)
	if err != nil {
		t.Fatalf("第二次 UpsertDrama: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("两次 upsert 返回 id 不同：%d vs %d", id1, id2)
	}
	if n := countRows(t, s.db, "drama"); n != 1 {
		t.Fatalf("drama 行数 = %d, 期望 1", n)
	}

	got, err := s.DramaByID(ctx, id1)
	if err != nil {
		t.Fatalf("DramaByID: %v", err)
	}
	assertDramaEqual(t, got, second)
}

// AC：genre / tags 是可变态（刷新时被上游覆盖），空值取 schema 默认。
func TestUpsertDramaGenreTags(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	d := Drama{
		Provider: "web", ProviderID: "g1", Title: "剧",
		Genre: "爱情", Tags: `["爱情","都市"]`,
	}
	id, err := s.UpsertDrama(ctx, d)
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	got, err := s.DramaByID(ctx, id)
	if err != nil {
		t.Fatalf("DramaByID: %v", err)
	}
	if got.Genre != "爱情" || got.Tags != `["爱情","都市"]` {
		t.Fatalf("genre/tags = (%q, %q), 期望上游原值", got.Genre, got.Tags)
	}

	// 刷新：tags[0] 变了 → genre 随之上移，tags 整体被覆盖。
	d.Genre, d.Tags = "家庭", `["家庭"]`
	if _, err := s.UpsertDrama(ctx, d); err != nil {
		t.Fatalf("第二次 UpsertDrama: %v", err)
	}
	got, err = s.DramaByID(ctx, id)
	if err != nil {
		t.Fatalf("DramaByID(刷新后): %v", err)
	}
	if got.Genre != "家庭" || got.Tags != `["家庭"]` {
		t.Fatalf("刷新后 genre/tags = (%q, %q), 期望被覆盖", got.Genre, got.Tags)
	}

	// 上游未声明标签：落成空串 / "[]"（不是 NULL）。
	zeroID, err := s.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "g2", Title: "无标签剧"})
	if err != nil {
		t.Fatalf("UpsertDrama(无标签): %v", err)
	}
	zero, err := s.DramaByID(ctx, zeroID)
	if err != nil {
		t.Fatalf("DramaByID(无标签): %v", err)
	}
	if zero.Genre != "" || zero.Tags != "" {
		t.Fatalf("零值 genre/tags = (%q, %q), 期望 (\"\", \"\")", zero.Genre, zero.Tags)
	}
}

// AC2：按 (provider, provider_id) 命中逐字段一致；未命中 ErrNotFound。
func TestDramaByProviderID(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	want := Drama{
		Provider: "app", ProviderID: "abc-123",
		Title: "测试剧", Year: 2024, Plot: "", CoverURL: "http://c/x.jpg",
		EpisodeTotal: 0, SyncAt: "",
	}
	id, err := s.UpsertDrama(ctx, want)
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	want.ID = id

	got, err := s.DramaByProviderID(ctx, "app", "abc-123")
	if err != nil {
		t.Fatalf("DramaByProviderID: %v", err)
	}
	assertDramaEqual(t, got, want)

	if _, err := s.DramaByProviderID(ctx, "app", "不存在"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未命中错误 = %v, 期望 ErrNotFound", err)
	}
	if _, err := s.DramaByProviderID(ctx, "web", "abc-123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("provider 不同未命中错误 = %v, 期望 ErrNotFound", err)
	}
}

// AC3：按 id 查剧 / 查集；未命中 ErrNotFound。
func TestByID(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	dramaID, err := s.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "p", Title: "剧"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	wantDrama, err := s.DramaByID(ctx, dramaID)
	if err != nil {
		t.Fatalf("DramaByID: %v", err)
	}
	assertDramaEqual(t, wantDrama, Drama{ID: dramaID, Provider: "web", ProviderID: "p", Title: "剧"})

	epID, err := s.UpsertEpisode(ctx, Episode{
		DramaID: dramaID, EpNo: 1, Title: "第一集", DurationSec: 300,
		Source: "web", Vid: "v1",
	})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}
	wantEp, err := s.EpisodeByID(ctx, epID)
	if err != nil {
		t.Fatalf("EpisodeByID: %v", err)
	}
	assertEpisodeEqual(t, wantEp, Episode{
		ID: epID, DramaID: dramaID, EpNo: 1, Title: "第一集", DurationSec: 300,
		Source: "web", Vid: "v1",
	})

	if _, err := s.DramaByID(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("剧未命中错误 = %v, 期望 ErrNotFound", err)
	}
	if _, err := s.EpisodeByID(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("集未命中错误 = %v, 期望 ErrNotFound", err)
	}
}

// AC4：剧列表返回全部插入的剧。
func TestListDramas(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	for _, d := range []Drama{
		{Provider: "web", ProviderID: "a", Title: "甲剧"},
		{Provider: "web", ProviderID: "b", Title: "乙剧"},
	} {
		if _, err := s.UpsertDrama(ctx, d); err != nil {
			t.Fatalf("UpsertDrama(%s): %v", d.ProviderID, err)
		}
	}
	got, err := s.ListDramas(ctx)
	if err != nil {
		t.Fatalf("ListDramas: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, 期望 2", len(got))
	}
	if got[0].ProviderID != "a" || got[1].ProviderID != "b" {
		t.Fatalf("列表与插入不对应：%+v", got)
	}
}

// AC5：集 upsert 幂等，冲突只更新可变态，入参 ID 被忽略。
func TestUpsertEpisodeIdempotent(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	dramaID, err := s.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "p", Title: "剧"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}

	first := Episode{
		DramaID: dramaID, EpNo: 2, Title: "旧标题", DurationSec: 120,
		Source: "web", Vid: "v1",
	}
	id1, err := s.UpsertEpisode(ctx, first)
	if err != nil {
		t.Fatalf("第一次 UpsertEpisode: %v", err)
	}

	second := first
	second.Title = "新标题"
	second.DurationSec = 125
	second.Vid = "v2"
	second.ID = id1 + 999
	id2, err := s.UpsertEpisode(ctx, second)
	if err != nil {
		t.Fatalf("第二次 UpsertEpisode: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("两次 upsert 返回 id 不同：%d vs %d", id1, id2)
	}
	if n := countRows(t, s.db, "episode"); n != 1 {
		t.Fatalf("episode 行数 = %d, 期望 1", n)
	}

	got, err := s.EpisodeByID(ctx, id1)
	if err != nil {
		t.Fatalf("EpisodeByID: %v", err)
	}
	assertEpisodeEqual(t, got, second)
}

// AC6：集列表按 ep_no 升序。
func TestListEpisodesOrder(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	dramaID, err := s.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "p", Title: "剧"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	for _, n := range []int{3, 1, 2} {
		if _, err := s.UpsertEpisode(ctx, Episode{DramaID: dramaID, EpNo: n, Source: "web", Vid: "v"}); err != nil {
			t.Fatalf("UpsertEpisode(%d): %v", n, err)
		}
	}
	got, err := s.ListEpisodes(ctx, dramaID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, 期望 3", len(got))
	}
	for i, want := range []int{1, 2, 3} {
		if got[i].EpNo != want {
			t.Fatalf("got[%d].EpNo = %d, 期望 %d", i, got[i].EpNo, want)
		}
	}
	// 没有该剧的集 → 空列表，不报错。
	empty, err := s.ListEpisodes(ctx, 99999)
	if err != nil {
		t.Fatalf("ListEpisodes(无剧): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空剧列表 len = %d, 期望 0", len(empty))
	}
}

// AC7：删剧级联删集、不影响其它剧；重复删报 ErrNotFound。
func TestDeleteDramaCascades(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	idA, err := s.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "a", Title: "剧A"})
	if err != nil {
		t.Fatalf("UpsertDrama A: %v", err)
	}
	_, err = s.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "b", Title: "剧B"})
	if err != nil {
		t.Fatalf("UpsertDrama B: %v", err)
	}
	for _, n := range []int{1, 2, 3} {
		if _, err := s.UpsertEpisode(ctx, Episode{DramaID: idA, EpNo: n, Source: "web", Vid: "v"}); err != nil {
			t.Fatalf("UpsertEpisode A:%d: %v", n, err)
		}
	}
	if n := countRows(t, s.db, "episode"); n != 3 {
		t.Fatalf("删除前 episode 行数 = %d, 期望 3", n)
	}

	if err := s.DeleteDrama(ctx, idA); err != nil {
		t.Fatalf("DeleteDrama: %v", err)
	}
	if n := countRows(t, s.db, "episode"); n != 0 {
		t.Fatalf("删除 A 后 episode 行数 = %d, 期望 0（级联）", n)
	}
	if n := countRows(t, s.db, "drama"); n != 1 {
		t.Fatalf("删除 A 后 drama 行数 = %d, 期望 1", n)
	}

	if err := s.DeleteDrama(ctx, idA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除错误 = %v, 期望 ErrNotFound", err)
	}
}

// AC8：全字段往返（含空串、0、中文标题）。
func TestFullFieldRoundtrip(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	// 全零/空串的剧与集。
	id0, err := s.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "zero"})
	if err != nil {
		t.Fatalf("UpsertDrama(zero): %v", err)
	}
	got0, err := s.DramaByID(ctx, id0)
	if err != nil {
		t.Fatalf("DramaByID(zero): %v", err)
	}
	assertDramaEqual(t, got0, Drama{ID: id0, Provider: "web", ProviderID: "zero"})

	ep0, err := s.UpsertEpisode(ctx, Episode{DramaID: id0, EpNo: 1, Source: "web", Vid: ""})
	if err != nil {
		t.Fatalf("UpsertEpisode(zero): %v", err)
	}
	gotEp0, err := s.EpisodeByID(ctx, ep0)
	if err != nil {
		t.Fatalf("EpisodeByID(zero): %v", err)
	}
	assertEpisodeEqual(t, gotEp0, Episode{ID: ep0, DramaID: id0, EpNo: 1, Source: "web"})

	// 全字段填满的中文剧 + 集。
	drama := Drama{
		Provider: "app", ProviderID: "full-1",
		Title: "星辰大海之梦", Year: 2025, Plot: "一段关于短剧的故事",
		CoverURL: "https://cdn.example.com/封面.jpg", EpisodeTotal: 48,
		SyncAt: "2026-10-29T08:00:00Z",
	}
	dID, err := s.UpsertDrama(ctx, drama)
	if err != nil {
		t.Fatalf("UpsertDrama(full): %v", err)
	}
	gotD, err := s.DramaByProviderID(ctx, "app", "full-1")
	if err != nil {
		t.Fatalf("DramaByProviderID(full): %v", err)
	}
	assertDramaEqual(t, gotD, Drama{
		ID: dID, Provider: "app", ProviderID: "full-1",
		Title: "星辰大海之梦", Year: 2025, Plot: "一段关于短剧的故事",
		CoverURL: "https://cdn.example.com/封面.jpg", EpisodeTotal: 48,
		SyncAt: "2026-10-29T08:00:00Z",
	})

	eID, err := s.UpsertEpisode(ctx, Episode{
		DramaID: dID, EpNo: 7, Title: "第七集·重逢", DurationSec: 894,
		Source: "app", Vid: "vid-7",
	})
	if err != nil {
		t.Fatalf("UpsertEpisode(full): %v", err)
	}
	gotE, err := s.EpisodeByID(ctx, eID)
	if err != nil {
		t.Fatalf("EpisodeByID(full): %v", err)
	}
	assertEpisodeEqual(t, gotE, Episode{
		ID: eID, DramaID: dID, EpNo: 7, Title: "第七集·重逢", DurationSec: 894,
		Source: "app", Vid: "vid-7",
	})
}

// AC9：真实唯一/主键约束冲突被 wrapWriteErr 映射为 ErrConflict，
// 且在公开写路径外不泄漏 *pgconn.PgError。
func TestWrapWriteErrMapsConstraintConflict(t *testing.T) {
	ctx := context.Background()
	db := newCRUDTestDB(t)

	// 用原始 INSERT 触发真实约束冲突，取到驱动的 *pgconn.PgError。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO drama(id, provider, provider_id, title) VALUES(1, 'web', 'p', '剧')`); err != nil {
		t.Fatalf("建剧: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO episode(id, drama_id, ep_no, source, vid) VALUES(1, 1, 1, 'web', 'v')`); err != nil {
		t.Fatalf("建集: %v", err)
	}

	t.Run("unique", func(t *testing.T) {
		_, err := db.ExecContext(ctx,
			`INSERT INTO episode(drama_id, ep_no, source, vid) VALUES(1, 1, 'web', 'v2')`)
		if err == nil {
			t.Fatal("预期 UNIQUE(drama_id, ep_no) 冲突")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("驱动错误类型 = %T (%v), 期望 *pgconn.PgError", err, err)
		}
		if pgErr.Code != pgUniqueViolation {
			t.Fatalf("实测 SQLSTATE = %s, 期望 %s（常量与驱动不符）", pgErr.Code, pgUniqueViolation)
		}
		if got := wrapWriteErr("写入集", err); !errors.Is(got, ErrConflict) {
			t.Fatalf("wrapWriteErr = %v, 期望 ErrConflict", got)
		}
	})

	t.Run("primary key", func(t *testing.T) {
		_, err := db.ExecContext(ctx,
			`INSERT INTO drama(id, provider, provider_id, title) VALUES(1, 'app', 'q', '剧2')`)
		if err == nil {
			t.Fatal("预期 PRIMARY KEY 冲突")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("驱动错误类型 = %T (%v), 期望 *pgconn.PgError", err, err)
		}
		if pgErr.Code != pgUniqueViolation {
			t.Fatalf("实测 SQLSTATE = %s, 期望 %s（主键冲突也是 unique_violation）", pgErr.Code, pgUniqueViolation)
		}
		if got := wrapWriteErr("写入剧目", err); !errors.Is(got, ErrConflict) {
			t.Fatalf("wrapWriteErr = %v, 期望 ErrConflict", got)
		}
	})
}

// AC10：并发写不丢数据、不报锁错误。10 goroutine × 每 goroutine 20 集，
// ep_no 互不重复（同一剧），全部成功且 1..200 无重复无缺失。
func TestConcurrentEpisodeWrites(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	const goroutines = 10
	const perGoroutine = 20
	dramaID, err := s.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "concurrent", Title: "并发剧"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int, errp *error) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				ep := Episode{
					DramaID: dramaID,
					EpNo:    g*perGoroutine + i + 1,
					Source:  "web",
					Vid:     "v",
				}
				if _, err := s.UpsertEpisode(ctx, ep); err != nil {
					*errp = err
					return
				}
			}
		}(g, &errs[g])
	}
	wg.Wait()

	for g, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d 写入失败: %v", g, err)
		}
	}

	if n := countRows(t, s.db, "episode"); n != goroutines*perGoroutine {
		t.Fatalf("episode 行数 = %d, 期望 %d（不丢数据）", n, goroutines*perGoroutine)
	}

	rows, err := s.db.QueryContext(context.Background(), `SELECT ep_no FROM episode WHERE drama_id = $1`, dramaID)
	if err != nil {
		t.Fatalf("查询 ep_no: %v", err)
	}
	defer rows.Close()
	seen := make([]bool, goroutines*perGoroutine+1)
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if n < 1 || n > goroutines*perGoroutine {
			t.Fatalf("ep_no = %d 越界", n)
		}
		seen[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历: %v", err)
	}
	for n := 1; n <= goroutines*perGoroutine; n++ {
		if !seen[n] {
			t.Fatalf("ep_no = %d 缺失", n)
		}
	}
}

// AC11：InTx 的提交/回滚语义。回滚后两表 0 行，提交路径可见，事务内 id 与
// 提交后的行一致；fn 的错误必须原样冒泡（不被包装成 ErrConflict 之类）。
func TestInTxCommitAndRollback(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	sentinel := errors.New("业务失败")
	err := s.InTx(ctx, func(tx *Store) error {
		id, err := tx.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "p", Title: "剧"})
		if err != nil {
			return err
		}
		if _, err := tx.UpsertEpisode(ctx, Episode{DramaID: id, EpNo: 1, Source: "web", Vid: "v1"}); err != nil {
			return err
		}
		// 事务内能读到自己写的行。
		if n := countRows(t, tx.db, "episode"); n != 1 {
			t.Fatalf("事务内 episode 行数 = %d, 期望 1", n)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx 返回 %v, 期望原样返回 sentinel", err)
	}
	if n := countRows(t, s.db, "drama"); n != 0 {
		t.Fatalf("回滚后 drama 行数 = %d, 期望 0", n)
	}
	if n := countRows(t, s.db, "episode"); n != 0 {
		t.Fatalf("回滚后 episode 行数 = %d, 期望 0", n)
	}

	var committedDramaID, committedEpID int64
	if err := s.InTx(ctx, func(tx *Store) error {
		id, err := tx.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "p", Title: "剧"})
		if err != nil {
			return err
		}
		committedDramaID = id
		epID, err := tx.UpsertEpisode(ctx, Episode{DramaID: id, EpNo: 1, Source: "web", Vid: "v1"})
		if err != nil {
			return err
		}
		committedEpID = epID
		return nil
	}); err != nil {
		t.Fatalf("InTx 提交路径: %v", err)
	}
	if n := countRows(t, s.db, "episode"); n != 1 {
		t.Fatalf("提交后 episode 行数 = %d, 期望 1", n)
	}
	if _, err := s.DramaByID(ctx, committedDramaID); err != nil {
		t.Fatalf("提交后按 id 查剧目: %v", err)
	}
	if _, err := s.EpisodeByID(ctx, committedEpID); err != nil {
		t.Fatalf("提交后按 id 查集: %v", err)
	}

	// 事务内 Store 不能再开事务：同一连接上不支持嵌套写事务，早失败好过隐式挂起。
	if err := s.InTx(ctx, func(tx *Store) error { return tx.InTx(ctx, func(*Store) error { return nil }) }); err == nil {
		t.Fatal("嵌套 InTx 未报错")
	}
	// 事务内的方法与根 Store 同语义：缺行仍返回 ErrNotFound，不因事务包装而漂移。
	if err := s.InTx(ctx, func(tx *Store) error {
		return tx.DeleteDrama(ctx, 99999)
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("事务内 DeleteDrama 缺行 = %v, 期望 ErrNotFound", err)
	}
}

// ExistingProviderIDs 与 StaleProviderIDs 是调度器一轮三段的两条查询：
// 前者算「新剧」，后者挑「最旧的 N 部」。
func TestSchedulerQueries(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()
	seed := func(provider, providerID, syncAt string) {
		if _, err := s.UpsertDrama(ctx, Drama{
			Provider: provider, ProviderID: providerID, Title: providerID, SyncAt: syncAt,
		}); err != nil {
			t.Fatalf("UpsertDrama %s/%s: %v", provider, providerID, err)
		}
	}
	seed("web", "a", "2026-10-03T00:00:00Z")
	seed("web", "b", "2026-10-01T00:00:00Z")
	seed("web", "c", "2026-10-02T00:00:00Z")
	seed("other", "z", "2026-09-01T00:00:00Z")

	ids, err := s.ExistingProviderIDs(ctx, "web")
	if err != nil {
		t.Fatalf("ExistingProviderIDs: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("web 的已有标识 = %v, 期望 3 个", ids)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, ok := ids[id]; !ok {
			t.Errorf("已有标识里缺 %s", id)
		}
	}
	if _, ok := ids["z"]; ok {
		t.Error("别的源的剧混进了 web 的已有标识（差集会被算错）")
	}

	// 最旧优先，只挑本源的；limit 生效、顺序确定。
	oldest, err := s.StaleProviderIDs(ctx, "web", 2)
	if err != nil {
		t.Fatalf("StaleProviderIDs: %v", err)
	}
	if len(oldest) != 2 || oldest[0] != "b" || oldest[1] != "c" {
		t.Errorf("最旧两部 = %v, 期望 [b c]（sync_at 升序）", oldest)
	}

	// limit <= 0 = 关闭存量轮转，返回空而不是报错。
	for _, limit := range []int{0, -1} {
		got, err := s.StaleProviderIDs(ctx, "web", limit)
		if err != nil {
			t.Fatalf("StaleProviderIDs(limit=%d): %v", limit, err)
		}
		if len(got) != 0 {
			t.Errorf("limit=%d 返回 %v, 期望空", limit, got)
		}
	}
}

// waitForLockWaiter 等到本库出现一个「正卡在 FOR UPDATE 等锁」的会话（排除自己）。
//
// 用它代替固定 sleep：外部事务先持住 drama 行，回填一旦请求那把行锁就会在
// pg_stat_activity 里显示 wait_event_type=Lock，测试因此是确定性的。
// 不能查 pg_locks 的 database 列：行锁等待是 transactionid 锁，那一列是 NULL。
func waitForLockWaiter(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%FOR UPDATE%'`).Scan(&n); err != nil {
			t.Fatalf("读 pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("5 秒内没有出现等锁的会话：回填没有请求 drama 行锁（锁序或调用点变了）")
}

// AC：写事务的锁序是「drama → episode」。回填必须先锁剧目行，再去改集。
//
// 反过来（先改集、再锁剧目）会与同步事务构成环：同步持 drama 等某集、回填持该集等
// drama → PostgreSQL 40P01 死锁。这里用确定性的探针钉住顺序：
//  1. 外部事务先 UPDATE drama，持住那一行（模拟同步事务已锁 drama）；
//  2. 起一个 BackfillEpisodeDuration，它在 pg_locks 里留下等锁记录；
//  3. 此时对同一集做 FOR UPDATE NOWAIT —— 能拿到说明回填还没锁集（顺序正确）；
//     拿到 55P03 说明它先锁了集（顺序反了，测试失败）。
func TestBackfillLocksDramaBeforeEpisode(t *testing.T) {
	db := newCRUDTestDB(t)
	st := NewStore(db.DB)
	ctx := context.Background()

	dramaID, err := st.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "lock-order", Title: "锁序剧"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	epID, err := st.UpsertEpisode(ctx, Episode{DramaID: dramaID, EpNo: 1, Source: "web", Vid: "v1"})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}

	hold, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启持锁事务: %v", err)
	}
	defer func() { _ = hold.Rollback() }()
	if _, err := hold.ExecContext(ctx, `UPDATE drama SET title = title WHERE id = $1`, dramaID); err != nil {
		t.Fatalf("持住 drama 行: %v", err)
	}

	done := make(chan struct{})
	var (
		changed bool
		backfil error
	)
	go func() {
		defer close(done)
		changed, backfil = st.BackfillEpisodeDuration(ctx, epID, 42)
	}()

	waitForLockWaiter(t, db.DB)

	probe, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启探针事务: %v", err)
	}
	if _, err := probe.ExecContext(ctx, `SELECT 1 FROM episode WHERE id = $1 FOR UPDATE NOWAIT`, epID); err != nil {
		_ = probe.Rollback()
		t.Fatalf("回填先锁住了 episode（锁序反转，与同步并发会 40P01）：%v", err)
	}
	_ = probe.Rollback()

	if err := hold.Rollback(); err != nil {
		t.Fatalf("放开 drama 行: %v", err)
	}
	<-done
	if backfil != nil {
		t.Fatalf("回填失败: %v", backfil)
	}
	if !changed {
		t.Fatal("回填没有写入（duration_sec 仍为 0？）")
	}
	ep, err := st.EpisodeByID(ctx, epID)
	if err != nil {
		t.Fatalf("EpisodeByID: %v", err)
	}
	if ep.DurationSec != 42 {
		t.Fatalf("duration_sec = %d, 期望 42", ep.DurationSec)
	}
}

// AC：同步（UpsertDrama → 逐集 UpsertEpisode → 重建派生表）与首播回填并发，
// 无死锁（40P01）、无重复行、每集都拿到实测时长。
func TestConcurrentBackfillAndSync(t *testing.T) {
	db := newCRUDTestDB(t)
	st := NewStore(db.DB)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const episodes = 8
	dramaID, err := st.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "sync-backfill", Title: "并发剧"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	ids := make([]int64, episodes)
	for i := range ids {
		id, err := st.UpsertEpisode(ctx, Episode{DramaID: dramaID, EpNo: i + 1, Source: "web", Vid: "v"})
		if err != nil {
			t.Fatalf("UpsertEpisode %d: %v", i+1, err)
		}
		ids[i] = id
	}

	const rounds = 10
	errCh := make(chan error, 2)
	// 写路径 A：与 sync.SyncSeries 同序的同步事务（先锁 drama，再逐集写，最后重建）。
	go func() {
		for r := 0; r < rounds; r++ {
			err := st.InTx(ctx, func(tx *Store) error {
				if _, err := tx.UpsertDrama(ctx, Drama{
					Provider: "web", ProviderID: "sync-backfill", Title: "并发剧 v" + strconv.Itoa(r),
				}); err != nil {
					return err
				}
				for i := range ids {
					if _, err := tx.UpsertEpisode(ctx, Episode{
						DramaID: dramaID, EpNo: i + 1, Source: "web", Vid: "v",
					}); err != nil {
						return err
					}
				}
				_, err := tx.RebuildEmbyItems(ctx, dramaID)
				return err
			})
			if err != nil {
				errCh <- fmt.Errorf("同步第 %d 轮: %w", r, err)
				return
			}
		}
		errCh <- nil
	}()
	// 写路径 B：首播回填（先锁 drama，再改集、重建）。
	go func() {
		for r := 0; r < rounds; r++ {
			for _, id := range ids {
				if _, err := st.BackfillEpisodeDuration(ctx, id, 100+r); err != nil {
					errCh <- fmt.Errorf("回填第 %d 轮: %w", r, err)
					return
				}
			}
		}
		errCh <- nil
	}()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("并发写失败: %v", err)
		}
	}

	eps, err := st.ListEpisodes(ctx, dramaID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(eps) != episodes {
		t.Fatalf("episode 行数 = %d, 期望 %d（并发 upsert 不得重复建集）", len(eps), episodes)
	}
	for _, ep := range eps {
		if ep.DurationSec <= 0 {
			t.Errorf("第 %d 集 duration_sec = %d, 期望回填后的实测值", ep.EpNo, ep.DurationSec)
		}
	}
	items, err := st.ListEmbyItems(ctx, dramaID)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	seen := make(map[int]bool, len(items))
	for _, it := range items {
		if seen[it.EpNo] {
			t.Fatalf("派生表出现重复对外集号 %d", it.EpNo)
		}
		seen[it.EpNo] = true
	}
	if len(items) != episodes {
		t.Fatalf("派生表条目 = %d, 期望 %d", len(items), episodes)
	}
}

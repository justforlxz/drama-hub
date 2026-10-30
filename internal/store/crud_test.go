package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"modernc.org/sqlite"
)

// newCRUDTestDB 用 Open 建临时文件库。测试级联删除与并发写必须走真实 DSN
// （WAL / busy_timeout / foreign_keys）；不能用 sql.Open 裸连接，否则 FK 不生效、
// 级联删除测试会假绿。
func newCRUDTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "crud.db"))
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
		got.DurationSec != want.DurationSec || got.Source != want.Source || got.Vid != want.Vid ||
		got.PlayURL != want.PlayURL || got.PlayURLAt != want.PlayURLAt {
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
		Source: "web", Vid: "v1", PlayURL: "http://s/1", PlayURLAt: "2026-10-01T00:00:00Z",
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
		Source: "web", Vid: "v1", PlayURL: "http://s/1", PlayURLAt: "2026-10-01T00:00:00Z",
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
		Source: "web", Vid: "v1", PlayURL: "http://s/old", PlayURLAt: "2026-10-01T00:00:00Z",
	}
	id1, err := s.UpsertEpisode(ctx, first)
	if err != nil {
		t.Fatalf("第一次 UpsertEpisode: %v", err)
	}

	second := first
	second.Title = "新标题"
	second.DurationSec = 125
	second.PlayURL = "http://s/new"
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
		Source: "app", Vid: "vid-7", PlayURL: "https://s.example.com/v7", PlayURLAt: "2026-10-29T08:05:00Z",
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
		Source: "app", Vid: "vid-7", PlayURL: "https://s.example.com/v7", PlayURLAt: "2026-10-29T08:05:00Z",
	})
}

// AC9：真实唯一/主键约束冲突被 wrapWriteErr 映射为 ErrConflict，
// 且在公开写路径外不泄漏 *sqlite.Error。
func TestWrapWriteErrMapsConstraintConflict(t *testing.T) {
	ctx := context.Background()
	db := newCRUDTestDB(t)

	// 用原始 INSERT 触发真实约束冲突，取到驱动的 *sqlite.Error。
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
		var se *sqlite.Error
		if !errors.As(err, &se) {
			t.Fatalf("驱动错误类型 = %T (%v), 期望 *sqlite.Error", err, err)
		}
		if se.Code() != sqliteConstraintUnique {
			t.Fatalf("实测 Code = %d, 期望 %d（常量与驱动不符）", se.Code(), sqliteConstraintUnique)
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
		var se *sqlite.Error
		if !errors.As(err, &se) {
			t.Fatalf("驱动错误类型 = %T (%v), 期望 *sqlite.Error", err, err)
		}
		if se.Code() != sqliteConstraintPrimaryKey {
			t.Fatalf("实测 Code = %d, 期望 %d（常量与驱动不符）", se.Code(), sqliteConstraintPrimaryKey)
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

	rows, err := s.db.QueryContext(context.Background(), `SELECT ep_no FROM episode WHERE drama_id = ?`, dramaID)
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

	// 事务内 Store 不能再开事务：SQLite 不支持嵌套写事务，早失败好过隐式挂起。
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

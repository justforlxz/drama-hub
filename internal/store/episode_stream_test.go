package store

import (
	"context"
	"testing"
)

// seedStreamDrama 建一部剧与若干集，返回剧主键与集主键（按 ep_no 升序）。
func seedStreamDrama(t *testing.T, st *Store, providerID string, epNos ...int) (int64, []int64) {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: providerID, Title: providerID})
	if err != nil {
		t.Fatalf("UpsertDrama %s: %v", providerID, err)
	}
	ids := make([]int64, len(epNos))
	for i, no := range epNos {
		epID, err := st.UpsertEpisode(ctx, Episode{DramaID: id, EpNo: no, Source: "web", Vid: providerID})
		if err != nil {
			t.Fatalf("UpsertEpisode %d: %v", no, err)
		}
		ids[i] = epID
	}
	return id, ids
}

// TestEpisodeStreamFailSchema 覆盖 AC-1 的表结构：四列、主键、外键，且迁移可重放。
func TestEpisodeStreamFailSchema(t *testing.T) {
	ctx := context.Background()
	db := newCRUDTestDB(t)

	rows, err := db.DB.QueryContext(ctx,
		`SELECT column_name FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'episode_stream_fail'
		  ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("查列: %v", err)
	}
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("扫列名: %v", err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历列: %v", err)
	}
	want := []string{"episode_id", "reason", "failed_at", "retry_after"}
	if len(cols) != len(want) {
		t.Fatalf("列 = %v, 期望 %v", cols, want)
	}
	for i := range want {
		if cols[i] != want[i] {
			t.Fatalf("列 = %v, 期望 %v", cols, want)
		}
	}

	for _, kind := range []string{"PRIMARY KEY", "FOREIGN KEY"} {
		var n int
		if err := db.DB.QueryRowContext(ctx,
			`SELECT count(*) FROM information_schema.table_constraints
			  WHERE table_schema = current_schema() AND table_name = 'episode_stream_fail'
			    AND constraint_type = $1`, kind).Scan(&n); err != nil {
			t.Fatalf("查 %s: %v", kind, err)
		}
		if n != 1 {
			t.Errorf("%s 数量 = %d, 期望 1", kind, n)
		}
	}

	// 迁移幂等：再跑一次不报错、不重复执行（0011 带 IF NOT EXISTS，版本表已推进）。
	applied, err := migrate(ctx, db.DB)
	if err != nil {
		t.Fatalf("第二次迁移: %v", err)
	}
	if applied != 0 {
		t.Errorf("第二次迁移 applied = %d, 期望 0", applied)
	}
}

// TestStreamFailureUpsertClearAndPerDrama 覆盖 AC-1 的读写语义：
// 同集 upsert 覆盖成一行、按剧读取、批量清除互不牵连。
func TestStreamFailureUpsertClearAndPerDrama(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	dramaA, epsA := seedStreamDrama(t, st, "a", 1, 2)
	_, epsB := seedStreamDrama(t, st, "b", 1)

	firstRetry := "2026-11-04T10:15:00Z"
	if err := st.RecordStreamFailure(ctx, epsA[0], "upstream", "2026-11-04T10:00:00Z", firstRetry); err != nil {
		t.Fatalf("第一次 RecordStreamFailure: %v", err)
	}
	// 同一集再失败一次：覆盖成最近一次（仍是一行）。
	secondRetry := "2026-11-04T10:30:00Z"
	if err := st.RecordStreamFailure(ctx, epsA[0], "rejected", "2026-11-04T10:15:00Z", secondRetry); err != nil {
		t.Fatalf("第二次 RecordStreamFailure: %v", err)
	}
	if err := st.RecordStreamFailure(ctx, epsA[1], "no_stream", "2026-11-04T10:00:00Z", secondRetry); err != nil {
		t.Fatalf("RecordStreamFailure epsA[1]: %v", err)
	}
	if err := st.RecordStreamFailure(ctx, epsB[0], "login", "2026-11-04T10:00:00Z", secondRetry); err != nil {
		t.Fatalf("RecordStreamFailure epsB[0]: %v", err)
	}
	if n := countRows(t, st.db, "episode_stream_fail"); n != 3 {
		t.Fatalf("行数 = %d, 期望 3（同集覆盖不新增）", n)
	}

	fails, err := st.StreamFailures(ctx, dramaA)
	if err != nil {
		t.Fatalf("StreamFailures: %v", err)
	}
	if len(fails) != 2 {
		t.Fatalf("剧目 A 的失败记录 = %d 条, 期望 2（不串到 B）", len(fails))
	}
	got := fails[epsA[0]]
	want := StreamFailure{EpisodeID: epsA[0], Reason: "rejected", FailedAt: "2026-11-04T10:15:00Z", RetryAfter: secondRetry}
	if got != want {
		t.Errorf("最近一次失败记录 = %+v, 期望 %+v", got, want)
	}

	// 批量清除：只删列表里的集，别的剧与同剧未列出的集保留。
	if err := st.ClearStreamFailures(ctx, []int64{epsA[0], epsB[0]}); err != nil {
		t.Fatalf("ClearStreamFailures: %v", err)
	}
	if n := countRows(t, st.db, "episode_stream_fail"); n != 1 {
		t.Fatalf("清除后行数 = %d, 期望 1", n)
	}
	fails, err = st.StreamFailures(ctx, dramaA)
	if err != nil {
		t.Fatalf("清除后 StreamFailures: %v", err)
	}
	if _, ok := fails[epsA[0]]; ok {
		t.Errorf("集 %d 的失败记录应已删除", epsA[0])
	}
	if _, ok := fails[epsA[1]]; !ok {
		t.Errorf("集 %d 的失败记录不该被别的集的清除带走", epsA[1])
	}

	// 空列表是 no-op（预取批次为空时不该打一条 DELETE）。
	if err := st.ClearStreamFailures(ctx, nil); err != nil {
		t.Fatalf("ClearStreamFailures(nil): %v", err)
	}
}

// TestUpsertEpisodeStreamKeepsLastOK 覆盖 AC-2：解析落库（不知道新链能不能用）
// 不得擦掉「上次确认能用」；调用方显式给非空值时仍然覆盖。
func TestUpsertEpisodeStreamKeepsLastOK(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	_, eps := seedStreamDrama(t, st, "a", 1)
	id := eps[0]

	base := EpisodeStream{
		EpisodeID: id,
		Channel:   "web",
		URL:       "https://cdn.example/1.mp4",
		ExpiresAt: "2026-11-04T11:00:00Z",
		FetchedAt: "2026-11-04T10:00:00Z",
	}
	if err := st.UpsertEpisodeStream(ctx, base); err != nil {
		t.Fatalf("首次 UpsertEpisodeStream: %v", err)
	}
	touched := "2026-11-04T10:05:00Z"
	if err := st.TouchEpisodeStream(ctx, id, touched); err != nil {
		t.Fatalf("TouchEpisodeStream: %v", err)
	}

	// 刷新直链（新 URL、新过期时间），解析器不知道可用性 → LastOKAt 空串。
	refreshed := base
	refreshed.URL = "https://cdn.example/1-rotated.mp4"
	refreshed.ExpiresAt = "2026-11-04T11:10:00Z"
	refreshed.FetchedAt = "2026-11-04T10:10:00Z"
	if err := st.UpsertEpisodeStream(ctx, refreshed); err != nil {
		t.Fatalf("刷新 UpsertEpisodeStream: %v", err)
	}
	got, err := st.GetEpisodeStream(ctx, id)
	if err != nil {
		t.Fatalf("GetEpisodeStream: %v", err)
	}
	if got.LastOKAt != touched {
		t.Errorf("刷新后 last_ok_at = %q, 期望保留 %q", got.LastOKAt, touched)
	}
	if got.URL != refreshed.URL {
		t.Errorf("刷新后 url = %q, 期望 %q", got.URL, refreshed.URL)
	}

	// 显式给非空值：仍然覆盖（例如调用方确实确认过这条新链可用）。
	explicit := refreshed
	explicit.LastOKAt = "2026-11-04T10:20:00Z"
	if err := st.UpsertEpisodeStream(ctx, explicit); err != nil {
		t.Fatalf("显式 UpsertEpisodeStream: %v", err)
	}
	got, err = st.GetEpisodeStream(ctx, id)
	if err != nil {
		t.Fatalf("GetEpisodeStream: %v", err)
	}
	if got.LastOKAt != explicit.LastOKAt {
		t.Errorf("显式 last_ok_at = %q, 期望 %q", got.LastOKAt, explicit.LastOKAt)
	}
}

// TestDramaBlocksUsesCurrentChunkSize 钉住 AC-3 的数据来源：导出的分块入口与
// 对外派生用同一个 merge_chunk_size（改 N 后下一次读取立刻是新块）。
func TestDramaBlocksUsesCurrentChunkSize(t *testing.T) {
	ctx := context.Background()
	db := newCRUDTestDB(t)
	st := NewStore(db.DB)
	settings := NewSettings(db.DB)
	dramaID, _ := seedStreamDrama(t, st, "a", 1, 2, 3, 4, 5)

	if err := settings.Set(ctx, KeyMergeChunkSize, "2"); err != nil {
		t.Fatalf("Set merge_chunk_size: %v", err)
	}
	blocks, err := st.DramaBlocks(ctx, dramaID)
	if err != nil {
		t.Fatalf("DramaBlocks: %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("块数 = %d, 期望 3", len(blocks))
	}
	for i, want := range [][]int{{1, 2}, {3, 4}, {5}} {
		if len(blocks[i]) != len(want) {
			t.Fatalf("第 %d 块成员数 = %d, 期望 %d", i, len(blocks[i]), len(want))
		}
		for j, no := range want {
			if blocks[i][j].EpNo != no {
				t.Errorf("第 %d 块第 %d 集号 = %d, 期望 %d", i, j, blocks[i][j].EpNo, no)
			}
		}
	}

	// 改 N 立刻生效（分块唯一的实现读的就是当前值）。
	if err := settings.Set(ctx, KeyMergeChunkSize, "10"); err != nil {
		t.Fatalf("Set merge_chunk_size=10: %v", err)
	}
	blocks, err = st.DramaBlocks(ctx, dramaID)
	if err != nil {
		t.Fatalf("改 N 后 DramaBlocks: %v", err)
	}
	if len(blocks) != 1 || len(blocks[0]) != 5 {
		t.Fatalf("改 N 后块 = %d 块（首块 %d 集）, 期望 1 块 5 集", len(blocks), len(blocks[0]))
	}

	// 空剧：0 块（不是 1 个空块）。
	emptyID, _ := seedStreamDrama(t, st, "empty")
	blocks, err = st.DramaBlocks(ctx, emptyID)
	if err != nil {
		t.Fatalf("空剧 DramaBlocks: %v", err)
	}
	if len(blocks) != 0 {
		t.Errorf("空剧块数 = %d, 期望 0", len(blocks))
	}
}

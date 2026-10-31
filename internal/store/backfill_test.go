package store

import (
	"context"
	"testing"
)

// seedZeroDurationDrama 建一部有 n 集的剧，时长全部是入库时的 0（上游批量拿不到），
// 并重建派生表 —— 与生产「同步完成 → 首播建索引」的起始状态一致。
func seedZeroDurationDrama(t *testing.T, st *Store, n int) (int64, []int64) {
	t.Helper()
	ctx := context.Background()
	did, err := st.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "b1", Title: "回填测试剧"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		id, err := st.UpsertEpisode(ctx, Episode{
			DramaID: did, EpNo: i + 1, Source: "web", Vid: "v",
		})
		if err != nil {
			t.Fatalf("UpsertEpisode %d: %v", i+1, err)
		}
		ids[i] = id
	}
	if _, err := st.RebuildEmbyItems(ctx, did); err != nil {
		t.Fatalf("RebuildEmbyItems: %v", err)
	}
	return did, ids
}

// episodeDuration 读一集的 duration_sec（回填的唯一真值是库里的列）。
func episodeDuration(t *testing.T, st *Store, id int64) int {
	t.Helper()
	ep, err := st.EpisodeByID(context.Background(), id)
	if err != nil {
		t.Fatalf("EpisodeByID %d: %v", id, err)
	}
	return ep.DurationSec
}

// TestBackfillEpisodeDuration_FillsZeroAndRebuildsRuntime 覆盖 AC「首播建索引回填」：
// 0 → 实测值，且同事务重建派生表，单集的 runtime_sec 立刻跟上（.nfo 的 <runtime>
// 就取它）。
func TestBackfillEpisodeDuration_FillsZeroAndRebuildsRuntime(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, eps := seedZeroDurationDrama(t, st, 3)

	if got := episodeDuration(t, st, eps[0]); got != 0 {
		t.Fatalf("初始 duration_sec = %d, 期望 0", got)
	}
	changed, err := st.BackfillEpisodeDuration(ctx, eps[0], 95)
	if err != nil {
		t.Fatalf("BackfillEpisodeDuration: %v", err)
	}
	if !changed {
		t.Errorf("回填 0 → 95 应报告已写行，实际 changed=false")
	}
	if got := episodeDuration(t, st, eps[0]); got != 95 {
		t.Errorf("回填后 duration_sec = %d, 期望 95", got)
	}
	items, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if items[0].RuntimeSec != 95 {
		t.Errorf("对外剧集 runtime_sec = %d, 期望 95（单集取本集时长）", items[0].RuntimeSec)
	}
	// 没播过的集仍是 0（AC：未建索引的集保持 0）。
	if got := episodeDuration(t, st, eps[1]); got != 0 {
		t.Errorf("未回填的集 duration_sec = %d, 期望 0", got)
	}
}

// TestBackfillEpisodeDuration_KeepsNonZero 覆盖 AC「回填不覆盖已有非零值」：
// 库里已有实测/上游值时，重复播放与索引重建都不许改写它，也不许动派生表。
func TestBackfillEpisodeDuration_KeepsNonZero(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, eps := seedZeroDurationDrama(t, st, 2)

	if _, err := st.UpsertEpisode(ctx, Episode{
		DramaID: did, EpNo: 1, DurationSec: 77, Source: "web", Vid: "v",
	}); err != nil {
		t.Fatalf("UpsertEpisode 非零时长: %v", err)
	}
	// 与生产一致：刷新写完集后在同一个同步事务末尾重建派生表。
	if _, err := st.RebuildEmbyItems(ctx, did); err != nil {
		t.Fatalf("RebuildEmbyItems: %v", err)
	}
	changed, err := st.BackfillEpisodeDuration(ctx, eps[0], 30)
	if err != nil {
		t.Fatalf("BackfillEpisodeDuration: %v", err)
	}
	if changed {
		t.Errorf("已有非零值时回填应报告未写行，实际 changed=true")
	}
	if got := episodeDuration(t, st, eps[0]); got != 77 {
		t.Errorf("已有非零值被覆盖：duration_sec = %d, 期望 77", got)
	}
	items, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if items[0].RuntimeSec != 77 {
		t.Errorf("对外剧集 runtime_sec = %d, 期望 77", items[0].RuntimeSec)
	}

	// 重复回填（第二次播放/索引重建）同样不改写已回填的值。
	changed, err = st.BackfillEpisodeDuration(ctx, eps[1], 40)
	if err != nil || !changed {
		t.Fatalf("首次回填 eps[1]: changed=%v err=%v", changed, err)
	}
	changed, err = st.BackfillEpisodeDuration(ctx, eps[1], 41)
	if err != nil {
		t.Fatalf("再次回填 eps[1]: %v", err)
	}
	if changed {
		t.Errorf("重复回填应报告未写行，实际 changed=true")
	}
	if got := episodeDuration(t, st, eps[1]); got != 40 {
		t.Errorf("重复回填后 duration_sec = %d, 期望 40", got)
	}
}

// TestBackfillEpisodeDuration_MergedRuntimeSum 钉住「派生表必须整体重算」这条：
// 合并集的 runtime_sec 是组内时长之和，回填其中一个成员后必须跟着变。
func TestBackfillEpisodeDuration_MergedRuntimeSum(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, eps := seedZeroDurationDrama(t, st, 3)

	if _, err := st.CreateMergeGroup(ctx, did, "第1-2集", eps[:2]); err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}
	if _, err := st.BackfillEpisodeDuration(ctx, eps[0], 60); err != nil {
		t.Fatalf("回填 eps[0]: %v", err)
	}
	if _, err := st.BackfillEpisodeDuration(ctx, eps[1], 50); err != nil {
		t.Fatalf("回填 eps[1]: %v", err)
	}

	items, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("对外剧集数 = %d, 期望 2：%+v", len(items), items)
	}
	if items[0].Kind != "merged" || items[0].RuntimeSec != 110 {
		t.Errorf("合并集 = (%s, runtime=%d), 期望 (merged, 110)", items[0].Kind, items[0].RuntimeSec)
	}
	if items[1].RuntimeSec != 0 {
		t.Errorf("未回填的单集 runtime_sec = %d, 期望 0", items[1].RuntimeSec)
	}
}

// TestBackfillEpisodeDuration_NoopInputs 覆盖「探测不到时长」与「集不存在」：
// 两者都不写库、不报错（索引照常建立失败/成功由调用方各自处理）。
func TestBackfillEpisodeDuration_NoopInputs(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	_, eps := seedZeroDurationDrama(t, st, 1)

	for _, sec := range []int{0, -3} {
		changed, err := st.BackfillEpisodeDuration(ctx, eps[0], sec)
		if err != nil {
			t.Fatalf("回填 %d 秒: %v", sec, err)
		}
		if changed {
			t.Errorf("回填 %d 秒应报告未写行", sec)
		}
	}
	if got := episodeDuration(t, st, eps[0]); got != 0 {
		t.Errorf("非正时长被写进库：duration_sec = %d, 期望 0", got)
	}
	if changed, err := st.BackfillEpisodeDuration(ctx, 999999, 60); err != nil || changed {
		t.Errorf("不存在的集：changed=%v err=%v，期望 false/nil", changed, err)
	}
}

// TestUpsertEpisode_KeepsBackfilledDuration 覆盖回填的持久性：下一次刷新把上游的 0
// 写回来时不许清零（否则 .nfo 的 <runtime> 每轮刷新后回到 0），上游真给了非零值
// 时仍然覆盖。
func TestUpsertEpisode_KeepsBackfilledDuration(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, eps := seedZeroDurationDrama(t, st, 1)

	if _, err := st.BackfillEpisodeDuration(ctx, eps[0], 90); err != nil {
		t.Fatalf("回填: %v", err)
	}
	// 刷新携带上游的 0。
	if _, err := st.UpsertEpisode(ctx, Episode{
		DramaID: did, EpNo: 1, DurationSec: 0, Source: "web", Vid: "v2",
	}); err != nil {
		t.Fatalf("UpsertEpisode 0: %v", err)
	}
	if got := episodeDuration(t, st, eps[0]); got != 90 {
		t.Errorf("刷新把回填值清零了：duration_sec = %d, 期望 90", got)
	}
	// 上游真给了非零值 → 覆盖（更权威的来源）。
	if _, err := st.UpsertEpisode(ctx, Episode{
		DramaID: did, EpNo: 1, DurationSec: 120, Source: "web", Vid: "v2",
	}); err != nil {
		t.Fatalf("UpsertEpisode 120: %v", err)
	}
	if got := episodeDuration(t, st, eps[0]); got != 120 {
		t.Errorf("上游非零时长未覆盖：duration_sec = %d, 期望 120", got)
	}
}

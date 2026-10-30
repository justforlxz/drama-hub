package store

import (
	"context"
	"errors"
	"testing"
)

// seedMergeDrama 建一部有 n 集、每集时长 100+ep_no 秒的剧，返回 (剧 id, 集 id 列表)。
func seedMergeDrama(t *testing.T, st *Store, n int) (int64, []int64) {
	t.Helper()
	ctx := context.Background()
	did, err := st.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "m1", Title: "合并测试剧"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		id, err := st.UpsertEpisode(ctx, Episode{
			DramaID: did, EpNo: i + 1, Title: "", DurationSec: 100 + i + 1, Source: "web", Vid: "v",
		})
		if err != nil {
			t.Fatalf("UpsertEpisode %d: %v", i+1, err)
		}
		ids[i] = id
	}
	return did, ids
}

// AC：合并组占 1 个对外集号，其余每集各占 1 个，集号连续、顺序按源集号。
func TestRebuildEmbyItems_MergedCollapsesRange(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, eps := seedMergeDrama(t, st, 12)

	gid, err := st.CreateMergeGroup(ctx, did, "第1-10集", eps[:10])
	if err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}

	items, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	// 12 集、前 10 集并成 1 → 3 条对外剧集。
	if len(items) != 3 {
		t.Fatalf("对外剧集数 = %d, 期望 3：%+v", len(items), items)
	}

	merged := items[0]
	if merged.Kind != "merged" {
		t.Errorf("items[0].Kind = %q, 期望 merged", merged.Kind)
	}
	if merged.EpNo != 1 {
		t.Errorf("合并集对外集号 = %d, 期望 1", merged.EpNo)
	}
	if want := mergedUID(gid); merged.UID != want {
		t.Errorf("合并集 uid = %q, 期望 %q（m<group_id>）", merged.UID, want)
	}
	if merged.EpFrom != 1 || merged.EpTo != 10 {
		t.Errorf("合并集源集号范围 = (%d,%d), 期望 (1,10)", merged.EpFrom, merged.EpTo)
	}
	// runtime = 组内各集时长之和（101..110）。
	if want := 101 + 102 + 103 + 104 + 105 + 106 + 107 + 108 + 109 + 110; merged.RuntimeSec != want {
		t.Errorf("合并集 runtime = %d, 期望 %d", merged.RuntimeSec, want)
	}

	// 剩下两集按源集号接在后面，对外集号连续（2、3 ⇒ S01E02/S01E03）。
	for i, wantEp := range []int{11, 12} {
		got := items[i+1]
		if got.Kind != "single" {
			t.Errorf("items[%d].Kind = %q, 期望 single", i+1, got.Kind)
		}
		if got.EpNo != i+2 {
			t.Errorf("items[%d].EpNo = %d, 期望 %d（对外集号连续）", i+1, got.EpNo, i+2)
		}
		if got.EpFrom != wantEp || got.EpTo != wantEp {
			t.Errorf("items[%d] 源集号 = (%d,%d), 期望 (%d,%d)", i+1, got.EpFrom, got.EpTo, wantEp, wantEp)
		}
		if got.UID != singleUID(eps[wantEp-1]) {
			t.Errorf("items[%d].UID = %q, 期望 %q（e<episode_id>）", i+1, got.UID, singleUID(eps[wantEp-1]))
		}
	}
}

// AC：重建幂等——同一份编排重建两次，行内容（含 uid 与集号）完全一致。
func TestRebuildEmbyItems_Idempotent(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, eps := seedMergeDrama(t, st, 5)

	if _, err := st.RebuildEmbyItems(ctx, did); err != nil {
		t.Fatalf("第一次 Rebuild: %v", err)
	}
	first, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if len(first) != 5 {
		t.Fatalf("无合并组时对外剧集数 = %d, 期望 5", len(first))
	}

	if _, err := st.RebuildEmbyItems(ctx, did); err != nil {
		t.Fatalf("第二次 Rebuild: %v", err)
	}
	second, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems 第二次: %v", err)
	}
	if len(second) != len(first) {
		t.Fatalf("两次重建行数不同：%d vs %d", len(first), len(second))
	}
	for i := range first {
		a, b := first[i], second[i]
		if a.UID != b.UID || a.EpNo != b.EpNo || a.Kind != b.Kind || a.RuntimeSec != b.RuntimeSec {
			t.Errorf("行 %d 两次重建不一致：%+v vs %+v", i, a, b)
		}
	}
	if _, err := st.CreateMergeGroup(ctx, did, "g", eps[:2]); err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}
	// 合并后重复重建仍幂等。
	if _, err := st.RebuildEmbyItems(ctx, did); err != nil {
		t.Fatalf("合并后 Rebuild: %v", err)
	}
	third, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems 第三次: %v", err)
	}
	if len(third) != 4 {
		t.Errorf("合并 2 集后对外剧集数 = %d, 期望 4", len(third))
	}
}

// AC：删除合并组后派生表回到逐集视图（无残留行、无残留 uid）。
func TestDeleteMergeGroup_RestoresSingles(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, eps := seedMergeDrama(t, st, 4)

	gid, err := st.CreateMergeGroup(ctx, did, "g", eps[:3])
	if err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}
	items, _ := st.ListEmbyItems(ctx, did)
	if len(items) != 2 {
		t.Fatalf("合并 3 集后对外剧集数 = %d, 期望 2", len(items))
	}

	if err := st.DeleteMergeGroup(ctx, gid); err != nil {
		t.Fatalf("DeleteMergeGroup: %v", err)
	}
	items, err = st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("删除合并组后对外剧集数 = %d, 期望 4", len(items))
	}
	for i, it := range items {
		if it.Kind != "single" {
			t.Errorf("items[%d].Kind = %q, 期望 single", i, it.Kind)
		}
		if it.EpNo != i+1 {
			t.Errorf("items[%d].EpNo = %d, 期望 %d", i, it.EpNo, i+1)
		}
	}
	// 已删除的组再删一次 → ErrNotFound。
	if err := st.DeleteMergeGroup(ctx, gid); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除 err = %v, 期望 ErrNotFound", err)
	}
}

// AC：编排越界一律 ErrInvalid 且不落库（集数不足、重复、跨剧、跨组重叠、空 label）。
func TestCreateMergeGroup_Validation(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, eps := seedMergeDrama(t, st, 4)
	_, otherEp := seedOtherDrama(t, st)

	if _, err := st.CreateMergeGroup(ctx, did, "g", eps[:1]); !errors.Is(err, ErrInvalid) {
		t.Errorf("单集合并组 err = %v, 期望 ErrInvalid", err)
	}
	if _, err := st.CreateMergeGroup(ctx, did, "g", []int64{eps[0], eps[0]}); !errors.Is(err, ErrInvalid) {
		t.Errorf("重复集 err = %v, 期望 ErrInvalid", err)
	}
	if _, err := st.CreateMergeGroup(ctx, did, "g", []int64{eps[0], 999999}); !errors.Is(err, ErrInvalid) {
		t.Errorf("不存在的集 err = %v, 期望 ErrInvalid", err)
	}
	if _, err := st.CreateMergeGroup(ctx, did, "g", []int64{eps[0], otherEp}); !errors.Is(err, ErrInvalid) {
		t.Errorf("跨剧的集 err = %v, 期望 ErrInvalid", err)
	}
	if _, err := st.CreateMergeGroup(ctx, did, "", []int64{eps[0], eps[1]}); !errors.Is(err, ErrInvalid) {
		t.Errorf("空 label err = %v, 期望 ErrInvalid", err)
	}

	if _, err := st.CreateMergeGroup(ctx, did, "g1", eps[:2]); err != nil {
		t.Fatalf("CreateMergeGroup g1: %v", err)
	}
	// 与已有组合并同一集 → ErrInvalid（重叠会重复播放）。
	if _, err := st.CreateMergeGroup(ctx, did, "g2", []int64{eps[1], eps[2]}); !errors.Is(err, ErrInvalid) {
		t.Errorf("跨组重叠 err = %v, 期望 ErrInvalid", err)
	}

	// 失败路径不落库：只有 g1 一个组，成员 2 个。
	groups, err := st.ListMergeGroups(ctx, did)
	if err != nil {
		t.Fatalf("ListMergeGroups: %v", err)
	}
	if len(groups) != 1 || groups[0].Label != "g1" {
		t.Errorf("合并组 = %+v, 期望只有 g1", groups)
	}
	members, err := st.ListMergeMembers(ctx, groups[0].ID)
	if err != nil {
		t.Fatalf("ListMergeMembers: %v", err)
	}
	if len(members) != 2 {
		t.Errorf("g1 成员数 = %d, 期望 2", len(members))
	}
	if members[0].Ord != 1 || members[1].Ord != 2 {
		t.Errorf("成员 ord = %d/%d, 期望 1/2", members[0].Ord, members[1].Ord)
	}
	if members[0].EpisodeID != eps[0] || members[1].EpisodeID != eps[1] {
		t.Errorf("成员顺序 = %d/%d, 期望 %d/%d", members[0].EpisodeID, members[1].EpisodeID, eps[0], eps[1])
	}
}

// seedOtherDrama 建另一部只有 1 集的剧，返回 (剧 id, 集 id)。
func seedOtherDrama(t *testing.T, st *Store) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	did, err := st.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "m2", Title: "另一部"})
	if err != nil {
		t.Fatalf("UpsertDrama other: %v", err)
	}
	ep, err := st.UpsertEpisode(ctx, Episode{DramaID: did, EpNo: 1, Source: "web", Vid: "v"})
	if err != nil {
		t.Fatalf("UpsertEpisode other: %v", err)
	}
	return did, ep
}

// AC：合并组的成员顺序就是时间轴顺序（按 ord，而非源集号）。
func TestListMergeMembers_OrderedByOrd(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, eps := seedMergeDrama(t, st, 4)

	// 逆序编排：先第 3 集、后第 1 集。
	gid, err := st.CreateMergeGroup(ctx, did, "逆序", []int64{eps[2], eps[0]})
	if err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}
	members, err := st.ListMergeMembers(ctx, gid)
	if err != nil {
		t.Fatalf("ListMergeMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("成员数 = %d, 期望 2", len(members))
	}
	if members[0].EpisodeID != eps[2] || members[1].EpisodeID != eps[0] {
		t.Errorf("成员顺序 = [%d,%d], 期望 [%d,%d]（ord 决定时间轴）",
			members[0].EpisodeID, members[1].EpisodeID, eps[2], eps[0])
	}
	if members[0].EpNo != 3 || members[1].EpNo != 1 {
		t.Errorf("成员源集号 = [%d,%d], 期望 [3,1]", members[0].EpNo, members[1].EpNo)
	}
}

// AC：删剧级联清掉合并组与派生表（外键 ON DELETE CASCADE）。
func TestDeleteDrama_CascadesMergeTables(t *testing.T) {
	ctx := context.Background()
	db := newCRUDTestDB(t)
	st := NewStore(db.DB)
	did, eps := seedMergeDrama(t, st, 3)

	if _, err := st.CreateMergeGroup(ctx, did, "g", eps[:2]); err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}
	if err := st.DeleteDrama(ctx, did); err != nil {
		t.Fatalf("DeleteDrama: %v", err)
	}
	for _, table := range []string{"merge_group", "merge_item", "emby_item"} {
		if n := countRows(t, db.DB, table); n != 0 {
			t.Errorf("删剧后 %s 残留 %d 行, 期望 0", table, n)
		}
	}
}

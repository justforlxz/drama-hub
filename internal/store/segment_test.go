package store

import (
	"context"
	"errors"
	"testing"
)

// TestReplaceSegments 覆盖索引整体替换语义：先删后插（同一事务），重复调用幂等，
// 失败整体回滚。
func TestReplaceSegments(t *testing.T) {
	ctx := context.Background()
	db := newCRUDTestDB(t)
	st := NewStore(db.DB)

	did, err := st.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "s1", Title: "测试剧"})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	eid, err := st.UpsertEpisode(ctx, Episode{DramaID: did, EpNo: 1, Source: "web", Vid: "v1"})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}

	// 初始为空。
	if segs, err := st.ListSegments(ctx, eid); err != nil || len(segs) != 0 {
		t.Fatalf("初始 ListSegments = (%v, %v), 期望空", segs, err)
	}

	// 写入 3 片。
	first := []Segment{
		{EpisodeID: eid, StartSec: 0, Dur: 4, Keyframe: true, CodecSig: "sig1", Mode: "copy"},
		{EpisodeID: eid, StartSec: 4, Dur: 4, Keyframe: true, CodecSig: "sig1", Mode: "copy"},
		{EpisodeID: eid, StartSec: 8, Dur: 2, Keyframe: true, CodecSig: "sig1", Mode: "copy"},
	}
	if err := st.ReplaceSegments(ctx, eid, first); err != nil {
		t.Fatalf("ReplaceSegments 第一次: %v", err)
	}
	segs, err := st.ListSegments(ctx, eid)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segs) != 3 {
		t.Fatalf("片数 = %d, 期望 3", len(segs))
	}
	for i, want := range first {
		if segs[i].StartSec != want.StartSec || segs[i].Dur != want.Dur ||
			segs[i].Keyframe != want.Keyframe || segs[i].Mode != want.Mode ||
			segs[i].CodecSig != want.CodecSig {
			t.Errorf("片 %d = %+v, 期望 %+v", i, segs[i], want)
		}
	}

	// 整体替换：片数变化、旧切点不残留。
	second := []Segment{
		{EpisodeID: eid, StartSec: 0, Dur: 6, Keyframe: true, CodecSig: "sig2", Mode: "copy"},
		{EpisodeID: eid, StartSec: 6, Dur: 6, Keyframe: true, CodecSig: "sig2", Mode: "copy"},
	}
	if err := st.ReplaceSegments(ctx, eid, second); err != nil {
		t.Fatalf("ReplaceSegments 第二次: %v", err)
	}
	segs, _ = st.ListSegments(ctx, eid)
	if len(segs) != 2 {
		t.Fatalf("整体替换后片数 = %d, 期望 2（旧切点应被清掉）", len(segs))
	}
	if segs[0].StartSec != 0 || segs[1].StartSec != 6 {
		t.Errorf("替换后切点 = %v/%v, 期望 0/6", segs[0].StartSec, segs[1].StartSec)
	}

	// 幂等：同一份再写一次，行数不变。
	if err := st.ReplaceSegments(ctx, eid, second); err != nil {
		t.Fatalf("ReplaceSegments 第三次: %v", err)
	}
	segs, _ = st.ListSegments(ctx, eid)
	if len(segs) != 2 {
		t.Errorf("重复替换后片数 = %d, 期望 2", len(segs))
	}

	// 清空：传空切片等价于删除该集索引。
	if err := st.ReplaceSegments(ctx, eid, nil); err != nil {
		t.Fatalf("ReplaceSegments 清空: %v", err)
	}
	segs, _ = st.ListSegments(ctx, eid)
	if len(segs) != 0 {
		t.Errorf("清空后片数 = %d, 期望 0", len(segs))
	}

	// 另一集不受影响（episode_id 作用域正确）。
	eid2, err := st.UpsertEpisode(ctx, Episode{DramaID: did, EpNo: 2, Source: "web", Vid: "v2"})
	if err != nil {
		t.Fatalf("UpsertEpisode 2: %v", err)
	}
	if err := st.ReplaceSegments(ctx, eid2, first[:1]); err != nil {
		t.Fatalf("ReplaceSegments 集2: %v", err)
	}
	if segs, _ := st.ListSegments(ctx, eid); len(segs) != 0 {
		t.Errorf("集 1 应保持空，实际 %d 片（episode_id 作用域泄漏）", len(segs))
	}
}

// TestReplaceSegments_RollsBackOnFailure 覆盖事务回滚：某一片写入失败时，整个
// 索引（含已成功的片）都不落库。
func TestReplaceSegments_RollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	db := newCRUDTestDB(t)
	st := NewStore(db.DB)

	did, _ := st.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "s2", Title: "剧"})
	eid, _ := st.UpsertEpisode(ctx, Episode{DramaID: did, EpNo: 1, Source: "web", Vid: "v1"})

	// 先留一份好索引。
	if err := st.ReplaceSegments(ctx, eid, []Segment{
		{EpisodeID: eid, StartSec: 0, Dur: 5, Keyframe: true, CodecSig: "s", Mode: "copy"},
	}); err != nil {
		t.Fatalf("ReplaceSegments 初始: %v", err)
	}

	// 第二片用非法 mode 触发 CHECK 约束失败 → 事务整体回滚。
	bad := []Segment{
		{EpisodeID: eid, StartSec: 0, Dur: 3, Keyframe: true, CodecSig: "s", Mode: "copy"},
		{EpisodeID: eid, StartSec: 3, Dur: 3, Keyframe: true, CodecSig: "s", Mode: "bogus"},
	}
	err := st.ReplaceSegments(ctx, eid, bad)
	if err == nil {
		t.Fatal("非法 mode 应返回错误")
	}
	// CHECK 约束失败是 SQLSTATE 23514（check_violation），不是 23505（unique_violation），
	// wrapWriteErr 因此不映射为 ErrConflict —— 这里的断言重点是整体回滚。
	if !errors.Is(err, ErrConflict) {
		t.Logf("错误类别 = %v（CHECK 失败不映射 ErrConflict，符合 wrapWriteErr 契约）", err)
	}

	// 回滚：旧索引原样保留（整体替换是原子的）。
	segs, _ := st.ListSegments(ctx, eid)
	if len(segs) != 1 || segs[0].StartSec != 0 || segs[0].Dur != 5 {
		t.Errorf("回滚后索引 = %+v, 期望保留旧的 1 片（0,5）", segs)
	}
}

// TestListSegments_OrderAndScope 覆盖按 start_sec 升序与 episode_id 作用域。
func TestListSegments_OrderAndScope(t *testing.T) {
	ctx := context.Background()
	db := newCRUDTestDB(t)
	st := NewStore(db.DB)

	did, _ := st.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "s3", Title: "剧"})
	e1, _ := st.UpsertEpisode(ctx, Episode{DramaID: did, EpNo: 1, Source: "web", Vid: "v1"})
	e2, _ := st.UpsertEpisode(ctx, Episode{DramaID: did, EpNo: 2, Source: "web", Vid: "v2"})

	if err := st.ReplaceSegments(ctx, e1, []Segment{
		{EpisodeID: e1, StartSec: 8, Dur: 2, Keyframe: true, CodecSig: "s", Mode: "copy"},
		{EpisodeID: e1, StartSec: 0, Dur: 4, Keyframe: true, CodecSig: "s", Mode: "copy"},
		{EpisodeID: e1, StartSec: 4, Dur: 4, Keyframe: true, CodecSig: "s", Mode: "copy"},
	}); err != nil {
		t.Fatalf("ReplaceSegments: %v", err)
	}
	segs, err := st.ListSegments(ctx, e1)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segs) != 3 {
		t.Fatalf("片数 = %d, 期望 3", len(segs))
	}
	if segs[0].StartSec != 0 || segs[1].StartSec != 4 || segs[2].StartSec != 8 {
		t.Errorf("ListSegments 顺序 = %v/%v/%v, 期望 0/4/8", segs[0].StartSec, segs[1].StartSec, segs[2].StartSec)
	}
	if segs[0].ID == 0 {
		t.Error("读回的行应有自增主键")
	}

	// e2 无索引 → 空切片（不是 ErrNotFound）。
	if segs, err := st.ListSegments(ctx, e2); err != nil || len(segs) != 0 {
		t.Errorf("无索引集 ListSegments = (%v, %v), 期望空且无错误", segs, err)
	}

	// 删剧级联删索引。
	if err := st.DeleteDrama(ctx, did); err != nil {
		t.Fatalf("DeleteDrama: %v", err)
	}
	if segs, _ := st.ListSegments(ctx, e1); len(segs) != 0 {
		t.Errorf("删剧后索引应级联删除，实际 %d 片", len(segs))
	}
}

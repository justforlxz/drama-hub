package sync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/store"
)

// 本文件覆盖「刷新只增改不删」（docs/DATA-MODEL.md「增删改语义」、
// docs/SCHEDULER-MODEL.md「幂等」）：源侧集数变少、剧目下架都不许删库，
// 合并编排（merge_group / merge_item）也不许被刷新冲掉。

// workDetail 造一部有 n 集的剧（集号 1..n，集标识 vid-1..vid-n）。
func workDetail(n int) plugin.WorkDetail {
	eps := make([]plugin.Episode, n)
	for i := range eps {
		eps[i] = plugin.Episode{No: i + 1}
	}
	return plugin.WorkDetail{
		Work: plugin.Work{
			Ref:         plugin.WorkRef{Plugin: "web", ID: "w1"},
			Title:       "假剧",
			EpisodeDecl: n,
		},
		Episodes: eps,
	}
}

// newNoshrinkSyncer 构造一个可改数据的假插件 + 可控时钟的写库编排。
func newNoshrinkSyncer(t *testing.T) (*fakePlugin, *Syncer, *fakeClock) {
	t.Helper()
	p := &fakePlugin{name: "web", detail: workDetail(5)}
	clock := &fakeClock{t: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)}
	s := New(p, nil, nil)
	s.now = clock.now
	return p, s, clock
}

// 集数变少：剧照常在，只是上游声明从 5 集变成 3 集。
// 行为：drama.episode_total 跟着变小、sync_at 更新；已入库的第 4、5 集行不删。
func TestResyncKeepsEpisodesWhenDeclaredShrinks(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	p, s, clock := newNoshrinkSyncer(t)

	if err := s.SyncSeries(ctx, st, "w1"); err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	first, err := st.DramaByProviderID(ctx, "web", "w1")
	if err != nil {
		t.Fatalf("DramaByProviderID: %v", err)
	}
	clock.advance(time.Hour)

	p.detail = workDetail(3) // 源侧集数变少
	if err := s.SyncSeries(ctx, st, "w1"); err != nil {
		t.Fatalf("重跑同步: %v", err)
	}

	second, err := st.DramaByProviderID(ctx, "web", "w1")
	if err != nil {
		t.Fatalf("重跑后 DramaByProviderID: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("剧目 id 变了：%d → %d", first.ID, second.ID)
	}
	if second.EpisodeTotal != 3 {
		t.Errorf("episode_total = %d, 期望跟着声明走 = 3", second.EpisodeTotal)
	}
	if second.SyncAt == first.SyncAt {
		t.Errorf("sync_at 没更新（%q）：刷新必须留痕", second.SyncAt)
	}

	eps, err := st.ListEpisodes(ctx, second.ID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(eps) != 5 {
		t.Fatalf("集行 %d 条, 期望 5 条（第 4、5 集不删；实际 %v）", len(eps), epNos(eps))
	}
	for i, ep := range eps {
		if ep.EpNo != i+1 {
			t.Fatalf("第 %d 行 ep_no = %d, 期望 %d", i+1, ep.EpNo, i+1)
		}
	}
}

// 剧目下架：上游对 Work 回 ErrNotFound。
// 行为：本次同步返回 ErrNotFound，剧与集一行不删、sync_at 不动。
func TestResyncIgnoresDelistedWork(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	p, s, clock := newNoshrinkSyncer(t)

	if err := s.SyncSeries(ctx, st, "w1"); err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	before, err := st.DramaByProviderID(ctx, "web", "w1")
	if err != nil {
		t.Fatalf("DramaByProviderID: %v", err)
	}
	dramas, episodes := dbCounts(t, st)

	clock.advance(time.Hour)
	p.err = fmt.Errorf("%w: 上游 404", plugin.ErrNotFound)
	err = s.SyncSeries(ctx, st, "w1")
	if !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("err = %v, 期望 ErrNotFound（下架不是故障）", err)
	}

	afterDramas, afterEpisodes := dbCounts(t, st)
	if afterDramas != dramas || afterEpisodes != episodes {
		t.Fatalf("下架后库变了：(剧,集) %d/%d → %d/%d", dramas, episodes, afterDramas, afterEpisodes)
	}
	after, err := st.DramaByProviderID(ctx, "web", "w1")
	if err != nil {
		t.Fatalf("下架后查不到剧了: %v", err)
	}
	if after.SyncAt != before.SyncAt {
		t.Errorf("下架不该改 sync_at：%q → %q", before.SyncAt, after.SyncAt)
	}
}

// 合并编排不被刷新冲掉：合并组先建好，随后一次「集数变少」的重跑必须保住
// merge_group / merge_item，派生表里那条合并集也还在。
func TestResyncKeepsMergeGroups(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	p, s, clock := newNoshrinkSyncer(t)

	if err := s.SyncSeries(ctx, st, "w1"); err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	drama, err := st.DramaByProviderID(ctx, "web", "w1")
	if err != nil {
		t.Fatalf("DramaByProviderID: %v", err)
	}
	eps, err := st.ListEpisodes(ctx, drama.ID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	groupID, err := st.CreateMergeGroup(ctx, drama.ID, "第 1-2 集", []int64{eps[0].ID, eps[1].ID})
	if err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}

	clock.advance(time.Hour)
	p.detail = workDetail(3) // 集数变少
	if err := s.SyncSeries(ctx, st, "w1"); err != nil {
		t.Fatalf("重跑同步: %v", err)
	}

	groups, err := st.ListMergeGroups(ctx, drama.ID)
	if err != nil {
		t.Fatalf("ListMergeGroups: %v", err)
	}
	if len(groups) != 1 || groups[0].ID != groupID {
		t.Fatalf("合并组 = %+v, 期望仍是 id=%d 那一组（刷新不许冲掉合并编排）", groups, groupID)
	}
	members, err := st.ListMergeMembers(ctx, groupID)
	if err != nil {
		t.Fatalf("ListMergeMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("合并组成员 %d 个, 期望 2 个", len(members))
	}
	if members[0].EpisodeID != eps[0].ID || members[1].EpisodeID != eps[1].ID {
		t.Fatalf("合并组成员 = %+v, 期望仍是原来的两集 %d / %d", members, eps[0].ID, eps[1].ID)
	}

	items, err := st.ListEmbyItems(ctx, drama.ID)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	merged := 0
	for _, it := range items {
		if it.Kind == "merged" {
			merged++
			if want := fmt.Sprintf("m%d", groupID); it.UID != want {
				t.Fatalf("合并集 uid = %q, 期望 %q", it.UID, want)
			}
			if it.EpFrom != 1 || it.EpTo != 2 {
				t.Fatalf("合并集覆盖源集号 %d-%d, 期望 1-2", it.EpFrom, it.EpTo)
			}
		}
	}
	if merged != 1 {
		t.Fatalf("派生表里有 %d 条合并集, 期望 1 条（实际 %+v）", merged, items)
	}
	// 集行仍是 5 条：第 4、5 集不因声明变少而消失。
	after, err := st.ListEpisodes(ctx, drama.ID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(after) != 5 {
		t.Fatalf("集行 %d 条, 期望 5 条", len(after))
	}
}

// epNos 取集号列表，只为失败信息可读。
func epNos(eps []store.Episode) []int {
	out := make([]int, 0, len(eps))
	for _, ep := range eps {
		out = append(out, ep.EpNo)
	}
	return out
}

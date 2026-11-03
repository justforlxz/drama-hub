package store

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// seedMergeDrama 建一部有 n 集、每集时长 100+ep_no 秒的剧，返回 (剧 id, 集 id 列表)。
func seedMergeDrama(t *testing.T, st *Store, n int) (int64, []int64) {
	t.Helper()
	ctx := context.Background()
	did, err := st.UpsertDrama(ctx, Drama{Provider: "web", ProviderID: "m1", Title: "分块测试剧"})
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

// setChunkSize 写入 merge_chunk_size（不写时取注册表默认 10）。
func setChunkSize(t *testing.T, st *Store, n int) {
	t.Helper()
	if err := NewSettings(st.root).Set(context.Background(), KeyMergeChunkSize, strconv.Itoa(n)); err != nil {
		t.Fatalf("Set merge_chunk_size=%d: %v", n, err)
	}
}

// AC-1：分块纯函数的边界（0 集、1 集、余数、整倍、N=1、N 大于集数）。
func TestChunkEpisodes(t *testing.T) {
	eps := func(n int) []Episode {
		out := make([]Episode, n)
		for i := range out {
			out[i] = Episode{ID: int64(i + 1), EpNo: i + 1}
		}
		return out
	}
	sizes := func(blocks [][]Episode) []int {
		out := make([]int, 0, len(blocks))
		for _, b := range blocks {
			out = append(out, len(b))
		}
		return out
	}
	cases := []struct {
		name   string
		count  int
		n      int
		want   []int
		blocks int
	}{
		{"0 集", 0, 10, nil, 0},
		{"1 集", 1, 10, []int{1}, 1},
		{"3 集 N=10", 3, 10, []int{3}, 1},
		{"22 集 N=10（余数 2）", 22, 10, []int{10, 10, 2}, 3},
		{"22 集 N=5", 22, 5, []int{5, 5, 5, 5, 2}, 5},
		{"20 集 N=10（整倍，不多空组）", 20, 10, []int{10, 10}, 2},
		{"3 集 N=1（全部单集）", 3, 1, []int{1, 1, 1}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocks := chunkEpisodes(eps(c.count), c.n)
			if len(blocks) != c.blocks {
				t.Fatalf("块数 = %d, 期望 %d（sizes=%v）", len(blocks), c.blocks, sizes(blocks))
			}
			for i, want := range c.want {
				if len(blocks[i]) != want {
					t.Errorf("第 %d 块 = %d 集, 期望 %d", i+1, len(blocks[i]), want)
				}
			}
			// 块是连续切片、且按顺序覆盖全部集：块首尾相接、不丢不重。
			seen := 0
			for _, b := range blocks {
				for _, ep := range b {
					seen++
					if ep.EpNo != seen {
						t.Fatalf("块内集号顺序断了：第 %d 个是 %d", seen, ep.EpNo)
					}
				}
			}
			if seen != c.count {
				t.Errorf("覆盖了 %d 集, 期望 %d", seen, c.count)
			}
		})
	}
}

// AC-2：22 集、默认 N=10 → 3 条对外剧集（10 + 10 + 2），uid 与时长都按块派生。
//
// 没有任何「重建」步骤：ListEmbyItems 现算（ADR-0014）。
func TestListEmbyItems_DefaultChunkTen(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, _ := seedMergeDrama(t, st, 22)

	items, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("对外剧集数 = %d, 期望 3：%+v", len(items), items)
	}

	want := []struct {
		epNo     int
		uid      string
		from, to int
		runtime  int
	}{
		{1, mergedUID(did, 1, 10), 1, 10, sumDurations(1, 10)},
		{2, mergedUID(did, 11, 20), 11, 20, sumDurations(11, 20)},
		{3, mergedUID(did, 21, 22), 21, 22, sumDurations(21, 22)},
	}
	for i, w := range want {
		it := items[i]
		if it.Kind != "merged" || it.EpNo != w.epNo || it.UID != w.uid {
			t.Errorf("items[%d] = {%s ep_no=%d uid=%s}, 期望 {merged ep_no=%d uid=%s}",
				i, it.Kind, it.EpNo, it.UID, w.epNo, w.uid)
		}
		if it.EpFrom != w.from || it.EpTo != w.to {
			t.Errorf("items[%d] 源集号范围 = (%d,%d), 期望 (%d,%d)", i, it.EpFrom, it.EpTo, w.from, w.to)
		}
		if it.RuntimeSec != w.runtime {
			t.Errorf("items[%d] runtime = %d, 期望 %d（块内时长之和）", i, it.RuntimeSec, w.runtime)
		}
	}
}

// sumDurations 是 seedMergeDrama 里第 from..to 集的时长之和（每集 100+ep_no 秒）。
func sumDurations(from, to int) int {
	sum := 0
	for ep := from; ep <= to; ep++ {
		sum += 100 + ep
	}
	return sum
}

// AC-2 / AC-3 的核心：同一个 Store 实例上把 N 从 10 改成 5，**不触发任何同步或重建**，
// 下一次 ListEmbyItems 就是新分块（22 集 → 5+5+5+5+2），旧合并 uid 立即失效。
func TestListEmbyItems_ChangeChunkSizeImmediately(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, _ := seedMergeDrama(t, st, 22)

	before, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems(N=10): %v", err)
	}
	if len(before) != 3 {
		t.Fatalf("N=10 时对外剧集数 = %d, 期望 3", len(before))
	}
	oldUID := before[0].UID

	setChunkSize(t, st, 5)
	after, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems(N=5): %v", err)
	}
	if len(after) != 5 {
		t.Fatalf("N=5 时对外剧集数 = %d, 期望 5（改配置立即生效）：%+v", len(after), after)
	}
	wantSizes := []int{5, 5, 5, 5, 2}
	for i, it := range after {
		if got := it.EpTo - it.EpFrom + 1; got != wantSizes[i] {
			t.Errorf("第 %d 块覆盖 %d 集, 期望 %d", i+1, got, wantSizes[i])
		}
		if it.UID == oldUID {
			t.Errorf("改 N 后仍出现旧 uid %q", oldUID)
		}
	}
	if _, _, err := st.EmbyItemByUID(ctx, oldUID); err == nil {
		t.Errorf("改 N 后旧 uid %q 仍是播放单元（旧 id 必须失效）", oldUID)
	}
	// 新分块的第一块立刻就能按 uid 取到，成员就是第 1-5 集。
	item, members, err := st.EmbyItemByUID(ctx, after[0].UID)
	if err != nil {
		t.Fatalf("EmbyItemByUID(新 uid): %v", err)
	}
	if item.UID != after[0].UID || len(members) != 5 || members[0].EpNo != 1 || members[4].EpNo != 5 {
		t.Errorf("新 uid 的成员 = %+v（item=%+v）, 期望第 1-5 集", members, item)
	}
}

// AC-4：21 集、N=10 → 2 个合并块 + 1 个单集块（余数 1 集走单集规则）。
func TestListEmbyItems_RemainderSingle(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, ids := seedMergeDrama(t, st, 21)

	items, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("对外剧集数 = %d, 期望 3", len(items))
	}
	if items[0].Kind != "merged" || items[1].Kind != "merged" {
		t.Errorf("前两块应为 merged：%+v", items[:2])
	}
	last := items[2]
	if last.Kind != "single" {
		t.Errorf("余数 1 集的 kind = %q, 期望 single", last.Kind)
	}
	if want := singleUID(ids[20]); last.UID != want {
		t.Errorf("余数 1 集的 uid = %q, 期望 %q（e<episode.id>）", last.UID, want)
	}
	if last.EpisodeID != ids[20] || last.Title != "" {
		t.Errorf("余数单集的源集字段 = {%d, %q}, 期望 {%d, \"\"}", last.EpisodeID, last.Title, ids[20])
	}
	if last.EpFrom != 21 || last.EpTo != 21 || last.RuntimeSec != sumDurations(21, 21) {
		t.Errorf("余数块 = %+v, 期望覆盖第 21 集且 runtime 为该集时长", last)
	}
}

// AC：集数是 N 的整倍数时不多出空组。
func TestListEmbyItems_ExactMultiple(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, _ := seedMergeDrama(t, st, 20)

	items, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("20 集 / N=10 对外剧集数 = %d, 期望 2（不多空组）", len(items))
	}
	for i, it := range items {
		if it.Kind != "merged" || it.EpFrom != i*10+1 || it.EpTo != (i+1)*10 {
			t.Errorf("items[%d] = %+v, 期望 merged 覆盖第 %d-%d 集", i, it, i*10+1, (i+1)*10)
		}
	}
}

// AC：N=1 时每集各占一条，全部走单集规则（uid 是 e<episode.id>）。
func TestListEmbyItems_ChunkOneAllSingle(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, ids := seedMergeDrama(t, st, 3)
	setChunkSize(t, st, 1)

	items, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems(N=1): %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("3 集 / N=1 对外剧集数 = %d, 期望 3", len(items))
	}
	for i, it := range items {
		if it.Kind != "single" || it.UID != singleUID(ids[i]) || it.EpNo != i+1 {
			t.Errorf("items[%d] = {%s ep_no=%d %s}, 期望 {single ep_no=%d %s}",
				i, it.Kind, it.EpNo, it.UID, i+1, singleUID(ids[i]))
		}
	}
}

// AC：0 集 → 0 条对外剧集，不留空组。
func TestListEmbyItems_ZeroEpisodes(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, _ := seedMergeDrama(t, st, 0)

	items, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("0 集剧的对外剧集数 = %d, 期望 0：%+v", len(items), items)
	}
}

// AC-6：集集合与 N 不变时重复读，全部 uid 逐字符相同（不飘移）。
func TestListEmbyItems_Idempotent(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, _ := seedMergeDrama(t, st, 22)

	first, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("第一次 ListEmbyItems: %v", err)
	}
	second, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("第二次 ListEmbyItems: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("两次读数不同：%d vs %d", len(first), len(second))
	}
	for i := range first {
		a, b := first[i], second[i]
		if a.UID != b.UID || a.EpNo != b.EpNo || a.Kind != b.Kind || a.RuntimeSec != b.RuntimeSec {
			t.Errorf("行 %d 两次读不一致：%+v vs %+v", i, a, b)
		}
	}
}

// AC-3：同一 N 下 `e<id>` 稳定可播；改成 N>1 把它并进块后，旧 uid 立即 ErrNotFound
// （播放面据此回 404，不返回别的形态的字节）。
func TestEmbyItemByUID_SingleBecomesMerged(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	_, ids := seedMergeDrama(t, st, 3)
	setChunkSize(t, st, 1)

	uid := singleUID(ids[1])
	item, members, err := st.EmbyItemByUID(ctx, uid)
	if err != nil {
		t.Fatalf("N=1 时 EmbyItemByUID(%q): %v", uid, err)
	}
	if item.Kind != "single" || len(members) != 1 || members[0].ID != ids[1] {
		t.Errorf("单集单元 = %+v members=%+v, 期望第 2 集一条", item, members)
	}

	setChunkSize(t, st, 10) // 不重建、不同步
	if _, _, err := st.EmbyItemByUID(ctx, uid); err != ErrNotFound {
		t.Errorf("3 集并成一块后旧单集 uid %q 的错误 = %v, 期望 ErrNotFound", uid, err)
	}
}

// AC-3：uid 形态不认识、剧/集不存在都返回 ErrNotFound（不是 500、不是别人的内容）。
func TestEmbyItemByUID_RejectsUnknown(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, _ := seedMergeDrama(t, st, 3)

	bad := []string{"", "x1", "e", "e0", "e-1", "e999999", "m0_1_2", "m1_1", "m1_10_1",
		fmt.Sprintf("m%d_1_10", did)}
	for _, uid := range bad {
		if _, _, err := st.EmbyItemByUID(ctx, uid); err != ErrNotFound {
			t.Errorf("EmbyItemByUID(%q) 的错误 = %v, 期望 ErrNotFound", uid, err)
		}
	}
}

// AC：删剧后对外剧集也消失（没有派生表要清，直接读不到集）。
func TestDeleteDrama_DropsDerivedItems(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, ids := seedMergeDrama(t, st, 3)

	if err := st.DeleteDrama(ctx, did); err != nil {
		t.Fatalf("DeleteDrama: %v", err)
	}
	items, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("删剧后对外剧集数 = %d, 期望 0", len(items))
	}
	if _, _, err := st.EmbyItemByUID(ctx, singleUID(ids[0])); err != ErrNotFound {
		t.Errorf("删剧后旧 uid 的错误 = %v, 期望 ErrNotFound", err)
	}
}

// AC：刷新（并发 upsert）与读派生并存时不出现重复对外集号、不报错。
func TestConcurrentUpsertAndList(t *testing.T) {
	db := newCRUDTestDB(t)
	st := NewStore(db.DB)
	ctx := context.Background()
	const episodes = 6
	dramaID, _ := seedMergeDrama(t, st, episodes)
	setChunkSize(t, st, 1)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	const rounds = 10
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for round := 0; round < rounds; round++ {
			for i := 1; i <= episodes; i++ {
				if _, err := st.UpsertEpisode(ctx, Episode{
					DramaID: dramaID, EpNo: i, DurationSec: 100 + i, Source: "web", Vid: "v2",
				}); err != nil {
					errCh <- fmt.Errorf("upsert ep%d: %w", i, err)
					return
				}
			}
		}
	}()
	for round := 0; round < rounds; round++ {
		items, err := st.ListEmbyItems(ctx, dramaID)
		if err != nil {
			t.Fatalf("并发读派生: %v", err)
		}
		seen := make(map[int]bool, len(items))
		for _, it := range items {
			if seen[it.EpNo] {
				t.Fatalf("出现重复对外集号 %d（items=%+v）", it.EpNo, items)
			}
			seen[it.EpNo] = true
		}
		if len(items) != episodes {
			t.Fatalf("派生条目 = %d, 期望 %d", len(items), episodes)
		}
	}
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("并发写失败: %v", err)
	default:
	}
	if n := countRows(t, db.DB, "episode"); n != episodes {
		t.Fatalf("episode 行数 = %d, 期望 %d（并发 upsert 不得重复建集）", n, episodes)
	}
}

// AC：uid 形态的真值只有一处 —— 解析失败的几种形态都返回 ok=false。
func TestParseMergedUID(t *testing.T) {
	cases := []struct {
		uid  string
		ok   bool
		want string
	}{
		{"m12_1_10", true, "12:1:10"},
		{"m1_21_22", true, "1:21:22"},
		{"m1_1", false, ""},
		{"m1_1_10_2", false, ""},
		{"m0_1_10", false, ""},
		{"m1_0_10", false, ""},
		{"m1_10_1", false, ""},
		{"e1", false, ""},
		{"mabc_1_10", false, ""},
	}
	for _, c := range cases {
		dramaID, from, to, ok := ParseMergedUID(c.uid)
		if ok != c.ok {
			t.Errorf("ParseMergedUID(%q) ok = %v, 期望 %v", c.uid, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got := fmt.Sprintf("%d:%d:%d", dramaID, from, to); got != c.want {
			t.Errorf("ParseMergedUID(%q) = %s, 期望 %s", c.uid, got, c.want)
		}
	}
}

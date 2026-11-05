package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
// 走 ListEmbyItems 而不是直接调 deriveEmbyItems：这条路径证明「默认值从 settings
// 读、派生在读取时发生」，库里没有任何派生行。
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

// AC-2 / AC-3（MKACG-666 核心）：改 N 后下一次读取立即是新分块，不需要同步、
// 不需要重建；旧合并 uid 与「被并进块的单集 uid」当场失效（404 语义 = ErrNotFound）。
func TestListEmbyItems_ChangeChunkSizeImmediate(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, ids := seedMergeDrama(t, st, 22)
	setChunkSize(t, st, 10)

	before, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems(N=10): %v", err)
	}
	if len(before) != 3 {
		t.Fatalf("N=10 时对外剧集数 = %d, 期望 3", len(before))
	}
	oldUID := before[0].UID
	// 单集被并进合并块：它的 e<id> 不是播放单元。
	if eps, err := st.PlaybackEpisodes(ctx, singleUID(ids[0])); !errors.Is(err, ErrNotFound) {
		t.Fatalf("N=10 时被合并的 e%d 仍可播放（eps=%d err=%v）", ids[0], len(eps), err)
	}

	// 只改配置，不做任何同步/重建。
	setChunkSize(t, st, 5)
	after, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems(N=5): %v", err)
	}
	if len(after) != 5 {
		t.Fatalf("N=5 时对外剧集数 = %d, 期望 5（改配置必须立即生效）：%+v", len(after), after)
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
	if _, err := st.PlaybackEpisodes(ctx, oldUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("改 N 后旧合并 uid %q 仍是播放单元（err=%v）", oldUID, err)
	}
	// 新分块下的合并 uid 立即可播。
	eps, err := st.PlaybackEpisodes(ctx, mergedUID(did, 1, 5))
	if err != nil {
		t.Fatalf("PlaybackEpisodes(新块): %v", err)
	}
	if len(eps) != 5 || eps[0].EpNo != 1 || eps[4].EpNo != 5 {
		t.Fatalf("新块成员 = %+v, 期望第 1-5 集", eps)
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
		t.Errorf("余数单集 = %+v, 期望带 EpisodeID 且标题取自源集", last)
	}
	if last.EpFrom != 21 || last.EpTo != 21 || last.RuntimeSec != sumDurations(21, 21) {
		t.Errorf("余数块 = %+v, 期望覆盖第 21 集且 runtime 为该集时长", last)
	}
	// 余数单集是真正的播放单元。
	if eps, err := st.PlaybackEpisodes(ctx, last.UID); err != nil || len(eps) != 1 || eps[0].ID != ids[20] {
		t.Fatalf("PlaybackEpisodes(余数单集) = %+v, err=%v", eps, err)
	}
}

// AC：集数是 N 的整倍数时不多出空组。
func TestListEmbyItems_ExactMultiple(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, _ := seedMergeDrama(t, st, 20)

	items, err := st.ListEmbyItems(ctx, did)
	if err != nil {
		t.Fatalf("ListEmbyItems(N=10): %v", err)
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
		if _, err := st.PlaybackEpisodes(ctx, it.UID); err != nil {
			t.Errorf("N=1 时单集 %s 不是播放单元: %v", it.UID, err)
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

// AC-3：集集合与 N 不变时重复读取，全部 uid 逐字符相同（不飘移）。
func TestListEmbyItems_StableUIDs(t *testing.T) {
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
		t.Fatalf("两次读取条数不同：%d vs %d", len(first), len(second))
	}
	for i := range first {
		a, b := first[i], second[i]
		if a.UID != b.UID || a.EpNo != b.EpNo || a.Kind != b.Kind || a.RuntimeSec != b.RuntimeSec {
			t.Errorf("行 %d 两次读取不一致：%+v vs %+v", i, a, b)
		}
	}
}

// AC-3：uid 形态不认识、区间对不上当前分块、集不存在 → 一律 ErrNotFound。
func TestPlaybackEpisodes_RejectsUnknownUID(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newCRUDTestDB(t).DB)
	did, ids := seedMergeDrama(t, st, 22)

	cases := []string{
		"",                     // 空
		"x1",                   // 未知前缀
		"e",                    // 缺 id
		"eabc",                 // 非数字
		"e9999999",             // 集不存在
		"m1_1",                 // 段数不足
		"m1_1_10_2",            // 段数过多
		"m0_1_10",              // 剧 id 非正
		"m1_10_1",              // 区间倒置
		mergedUID(did, 3, 9),   // 与任何块边界都不一致
		mergedUID(did, 21, 21), // 单集区间不能冒充合并集
	}
	for _, uid := range cases {
		if eps, err := st.PlaybackEpisodes(ctx, uid); !errors.Is(err, ErrNotFound) {
			t.Errorf("PlaybackEpisodes(%q) = (%d 集, %v), 期望 ErrNotFound", uid, len(eps), err)
		}
	}
	// 合法合并集可展开，成员按播放顺序。
	eps, err := st.PlaybackEpisodes(ctx, mergedUID(did, 11, 20))
	if err != nil {
		t.Fatalf("PlaybackEpisodes(m1_11_20): %v", err)
	}
	if len(eps) != 10 || eps[0].ID != ids[10] || eps[9].ID != ids[19] {
		t.Fatalf("合并集成员 = %+v, 期望 ids[10]..ids[19]", eps)
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

// AC：并发刷新与并发读取不产生重复对外集号。
//
// 读出路径只读、不写库：它必须能在 upsert 事务提交的任意时刻给出「当前」派生结果，
// 且同一集在结果里只出现一次。这里让一条写路径与两条读路径交错多轮。
func TestConcurrentUpsertAndList(t *testing.T) {
	db := newCRUDTestDB(t)
	st := NewStore(db.DB)
	ctx := context.Background()
	const episodes = 6
	dramaID, _ := seedMergeDrama(t, st, episodes)
	setChunkSize(t, st, 1) // N=1 让每条对外剧集对应一集，重复行才看得见

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	const rounds = 10
	errCh := make(chan error, 3)
	// 写路径：刷新同一部剧的每一集（同剧并发 upsert）。
	go func() {
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
		errCh <- nil
	}()
	// 读路径：两个并发读取。
	for i := 0; i < 2; i++ {
		go func() {
			for round := 0; round < rounds; round++ {
				if _, err := st.ListEmbyItems(ctx, dramaID); err != nil {
					errCh <- fmt.Errorf("list: %w", err)
					return
				}
			}
			errCh <- nil
		}()
	}
	for i := 0; i < 3; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("并发访问失败: %v", err)
		}
	}

	items, err := st.ListEmbyItems(ctx, dramaID)
	if err != nil {
		t.Fatalf("ListEmbyItems: %v", err)
	}
	seen := make(map[int]bool, len(items))
	for _, it := range items {
		if seen[it.EpNo] {
			t.Fatalf("出现重复对外集号 %d（items=%+v）", it.EpNo, items)
		}
		seen[it.EpNo] = true
	}
	if len(items) != episodes {
		t.Fatalf("对外剧集数 = %d, 期望 %d", len(items), episodes)
	}
	if n := countRows(t, db.DB, "episode"); n != episodes {
		t.Fatalf("episode 行数 = %d, 期望 %d（并发 upsert 不得重复建集）", n, episodes)
	}
}

// TestDeriveEmbyItems_RuntimeUnknown 钉住 MKACG-689：未知时长不得当作完整的 0。
// 合并块里还有没探测过的成员时，RuntimeSec 只是已知部分之和，RuntimeUnknown 记这些集数。
func TestDeriveEmbyItems_RuntimeUnknown(t *testing.T) {
	eps := []Episode{
		{ID: 1, DramaID: 7, EpNo: 1, DurationSec: 0},
		{ID: 2, DramaID: 7, EpNo: 2, DurationSec: 50},
		{ID: 3, DramaID: 7, EpNo: 3, DurationSec: 0},
	}
	merged := deriveEmbyItems(7, eps, 3)
	if len(merged) != 1 {
		t.Fatalf("对外剧集数 = %d, 期望 1", len(merged))
	}
	if got := merged[0]; got.RuntimeSec != 50 || got.RuntimeUnknown != 2 {
		t.Errorf("合并集 = (runtime=%d, unknown=%d), 期望 (50, 2)", got.RuntimeSec, got.RuntimeUnknown)
	}

	// 所有成员都已知时 unknown 归零，RuntimeSec 就是完整总时长。
	complete := deriveEmbyItems(7, []Episode{
		{ID: 1, EpNo: 1, DurationSec: 40},
		{ID: 2, EpNo: 2, DurationSec: 60},
	}, 10)
	if got := complete[0]; got.RuntimeSec != 100 || got.RuntimeUnknown != 0 {
		t.Errorf("完整合并集 = (runtime=%d, unknown=%d), 期望 (100, 0)", got.RuntimeSec, got.RuntimeUnknown)
	}

	// N=1 时未知集自成一条单集，它的 Unknown 同样为 1（NFO 也因此不写 runtime）。
	singles := deriveEmbyItems(7, eps, 1)
	if len(singles) != 3 {
		t.Fatalf("N=1 对外剧集数 = %d, 期望 3", len(singles))
	}
	for i, wantUnknown := range []int{1, 0, 1} {
		if singles[i].RuntimeUnknown != wantUnknown {
			t.Errorf("N=1 items[%d] unknown = %d, 期望 %d", i, singles[i].RuntimeUnknown, wantUnknown)
		}
	}
}

// TestDeriveEmbyItems_PerEpisodeRuntimeIndependentOfChunkSize 钉住 MKACG-689 AC-4：
// 单集时长口径与 merge_chunk_size 无关 —— N=1 的单集时长之和等于 N=10 的合并集总时长。
func TestDeriveEmbyItems_PerEpisodeRuntimeIndependentOfChunkSize(t *testing.T) {
	eps := []Episode{
		{ID: 1, DramaID: 9, EpNo: 1, DurationSec: 100},
		{ID: 2, DramaID: 9, EpNo: 2, DurationSec: 200},
		{ID: 3, DramaID: 9, EpNo: 3, DurationSec: 300},
	}

	perEpisodeTotal := 0
	for _, it := range deriveEmbyItems(9, eps, 1) {
		if it.Kind != "single" {
			t.Fatalf("N=1 出现非单集 %+v", it)
		}
		perEpisodeTotal += it.RuntimeSec
	}

	merged := deriveEmbyItems(9, eps, 10)
	if len(merged) != 1 {
		t.Fatalf("N=10 对外剧集数 = %d, 期望 1", len(merged))
	}
	if merged[0].RuntimeSec != perEpisodeTotal {
		t.Errorf("N=10 合并集总时长 = %d, N=1 单集之和 = %d, 期望一致",
			merged[0].RuntimeSec, perEpisodeTotal)
	}
}

package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// EmbyItem 是一条对外剧集：Emby 看到的一集，单集或按当前 merge_chunk_size
// 自动分块后的合并块。
//
// 它不是表行。库里只存原始 episode，对外形态在每次读取时由 deriveEmbyItems
// 按当前 settings 现算（docs/adr/0014），因此改 merge_chunk_size 立刻影响
// 对外集号、文件名与播放单元，不需要同步、不需要重建。uid 是 `/video/{id}`
// 里 `{id}` 的 uid 段：单集 `e<episode.id>`、合并集
// `m<dramaID>_<epFrom>_<epTo>`（两个空间互不匹配，docs/adr/0010）。
type EmbyItem struct {
	DramaID    int64
	Season     int
	EpNo       int
	Kind       string // single | merged
	UID        string
	RuntimeSec int
	// EpisodeID 是单集对应的源集主键；合并集为 0。
	EpisodeID int64
	// EpFrom / EpTo 是这条对外剧集覆盖的源集号：单集两者都等于自身 ep_no，
	// 合并集是块内最小与最大 ep_no（渲染「第 A-B 集」标签用）。
	EpFrom int
	EpTo   int
	// Title 是源集标题（仅单集有值）；合并集标签由 EpFrom/EpTo 派生「第 A-B 集」。
	Title string
}

// chunkEpisodes 把按 ep_no 升序的集列表切成连续块，每块至多 n 集（纯函数）。
//
// 切的是**列表切片**而不是 `ep_no` 数值区间：库里集号可能有缺号（源侧下架不删行），
// 数值区间会切出空块或让某块只剩 1 集。n ≤ 0 时按 1 处理，只为不除零 panic；
// 注册表（settings 的 merge_chunk_size）已经保证 n ∈ [1,500]。
//
// 返回值是入参的子切片（不拷贝）：调用方只读，不要就地改写返回的块。
func chunkEpisodes(eps []Episode, n int) [][]Episode {
	if n <= 0 {
		n = 1
	}
	blocks := make([][]Episode, 0, (len(eps)+n-1)/n)
	for start := 0; start < len(eps); start += n {
		blocks = append(blocks, eps[start:min(start+n, len(eps))])
	}
	return blocks
}

// deriveEmbyItems 把一部剧的原始集按分块大小派生成对外剧集（纯函数）。
//
// 唯一派生实现：ListEmbyItems（vfs 文件名 / NFO）与 PlaybackEpisodes（播放单元）
// 都走这里，因此「对外集号、文件名、播放列表」永远来自同一份规则（硬约束 6）。
//
// 派生规则（docs/adr/0010）：episode 按 ep_no 升序，每 n 集切一块，第 i 块占
// 对外集号 i；块内 1 集是单集、≥2 集是合并集。0 集 → 0 条，不留空组。
// uid 只由 (episode.id) 或 (dramaID, epFrom, epTo) 派生，不含自增主键。
// runtime_sec 单集取该集时长、合并集取块内时长之和。
func deriveEmbyItems(dramaID int64, eps []Episode, n int) []EmbyItem {
	blocks := chunkEpisodes(eps, n)
	out := make([]EmbyItem, 0, len(blocks))
	for i, block := range blocks {
		it := EmbyItem{
			DramaID: dramaID,
			Season:  1,
			EpNo:    i + 1,
			EpFrom:  block[0].EpNo,
			EpTo:    block[len(block)-1].EpNo,
		}
		for _, ep := range block {
			it.RuntimeSec += ep.DurationSec
		}
		if len(block) == 1 {
			it.Kind = "single"
			it.UID = singleUID(block[0].ID)
			it.EpisodeID = block[0].ID
			it.Title = block[0].Title
		} else {
			it.Kind = "merged"
			it.UID = mergedUID(dramaID, it.EpFrom, it.EpTo)
		}
		out = append(out, it)
	}
	return out
}

// mergeChunkSize 现读 settings 的 merge_chunk_size（每次派生都读，不缓存）。
//
// 库中无行取注册表默认值，所以读的是 settingValue 而不是 Settings —— 后者持有
// *sql.DB，而事务内的 Store 只有 dbtx。校验规则仍只有 specFor 一份。
func (s *Store) mergeChunkSize(ctx context.Context) (int, error) {
	value, err := settingValue(ctx, s.db, KeyMergeChunkSize)
	if err != nil {
		return 0, fmt.Errorf("读 merge_chunk_size: %w", err)
	}
	n, ok := value.(int)
	if !ok {
		return 0, fmt.Errorf("%w: merge_chunk_size 不是整型", ErrInvalidSetting)
	}
	return n, nil
}

// ListEmbyItems 返回某部剧的对外剧集，按对外集号升序。
//
// 每次调用都现读 merge_chunk_size 再派生：库里没有派生行可查，所以改 N 之后
// 下一次读取就是新分块（docs/adr/0014）。vfs 渲染文件名与 NFO 都用它，因此这是
// 「对外集号由派生规则决定」的唯一入口（硬约束 6）。
func (s *Store) ListEmbyItems(ctx context.Context, dramaID int64) ([]EmbyItem, error) {
	n, err := s.mergeChunkSize(ctx)
	if err != nil {
		return nil, err
	}
	eps, err := s.ListEpisodes(ctx, dramaID)
	if err != nil {
		return nil, err
	}
	return deriveEmbyItems(dramaID, eps, n), nil
}

// DramaBlocks 返回某部剧的原始集按**当前** merge_chunk_size 切好的块（每块按 ep_no 升序）。
//
// 只是 chunkEpisodes 的导出入口：分块只有一份实现（与 deriveEmbyItems /
// PlaybackEpisodes 同一套规则，硬约束 6），预取需要「当前块 / 下一块 / 其余块」，
// 不能自己再按 merge_chunk_size 切一遍（docs/adr/0017-prefetch-and-stream-health.md）。
// 返回值是 ListEpisodes 结果切出的子切片，调用方只读。
func (s *Store) DramaBlocks(ctx context.Context, dramaID int64) ([][]Episode, error) {
	n, err := s.mergeChunkSize(ctx)
	if err != nil {
		return nil, err
	}
	eps, err := s.ListEpisodes(ctx, dramaID)
	if err != nil {
		return nil, err
	}
	return chunkEpisodes(eps, n), nil
}

// PlaybackEpisodes 把一个播放单元 uid 展开成成员集（按播放顺序）。
//
// 「合法 uid」由当前派生规则决定，不再查任何派生表（docs/adr/0014）：
//   - 合并集 `m<dramaID>_<epFrom>_<epTo>`：现读 N、取该剧全部集、切块，必须有一块
//     的集号区间与 uid 完全一致且块内 ≥2 集；
//   - 单集 `e<episode.id>`：该集必须存在，且在当前 N 下自成一块（块内 1 集）。
//
// 某集被并进合并块后它的 `e<id>` 立刻不是播放单元，改 N 后旧合并 uid 也不再匹配，
// 一律返回 ErrNotFound（契约「不飘移」第 3 条：旧 URL 得到 404 而不是播到别的形态）。
func (s *Store) PlaybackEpisodes(ctx context.Context, uid string) ([]Episode, error) {
	dramaID, epFrom, epTo, merged := ParseMergedUID(uid)
	var episodeID int64
	if !merged {
		id, ok := parseUIDNumber(uid, "e")
		if !ok {
			return nil, ErrNotFound
		}
		episodeID = id
		ep, err := s.EpisodeByID(ctx, episodeID)
		if err != nil {
			return nil, err
		}
		dramaID = ep.DramaID
	}

	n, err := s.mergeChunkSize(ctx)
	if err != nil {
		return nil, err
	}
	eps, err := s.ListEpisodes(ctx, dramaID)
	if err != nil {
		return nil, err
	}
	for _, block := range chunkEpisodes(eps, n) {
		if merged {
			if len(block) >= 2 && block[0].EpNo == epFrom && block[len(block)-1].EpNo == epTo {
				return block, nil
			}
			continue
		}
		if len(block) == 1 && block[0].ID == episodeID {
			return block, nil
		}
	}
	return nil, ErrNotFound
}

// singleUID 是单集句柄：`e` + episode 主键十进制（与 internal/stream 的 uid 规则一致）。
func singleUID(episodeID int64) string {
	return "e" + strconv.FormatInt(episodeID, 10)
}

// mergedUID 是合并集句柄：`m<dramaID>_<epFrom>_<epTo>`。
//
// 只由剧 id 与块成员集号区间派生，不含自增主键：集集合与 N 不变时逐字符不变
// （PLAYBACK-CONTRACT「不飘移」）。分隔符是 `_` 而不是 `-` —— 播放 id 是
// `<uid>-<签名>`，stream.ParseID 按首个 `-` 切，uid 里带 `-` 会把 404 顶成 401。
func mergedUID(dramaID int64, epFrom, epTo int) string {
	return "m" + strconv.FormatInt(dramaID, 10) + "_" + strconv.Itoa(epFrom) + "_" + strconv.Itoa(epTo)
}

// ParseMergedUID 解析合并集句柄 `m<dramaID>_<epFrom>_<epTo>`。
//
// 形态真值在这里：store 用它判断播放单元是不是合并集，internal/segment 用它把
// 播放单元展开成成员集。不匹配、剧 id 非正、区间倒置都返回 ok=false。
func ParseMergedUID(uid string) (dramaID int64, epFrom, epTo int, ok bool) {
	rest, ok := strings.CutPrefix(uid, "m")
	if !ok {
		return 0, 0, 0, false
	}
	parts := strings.Split(rest, "_")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	dramaID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || dramaID <= 0 {
		return 0, 0, 0, false
	}
	epFrom, err = strconv.Atoi(parts[1])
	if err != nil || epFrom <= 0 {
		return 0, 0, 0, false
	}
	epTo, err = strconv.Atoi(parts[2])
	if err != nil || epTo < epFrom {
		return 0, 0, 0, false
	}
	return dramaID, epFrom, epTo, true
}

// parseUIDNumber 解析 `<prefix><十进制>` 形式的 uid，返回其中的数值。
func parseUIDNumber(uid, prefix string) (int64, bool) {
	rest, ok := strings.CutPrefix(uid, prefix)
	if !ok || rest == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

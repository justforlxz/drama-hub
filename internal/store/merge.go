package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// EmbyItem 是 Emby 看到的一条剧集（单集或自动分块后的合并集）。
//
// 它**不是表行**：库里只存原始集（`episode`）与当前配置（`settings.merge_chunk_size`），
// 对外剧集在每次读到时现算（ADR-0014，硬约束 6）。因此 ID 不存在——派生没有任何自增
// 主键可挂，uid 是 `/video/{id}` 里 `{id}` 的 uid 段：单集 `e<episode.id>`、
// 合并集 `m<dramaID>_<epFrom>_<epTo>`（两个空间互不匹配，docs/adr/0010）。
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

// embyItems 是「对外剧集」的唯一派生实现（纯函数）：把按 ep_no 升序的原始集按 n 切块，
// 同时返回每条的成员集（播放面展开用，省掉一次区间查询）。
//
// 派生规则（docs/adr/0010）：第 i 块占对外集号 i，块内 1 集是单集、≥2 集是合并集。
// 0 集 → 0 条。uid 生成规则只在这里：单集 `e<episode.id>`、合并集
// `m<dramaID>_<epFrom>_<epTo>`；runtime_sec 单集取该集时长、合并集取块内时长之和。
func embyItems(dramaID int64, eps []Episode, n int) ([]EmbyItem, [][]Episode) {
	blocks := chunkEpisodes(eps, n)
	items := make([]EmbyItem, 0, len(blocks))
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
		items = append(items, it)
	}
	return items, blocks
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

// ListEmbyItems 返回某部剧**当前配置下**的对外剧集，按对外集号升序。
//
// 每次调用现算：读该剧 `episode`（一次查询）+ 现读 `merge_chunk_size`（一次点查），
// 再用 embyItems 切块。改配置后下一次读就是新分块——没有派生表、没有写库、不需要
// 同步或重建（ADR-0014）。
//
// vfs 渲染文件名与 NFO 都用它，因此这是「对外集号只由原始集 + 当前 N 决定」的唯一入口
// （硬约束 6）。
func (s *Store) ListEmbyItems(ctx context.Context, dramaID int64) ([]EmbyItem, error) {
	n, err := s.mergeChunkSize(ctx)
	if err != nil {
		return nil, err
	}
	eps, err := s.ListEpisodes(ctx, dramaID)
	if err != nil {
		return nil, err
	}
	items, _ := embyItems(dramaID, eps, n)
	return items, nil
}

// EmbyItemByUID 按 uid 查一条**当前**对外剧集，并返回它的成员集（按播放顺序）。
//
// 播放面的 `/video/{id}` 用它判断「这个播放单元现在还在不在」：uid 必须是当前配置下
// 某一块的句柄（docs/PLAYBACK-CONTRACT.md「不飘移」第 3 条 —— 改 N 之后旧 uid 立刻
// 不再是播放单元，旧 URL 得到 404 而不是播到别的形态）。无此单元返回 ErrNotFound。
//
// 查询次数是常数、与集数无关：合并 uid 2 次（该剧集列表 + N），单集 uid 3 次
// （episode 行取 drama_id + 上面两次）。成员集一并返回，调用方不必再查一次区间。
func (s *Store) EmbyItemByUID(ctx context.Context, uid string) (EmbyItem, []Episode, error) {
	dramaID, err := s.dramaIDOfUID(ctx, uid)
	if err != nil {
		return EmbyItem{}, nil, err
	}
	n, err := s.mergeChunkSize(ctx)
	if err != nil {
		return EmbyItem{}, nil, err
	}
	eps, err := s.ListEpisodes(ctx, dramaID)
	if err != nil {
		return EmbyItem{}, nil, err
	}
	items, blocks := embyItems(dramaID, eps, n)
	for i := range items {
		if items[i].UID == uid {
			return items[i], blocks[i], nil
		}
	}
	return EmbyItem{}, nil, ErrNotFound
}

// dramaIDOfUID 解出 uid 所属的剧：合并集 uid 自带剧 id，单集 `e<episode.id>` 要查一次
// episode 行。形态不合法或该集/剧不存在都返回 ErrNotFound。
func (s *Store) dramaIDOfUID(ctx context.Context, uid string) (int64, error) {
	if dramaID, _, _, ok := ParseMergedUID(uid); ok {
		return dramaID, nil
	}
	episodeID, ok := parseUIDNumber(uid, "e")
	if !ok {
		return 0, ErrNotFound
	}
	var dramaID int64
	err := s.db.QueryRowContext(ctx, `SELECT drama_id FROM episode WHERE id = $1`, episodeID).Scan(&dramaID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("查 episode %d 所属剧目: %w", episodeID, err)
	}
	return dramaID, nil
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
// 形态真值在这里：store 用它解出剧 id 与成员区间，internal/segment 用它把播放单元
// 展开成成员集（`ep_no ∈ [epFrom, epTo]`，列表连续切片的性质保证区间内没有别块的集）。
// 不匹配、剧 id 非正、区间倒置都返回 ok=false。
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

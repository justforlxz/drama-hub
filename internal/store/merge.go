package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// MergeGroup 是 merge_group 表的一行：一部剧里被合成一条对外剧集的若干集。
type MergeGroup struct {
	ID      int64
	DramaID int64
	Label   string
}

// MergeMember 是合并组内的一个成员：第 Ord 位放的是 EpisodeID，其源集号是 EpNo。
//
// Ord 从 1 起、连续；成员顺序就是合并集时间轴上的顺序。
type MergeMember struct {
	EpisodeID int64
	Ord       int
	EpNo      int
}

// EmbyItem 是 emby_item 派生表的一行：Emby 看到的一条剧集（单集或合并集）。
//
// 派生表由 RebuildEmbyItems 幂等重建，uid 是 `/video/{id}` 里 `{id}` 的 uid 段：
// 单集 `e<episode.id>`、合并集 `m<merge_group.id>`（两个空间互不匹配，DESIGN D3）。
type EmbyItem struct {
	ID         int64
	DramaID    int64
	Season     int
	EpNo       int
	Kind       string // single | merged
	UID        string
	RuntimeSec int
	// EpisodeID 是单集对应的源集主键；合并集为 0。
	EpisodeID int64
	// EpFrom / EpTo 是这条对外剧集覆盖的源集号：单集两者都等于自身 ep_no，
	// 合并集是组内最小与最大 ep_no（渲染「第 A-B 集」标签用）。
	EpFrom int
	EpTo   int
	// Title 是源集标题（仅单集有值）；合并集标签由 EpFrom/EpTo 派生「第 A-B 集」。
	Title string
}

// mergeGroupColumns 是 merge_group 表的显式列清单，顺序必须与 scanMergeGroup 一致。
const mergeGroupColumns = `SELECT id, drama_id, label FROM merge_group`

// scanMergeGroup 按 mergeGroupColumns 的顺序读出一行。
func scanMergeGroup(scan func(dest ...any) error) (MergeGroup, error) {
	var g MergeGroup
	if err := scan(&g.ID, &g.DramaID, &g.Label); err != nil {
		return MergeGroup{}, err
	}
	return g, nil
}

// ListMergeGroups 返回某部剧的全部合并组，按 id 升序。
func (s *Store) ListMergeGroups(ctx context.Context, dramaID int64) ([]MergeGroup, error) {
	rows, err := s.db.QueryContext(ctx, mergeGroupColumns+` WHERE drama_id = $1 ORDER BY id`, dramaID)
	if err != nil {
		return nil, fmt.Errorf("列合并组 drama_id=%d: %w", dramaID, err)
	}
	defer rows.Close()

	out := make([]MergeGroup, 0)
	for rows.Next() {
		g, err := scanMergeGroup(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("读取合并组行: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历合并组 drama_id=%d: %w", dramaID, err)
	}
	return out, nil
}

// MergeGroupByID 按主键查一个合并组。无行返回 ErrNotFound。
func (s *Store) MergeGroupByID(ctx context.Context, id int64) (MergeGroup, error) {
	row := s.db.QueryRowContext(ctx, mergeGroupColumns+` WHERE id = $1`, id)
	g, err := scanMergeGroup(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return MergeGroup{}, ErrNotFound
	}
	if err != nil {
		return MergeGroup{}, fmt.Errorf("查合并组 id=%d: %w", id, err)
	}
	return g, nil
}

// ListMergeMembers 返回合并组的成员，按 Ord 升序（即合并集时间轴顺序）。
//
// 成员行与 episode 联表读回源集号，渲染「第 A-B 集」与排序都需要它。
// 组不存在或没有成员都返回空切片。
func (s *Store) ListMergeMembers(ctx context.Context, groupID int64) ([]MergeMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT mi.episode_id, mi.ord, e.ep_no
		 FROM merge_item mi JOIN episode e ON e.id = mi.episode_id
		 WHERE mi.group_id = $1 ORDER BY mi.ord`, groupID)
	if err != nil {
		return nil, fmt.Errorf("列合并组成员 group_id=%d: %w", groupID, err)
	}
	defer rows.Close()

	out := make([]MergeMember, 0)
	for rows.Next() {
		var m MergeMember
		if err := rows.Scan(&m.EpisodeID, &m.Ord, &m.EpNo); err != nil {
			return nil, fmt.Errorf("读取合并组成员行: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历合并组成员 group_id=%d: %w", groupID, err)
	}
	return out, nil
}

// CreateMergeGroup 新建一个合并组，按给定顺序写入成员（ord 从 1 起），并在同一
// 事务内重建该剧的派生表（合并变更 = 派生表重建的调用点之一）。
//
// episodeIDs 必须至少两集、互不重复、都属于 dramaID、且不与同剧其它合并组重叠
// （重叠会让时间轴出现重复播放的内容，属非法编排）。任一不满足返回 ErrInvalid。
// 返回新组 id。
func (s *Store) CreateMergeGroup(ctx context.Context, dramaID int64, label string, episodeIDs []int64) (int64, error) {
	if label == "" {
		return 0, fmt.Errorf("%w: 合并组 label 不能为空", ErrInvalid)
	}
	if len(episodeIDs) < 2 {
		return 0, fmt.Errorf("%w: 合并组至少需要 2 集，实际 %d 集", ErrInvalid, len(episodeIDs))
	}

	var groupID int64
	err := s.InTx(ctx, func(tx *Store) error {
		// 锁序统一从 drama 开始（DEV-STANDARDS「数据库」）：先拿锁，再做归属校验与写入。
		if err := tx.lockDrama(ctx, dramaID); err != nil {
			return err
		}
		eps, err := tx.ListEpisodes(ctx, dramaID)
		if err != nil {
			return err
		}
		owned := make(map[int64]struct{}, len(eps))
		for _, ep := range eps {
			owned[ep.ID] = struct{}{}
		}

		seen := make(map[int64]struct{}, len(episodeIDs))
		for _, id := range episodeIDs {
			if _, dup := seen[id]; dup {
				return fmt.Errorf("%w: 剧集 %d 在同一合并组里出现两次", ErrInvalid, id)
			}
			seen[id] = struct{}{}
			if _, ok := owned[id]; !ok {
				return fmt.Errorf("%w: 剧集 %d 不属于剧 %d", ErrInvalid, id, dramaID)
			}
		}

		groups, err := tx.ListMergeGroups(ctx, dramaID)
		if err != nil {
			return err
		}
		for _, g := range groups {
			members, err := tx.ListMergeMembers(ctx, g.ID)
			if err != nil {
				return err
			}
			for _, m := range members {
				if _, clash := seen[m.EpisodeID]; clash {
					return fmt.Errorf("%w: 剧集 %d 已在合并组 %d（%s）里", ErrInvalid, m.EpisodeID, g.ID, g.Label)
				}
			}
		}

		if err := tx.db.QueryRowContext(ctx,
			`INSERT INTO merge_group(drama_id, label) VALUES($1, $2) RETURNING id`,
			dramaID, label,
		).Scan(&groupID); err != nil {
			return wrapWriteErr("写入合并组", err)
		}
		for i, id := range episodeIDs {
			if _, err := tx.db.ExecContext(ctx,
				`INSERT INTO merge_item(group_id, episode_id, ord) VALUES($1, $2, $3)`,
				groupID, id, i+1,
			); err != nil {
				return wrapWriteErr("写入合并组成员", err)
			}
		}
		return tx.rebuildEmbyItems(ctx, dramaID)
	})
	if err != nil {
		return 0, err
	}
	return groupID, nil
}

// DeleteMergeGroup 删除一个合并组（成员级联删除），并在同一事务内重建该剧的
// 派生表。无该组返回 ErrNotFound。
func (s *Store) DeleteMergeGroup(ctx context.Context, id int64) error {
	return s.InTx(ctx, func(tx *Store) error {
		g, err := tx.MergeGroupByID(ctx, id)
		if err != nil {
			return err
		}
		// 锁序统一从 drama 开始：先锁剧目，再删合并组（与重建、回填、同步同一顺序）。
		if err := tx.lockDrama(ctx, g.DramaID); err != nil {
			return err
		}
		res, err := tx.db.ExecContext(ctx, `DELETE FROM merge_group WHERE id = $1`, id)
		if err != nil {
			return wrapWriteErr("删除合并组", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("删除合并组 id=%d: %w", id, err)
		} else if n == 0 {
			return ErrNotFound
		}
		return tx.rebuildEmbyItems(ctx, g.DramaID)
	})
}

// RebuildEmbyItems 幂等重建某部剧的对外剧集派生表，并返回重建后的行。
//
// 派生规则（DESIGN D5）：合并组占 1 个对外集号、覆盖其全部成员；其余每集各占
// 1 个；对外集号从 1 起连续，顺序按源集号（合并集取其组内最小源集号）升序。
// 重建语义是「整体替换」：先删该剧全部 emby_item 行再插入，重复调用结果一致。
//
// uid 生成规则只在 rebuildEmbyItems 一处：单集 `e<episode.id>`、合并集
// `m<merge_group.id>`；runtime_sec 单集取该集时长、合并集取组内时长之和。
//
// 根 Store 调用时自带事务；事务内 Store 调用时沿用外层事务（同步完成是第二个
// 调用点，它需要与写剧集同一事务）。
func (s *Store) RebuildEmbyItems(ctx context.Context, dramaID int64) ([]EmbyItem, error) {
	if s.root == nil {
		if err := s.rebuildEmbyItems(ctx, dramaID); err != nil {
			return nil, err
		}
		return s.ListEmbyItems(ctx, dramaID)
	}
	var out []EmbyItem
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.rebuildEmbyItems(ctx, dramaID); err != nil {
			return err
		}
		items, err := tx.ListEmbyItems(ctx, dramaID)
		out = items
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// outwardItem 是重建过程中的中间态：一条对外剧集及其排序键。
type outwardItem struct {
	kind      string
	uid       string
	episodeID int64
	epFrom    int
	epTo      int
	runtime   int
	sortKey   int
}

// rebuildEmbyItems 是 RebuildEmbyItems 的事务内实现（只碰 s.db，不开关事务）。
//
// 两条约束都在 lockDrama：
//   - 同一部剧的「先删后插」必须串行。READ COMMITTED 下两个并发重建会各自按自己的
//     快照删（删不到对方刚插入的行），再各自插入同一批 uid → 唯一约束冲突；
//   - 这把锁同时是全局锁序（drama → 子表）的入口，锁在调用方的事务结束时释放，
//     所以所有调用点都必须已经在事务里（CreateMergeGroup / DeleteMergeGroup /
//     RebuildEmbyItems / BackfillEpisodeDuration）。
func (s *Store) rebuildEmbyItems(ctx context.Context, dramaID int64) error {
	if err := s.lockDrama(ctx, dramaID); err != nil {
		return err
	}
	eps, err := s.ListEpisodes(ctx, dramaID)
	if err != nil {
		return err
	}
	groups, err := s.ListMergeGroups(ctx, dramaID)
	if err != nil {
		return err
	}

	duration := make(map[int64]int, len(eps))
	for _, ep := range eps {
		duration[ep.ID] = ep.DurationSec
	}
	inGroup := make(map[int64]struct{}, len(eps))
	items := make([]outwardItem, 0, len(eps))
	for _, g := range groups {
		members, err := s.ListMergeMembers(ctx, g.ID)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			continue // 空组不产出一条「零集」的对外剧集
		}
		runtime := 0
		from, to := members[0].EpNo, members[0].EpNo
		for _, m := range members {
			inGroup[m.EpisodeID] = struct{}{}
			runtime += duration[m.EpisodeID]
			if m.EpNo < from {
				from = m.EpNo
			}
			if m.EpNo > to {
				to = m.EpNo
			}
		}
		items = append(items, outwardItem{
			kind: "merged", uid: mergedUID(g.ID), epFrom: from, epTo: to,
			runtime: runtime, sortKey: from,
		})
	}
	for _, ep := range eps {
		if _, ok := inGroup[ep.ID]; ok {
			continue
		}
		items = append(items, outwardItem{
			kind: "single", uid: singleUID(ep.ID), episodeID: ep.ID,
			epFrom: ep.EpNo, epTo: ep.EpNo, runtime: ep.DurationSec, sortKey: ep.EpNo,
		})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].sortKey < items[j].sortKey })

	if _, err := s.db.ExecContext(ctx, `DELETE FROM emby_item WHERE drama_id = $1`, dramaID); err != nil {
		return wrapWriteErr("清空派生表", err)
	}
	for i, it := range items {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO emby_item(drama_id, season, ep_no, kind, uid, runtime_sec)
			 VALUES($1, 1, $2, $3, $4, $5)`,
			dramaID, i+1, it.kind, it.uid, it.runtime,
		); err != nil {
			return wrapWriteErr("写入派生表", err)
		}
	}
	return nil
}

// embyItemColumns 是 emby_item 表的显式列清单（不含渲染辅助的 EpFrom/EpTo）。
const embyItemColumns = `SELECT id, drama_id, season, ep_no, kind, uid, runtime_sec FROM emby_item`

// EmbyItemByUID 按 uid 查一条对外剧集（单集 `e<episode.id>` / 合并集 `m<group.id>`）。
//
// 播放面的 `/video/{id}` 用它判断「这个播放单元现在还在不在」：uid 只在派生表里
// 存在才算合法（docs/PLAYBACK-CONTRACT.md「不飘移」第 3 条 —— 某集被并进合并组后
// 它的 `e<id>` 立刻不再是播放单元，旧 URL 得到 404 而不是播到别的形态）。
// 无行返回 ErrNotFound。
func (s *Store) EmbyItemByUID(ctx context.Context, uid string) (EmbyItem, error) {
	var it EmbyItem
	err := s.db.QueryRowContext(ctx, embyItemColumns+` WHERE uid = $1`, uid).
		Scan(&it.ID, &it.DramaID, &it.Season, &it.EpNo, &it.Kind, &it.UID, &it.RuntimeSec)
	if errors.Is(err, sql.ErrNoRows) {
		return EmbyItem{}, ErrNotFound
	}
	if err != nil {
		return EmbyItem{}, fmt.Errorf("按 uid 查对外剧集 %q: %w", uid, err)
	}
	return it, nil
}

// ListEmbyItems 返回某部剧的对外剧集，按对外集号升序。
//
// 除表内列外还解出 EpisodeID（单集的源集主键，由 uid 的 `e` 前缀解析）与
// EpFrom/EpTo（合并集的源集号范围）。vfs 渲染文件名与 NFO 都用它，因此这是
// 「对外集号只由 emby_item 决定」的唯一入口（硬约束 6）。
func (s *Store) ListEmbyItems(ctx context.Context, dramaID int64) ([]EmbyItem, error) {
	rows, err := s.db.QueryContext(ctx, embyItemColumns+` WHERE drama_id = $1 ORDER BY ep_no`, dramaID)
	if err != nil {
		return nil, fmt.Errorf("列对外剧集 drama_id=%d: %w", dramaID, err)
	}
	defer rows.Close()

	out := make([]EmbyItem, 0)
	for rows.Next() {
		var it EmbyItem
		if err := rows.Scan(&it.ID, &it.DramaID, &it.Season, &it.EpNo, &it.Kind, &it.UID, &it.RuntimeSec); err != nil {
			return nil, fmt.Errorf("读取对外剧集行: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历对外剧集 drama_id=%d: %w", dramaID, err)
	}

	// 补齐渲染辅助字段：单集的源集（一次 IN 查询取全部）、合并集的源集号范围
	// （每组合并组一次成员查询；一部剧的合并组数量很小，不做 IN 拼接）。
	var singleIDs []int64
	for i := range out {
		if out[i].Kind != "single" {
			continue
		}
		id, ok := parseUIDNumber(out[i].UID, "e")
		if !ok {
			return nil, fmt.Errorf("单集 uid %q 形态非法", out[i].UID)
		}
		out[i].EpisodeID = id
		singleIDs = append(singleIDs, id)
	}
	if len(singleIDs) > 0 {
		placeholders := placeholders(1, len(singleIDs))
		args := make([]any, len(singleIDs))
		for i, id := range singleIDs {
			args[i] = id
		}
		epRows, err := s.db.QueryContext(ctx,
			`SELECT id, ep_no, title FROM episode WHERE id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("查派生表对应剧集: %w", err)
		}
		type epInfo struct {
			epNo  int
			title string
		}
		infoByID := make(map[int64]epInfo, len(singleIDs))
		for epRows.Next() {
			var id int64
			var info epInfo
			if err := epRows.Scan(&id, &info.epNo, &info.title); err != nil {
				_ = epRows.Close()
				return nil, fmt.Errorf("读取派生表对应剧集: %w", err)
			}
			infoByID[id] = info
		}
		if err := epRows.Err(); err != nil {
			_ = epRows.Close()
			return nil, fmt.Errorf("遍历派生表对应剧集: %w", err)
		}
		_ = epRows.Close()
		for i := range out {
			if out[i].Kind != "single" {
				continue
			}
			info, ok := infoByID[out[i].EpisodeID]
			if !ok {
				return nil, fmt.Errorf("派生表指向不存在的剧集 %d", out[i].EpisodeID)
			}
			out[i].EpFrom, out[i].EpTo = info.epNo, info.epNo
			out[i].Title = info.title
		}
	}

	for i := range out {
		if out[i].Kind != "merged" {
			continue
		}
		id, ok := parseUIDNumber(out[i].UID, "m")
		if !ok {
			return nil, fmt.Errorf("合并集 uid %q 形态非法", out[i].UID)
		}
		members, err := s.ListMergeMembers(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(members) == 0 {
			continue // 空组：EpFrom/EpTo 保持零值
		}
		out[i].EpFrom, out[i].EpTo = members[0].EpNo, members[0].EpNo
		for _, m := range members {
			if m.EpNo < out[i].EpFrom {
				out[i].EpFrom = m.EpNo
			}
			if m.EpNo > out[i].EpTo {
				out[i].EpTo = m.EpNo
			}
		}
	}
	return out, nil
}

// singleUID 是单集句柄：`e` + episode 主键十进制（与 internal/stream 的 uid 规则一致）。
func singleUID(episodeID int64) string {
	return "e" + strconv.FormatInt(episodeID, 10)
}

// mergedUID 是合并集句柄：`m` + merge_group 主键十进制。前缀与单集不同，
// 因此合并集 uid 永不匹配 `^e[0-9]+$`（DESIGN D3）。
func mergedUID(groupID int64) string {
	return "m" + strconv.FormatInt(groupID, 10)
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

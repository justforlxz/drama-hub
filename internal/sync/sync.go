// Package sync 是同步编排（框架侧）：按插件契约取数，把领域值写进 store。
//
// 插件只做「上游字节 → 领域值」的翻译：不写库、不开事务、不 import store（ADR-0006）。
// 事务、派生表重建、sync_at 都在这里。刷新调度（一轮三段、游标推进、间隔热读）属
// S4（MKACG-606）；本包只提供「按一个作品写一次库」这个幂等动作。
package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/store"
)

// WorkWithEpisodeKeys 是插件的可选能力：一次 Work 同时返回详情与每集在源侧的集标识
// （红果的 vid），框架据此写 episode.vid。
//
// docs/PLUGIN-CONTRACT.md 的 Episode 只有集号 / 标题 / 时长，源侧集标识没有位置；
// 为了「迁移前后入库字段一致」（episode.vid 保留），由本包（消费方）定义这个可选
// 接口，插件侧按需实现。不实现它的插件 episode.vid 留空。
type WorkWithEpisodeKeys interface {
	WorkWithEpisodeKeys(ctx context.Context, ref plugin.WorkRef) (plugin.WorkDetail, []string, error)
}

// Syncer 是一个插件的写库编排。零值不可用，用 New 构造。
//
// 不持有日志器：本包只把错误原样返回，口径由调用方（管理面）记一条，避免同一次
// 失败在两层各写一行。
type Syncer struct {
	source plugin.Plugin
	now    func() time.Time // 取当前时间，测试注入固定值
}

// New 构造 Syncer，时钟用 time.Now。
func New(source plugin.Plugin) *Syncer {
	return &Syncer{source: source, now: time.Now}
}

// Name 返回底层插件的名字（= 写库用的 drama.provider / episode.source）。
func (s *Syncer) Name() string { return s.source.Name() }

// SyncSeries 按上游作品 id 取一次详情，并在一个事务里写入剧目与全部集。
//
// 入库字段与 1.0（internal/provider.SyncSeries）逐字段一致：provider / source = 插件名、
// provider_id = 作品 id、year 由上游首见时间推出、episode_total = 集表长度、
// episode.vid 取插件的源侧集标识；episode.title / duration_sec 留零值（源侧批量拿不到），
// play_url 不写（同步只保证剧与集齐全，见 docs/DEV-STANDARDS.md「play_url 留空的契约」）。
//
// 取数与解析在事务之外完成；库里任何一步失败整体回滚，不留「有剧无集」的半截数据。
// 错误是插件哨兵（ErrUpstream / ErrRejected / ErrSchema / ErrNotFound / ErrNoStream）
// 或 store 哨兵，原样返回，调用方用 errors.Is 判定。
func (s *Syncer) SyncSeries(ctx context.Context, st *store.Store, id string) error {
	ref := plugin.WorkRef{Plugin: s.source.Name(), ID: id}
	detail, keys, err := s.work(ctx, ref)
	if err != nil {
		return err
	}
	if err := validateDetail(ref, detail, keys); err != nil {
		return err
	}
	if len(keys) == 0 { // 插件没实现可选接口：不给 episode.vid 编造值
		keys = make([]string, len(detail.Episodes))
	}

	w := detail.Work
	drama := store.Drama{
		Provider:     s.source.Name(),
		ProviderID:   ref.ID,
		Title:        w.Title,
		Year:         w.Year,
		Plot:         w.Plot,
		CoverURL:     w.CoverURL,
		EpisodeTotal: len(detail.Episodes),
		SyncAt:       s.now().UTC().Format(time.RFC3339),
	}
	return st.InTx(ctx, func(tx *store.Store) error {
		dramaID, err := tx.UpsertDrama(ctx, drama)
		if err != nil {
			return fmt.Errorf("写入剧目 %s: %w", ref.ID, err)
		}
		for _, ep := range detail.Episodes {
			row := store.Episode{
				DramaID:     dramaID,
				EpNo:        ep.No,
				Title:       ep.Title,
				DurationSec: ep.DurationSec,
				Source:      s.source.Name(),
				Vid:         keys[ep.No-1],
			}
			if _, err := tx.UpsertEpisode(ctx, row); err != nil {
				return fmt.Errorf("写入第 %d 集: %w", ep.No, err)
			}
		}
		// 同步完成是 emby_item 派生表的调用点之一（DEV-STANDARDS「数据库」）：剧与集
		// 落库后立刻重建对外剧集视图，否则 vfs 树看不到任何集（硬约束 6 只认派生表）。
		if _, err := tx.RebuildEmbyItems(ctx, dramaID); err != nil {
			return fmt.Errorf("重建对外剧集 %s: %w", ref.ID, err)
		}
		return nil
	})
}

// work 取详情与源侧集标识：插件实现 WorkWithEpisodeKeys 时一次调用拿两样，
// 否则退回契约的 Work，集标识留空。
func (s *Syncer) work(ctx context.Context, ref plugin.WorkRef) (plugin.WorkDetail, []string, error) {
	if kw, ok := s.source.(WorkWithEpisodeKeys); ok {
		return kw.WorkWithEpisodeKeys(ctx, ref)
	}
	detail, err := s.source.Work(ctx, ref)
	return detail, nil, err
}

// validateDetail 把契约里「框架会校验」的几条落成代码：集表非空、集号从 1 起连续
// （Episodes[i].No == i+1）、源侧集标识数量与集数一致。违反按 ErrSchema 处理——
// 这是插件实现 bug，不是上游问题（docs/PLUGIN-CONTRACT.md 硬性要求 7）。
func validateDetail(ref plugin.WorkRef, detail plugin.WorkDetail, keys []string) error {
	n := len(detail.Episodes)
	if n == 0 {
		return fmt.Errorf("%w: %s 的集表为空", plugin.ErrSchema, ref.ID)
	}
	for i, ep := range detail.Episodes {
		if ep.No != i+1 {
			return fmt.Errorf("%w: %s 的第 %d 项集号是 %d，期望 %d",
				plugin.ErrSchema, ref.ID, i+1, ep.No, i+1)
		}
	}
	if len(keys) != 0 && len(keys) != n {
		return fmt.Errorf("%w: %s 的源侧集标识 %d 项，集表 %d 项",
			plugin.ErrSchema, ref.ID, len(keys), n)
	}
	return nil
}

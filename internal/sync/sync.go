// Package sync 是同步编排（框架侧）：按插件契约取数，把领域值写进 store。
//
// 插件只做「上游字节 → 领域值」的翻译：不写库、不开事务、不 import store（ADR-0006）。
// 事务、派生表重建、sync_at 都在这里。刷新调度（一轮三段、游标推进、间隔热读）属
// S4（MKACG-606）；本包只提供「按一个作品写一次库」这个幂等动作。
package sync

import (
	"context"
	"encoding/json"
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

// CoverLocalizer 是海报本地化的能力依赖（docs/CACHE-MODEL.md「海报」）：把上游封面
// 地址落成本地文件并写 artifact(kind='cover') 索引，之后刷新不再回源。
//
// 接口在消费方定义：同步编排只要「把这张图变成自己的」，不关心落盘布局与 HTTP。
type CoverLocalizer interface {
	LocalizeCover(ctx context.Context, provider, providerID, url string) error
}

// Syncer 是一个插件的写库编排。零值不可用，用 New 构造。
//
// 不持有日志器：本包只把错误原样返回，口径由调用方（管理面）记一条，避免同一次
// 失败在两层各写一行。
type Syncer struct {
	source plugin.Plugin
	now    func() time.Time // 取当前时间，测试注入固定值
	covers CoverLocalizer   // nil = 不做海报本地化
}

// New 构造 Syncer，时钟用 time.Now。
//
// covers 为 nil 表示不本地化海报（不关心海报的测试、无可写缓存目录的调用方）；
// 生产装配根必须传真实实现，否则海报只会留在上游。
func New(source plugin.Plugin, covers CoverLocalizer) *Syncer {
	return &Syncer{source: source, now: time.Now, covers: covers}
}

// Name 返回底层插件的名字（= 写库用的 drama.provider / episode.source）。
func (s *Syncer) Name() string { return s.source.Name() }

// SyncSeries 按上游作品 id 取一次详情，并在一个事务里写入剧目与全部集。
//
// 入库字段与 1.0（internal/provider.SyncSeries）逐字段一致：provider / source = 插件名、
// provider_id = 作品 id、year 由上游首见时间推出、episode_total = 集表长度、
// episode.vid 取插件的源侧集标识；episode.title / duration_sec 留零值（源侧批量拿不到；
// duration_sec 由首播建索引时回填，见 internal/segment 与 docs/DEV-STANDARDS.md），
// 直链不落库（同步只保证剧与集齐全，见 docs/DEV-STANDARDS.md「直链不落库的契约」）。
// 2.0 新增三列按 docs/DATA-MODEL.md「类型 / 分类」落：genre = Genres[0]（主类型，
// 空 = 上游未声明）、tags = 全部标签的 JSON 数组（保真）、shard 由 store 按身份派生。
//
// 取数与解析在事务之外完成；库里任何一步失败整体回滚，不留「有剧无集」的半截数据。
// 错误是插件哨兵（ErrUpstream / ErrRejected / ErrSchema / ErrNotFound / ErrNoStream）
// 或 store 哨兵，原样返回，调用方用 errors.Is 判定。
//
// 海报本地化在事务提交之后：它是附带资产，落盘与索引都不参与剧/集写入。失败时返回
// 带上下文的错误，但剧与集已经入库（各自完整、重跑幂等），因此调用方看到的是
// 「数据已入库、海报没取到」。已有本地海报时不再回源（源图失效不影响已入库海报）。
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
		Genre:        primaryGenre(w.Genres),
		Tags:         tagsJSON(w.Genres),
	}
	err = st.InTx(ctx, func(tx *store.Store) error {
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
	if err != nil {
		return err
	}
	if s.covers == nil {
		return nil
	}
	if err := s.covers.LocalizeCover(ctx, ref.Plugin, ref.ID, w.CoverURL); err != nil {
		return fmt.Errorf("海报本地化 %s（剧与集已入库）: %w", ref.ID, err)
	}
	return nil
}

// primaryGenre 取主类型：上游声明的第一个标签；空 = 上游未声明（目录兜底为「其他」）。
func primaryGenre(genres []string) string {
	if len(genres) == 0 {
		return ""
	}
	return genres[0]
}

// tagsJSON 把上游标签保序、去重后编码成 JSON 数组；空列表编码为 "[]"，与 schema
// 默认值一致（null 会让「有没有标签」多一种表示）。
func tagsJSON(genres []string) string {
	uniq := make([]string, 0, len(genres))
	seen := make(map[string]struct{}, len(genres))
	for _, g := range genres {
		if _, ok := seen[g]; ok {
			continue
		}
		seen[g] = struct{}{}
		uniq = append(uniq, g)
	}
	// []string 的 JSON 编码不会失败（元素只可能是合法 UTF-8 字符串，非法字节已被
	// 替换成 U+FFFD），因此这里不返回错误。
	body, _ := json.Marshal(uniq)
	return string(body)
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

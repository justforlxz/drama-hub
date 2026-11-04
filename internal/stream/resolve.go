package stream

import (
	"context"
	"fmt"

	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/store"
)

// Direct 是一次解析的结果：可播直链 + 可选的解密 key（plugin.Stream 的透传）。
//
// 「可播」的唯一判据是 URL 非空（docs/PLAYBACK-CONTRACT.md）；Key 只是附加属性，
// nil = 明文。Key 与 URL 同生命周期：只在本次请求的内存里存在，不落库、不进日志、
// 不跨请求复用（docs/PLUGIN-CONTRACT.md「加密切片与 Key」）。
type Direct struct {
	URL string
	Key []byte // 16 字节 AES-128；nil = 明文
}

// Resolver 在请求时把一集解析成可播放直链（docs/PLAYBACK-CONTRACT.md「两个口子」）。
//
// 直链是短时凭据：不落库、不缓存、不跨请求复用。每次播放请求现解析一次，
// 上游换链后下一次请求自然拿到新链 —— 这正是 1.0 断链（play_url 从不写入）的修法。
type Resolver struct {
	st  *store.Store
	reg *plugin.Registry
}

// NewResolver 装配解析器。st / reg 不可为 nil（装配根持有唯一实例）。
func NewResolver(st *store.Store, reg *plugin.Registry) *Resolver {
	return &Resolver{st: st, reg: reg}
}

// Direct 解析 ep 这一集的直链与解密 key：ep → 所属剧（provider / provider_id）→ 插件 Resolve。
//
// 错误全部原样透出（插件哨兵 plugin.ErrNoStream / ErrNotFound / ErrUpstream …）：
// 边界包按契约一律映射 502，且不写库、不写缓存。插件返回空链而不报错同样是
// 「没有可播直链」，归 plugin.ErrNoStream。
func (r *Resolver) Direct(ctx context.Context, ep store.Episode) (Direct, error) {
	d, err := r.st.DramaByID(ctx, ep.DramaID)
	if err != nil {
		return Direct{}, fmt.Errorf("查剧集 %d 所属剧目: %w", ep.ID, err)
	}
	p, ok := r.reg.Get(d.Provider)
	if !ok {
		return Direct{}, fmt.Errorf("剧集 %d 的源 %q 未注册插件", ep.ID, d.Provider)
	}
	s, err := p.Resolve(ctx, plugin.WorkRef{Plugin: d.Provider, ID: d.ProviderID}, ep.EpNo)
	if err != nil {
		return Direct{}, err
	}
	if s.URL == "" {
		return Direct{}, fmt.Errorf("剧集 %d（%s 第 %d 集）: %w", ep.ID, d.ProviderID, ep.EpNo, plugin.ErrNoStream)
	}
	return Direct{URL: s.URL, Key: s.Key}, nil
}

// SeriesResolver 是插件的可选能力：一次调用解析同一 series 的多集直链
// （红果的 Plugin.ResolveSeries，MKACG-677）。
//
// 与 sync.WorkWithEpisodeKeys 同一形态——不在 plugin.Plugin 里，由框架侧按需
// 断言。为什么要它：插件 Resolve 每集都要先取一次剧集页（拿 vid 与
// protectedPlayback），合并集播放 M 集就是 M+1 次页面请求；源侧实测约 1/10 的
// 页面请求会「200 + 空体 + 挂到超时」，请求数被放大等于 502 概率被放大。
// 批量能力把页面请求压到 1 次；插件不实现它时 DirectMany 退回逐集 Direct。
type SeriesResolver interface {
	ResolveSeries(ctx context.Context, ref plugin.WorkRef, epNos []int) ([]plugin.Stream, error)
}

// DirectMany 解析同一部剧多集的直链（eps 按播放单元顺序）。返回顺序与 eps 一一对应。
//
// 与 Direct 一样不缓存：直链只活在本次调用的内存里。插件实现了 SeriesResolver 就
// 一次解析（一次页面请求），否则逐集 Direct。错误原样透出，带剧集号便于定位。
func (r *Resolver) DirectMany(ctx context.Context, eps []store.Episode) ([]Direct, error) {
	if len(eps) == 0 {
		return nil, nil
	}
	dramaID := eps[0].DramaID
	for _, ep := range eps {
		if ep.DramaID != dramaID {
			return nil, fmt.Errorf("批量解析跨剧：剧集 %d 属于剧目 %d 而非 %d", ep.ID, ep.DramaID, dramaID)
		}
	}
	d, err := r.st.DramaByID(ctx, dramaID)
	if err != nil {
		return nil, fmt.Errorf("查剧目 %d（%d 集）: %w", dramaID, len(eps), err)
	}
	p, ok := r.reg.Get(d.Provider)
	if !ok {
		return nil, fmt.Errorf("剧目 %d 的源 %q 未注册插件", dramaID, d.Provider)
	}
	host := func(ep store.Episode) string {
		return fmt.Sprintf("剧集 %d（%s 第 %d 集）", ep.ID, d.ProviderID, ep.EpNo)
	}

	streams := make([]plugin.Stream, len(eps))
	if sr, ok := p.(SeriesResolver); ok {
		nos := make([]int, len(eps))
		for i, ep := range eps {
			nos[i] = ep.EpNo
		}
		streams, err = sr.ResolveSeries(ctx, plugin.WorkRef{Plugin: d.Provider, ID: d.ProviderID}, nos)
		if err != nil {
			return nil, err
		}
		if len(streams) != len(eps) {
			return nil, fmt.Errorf("%w: %s 批量解析 %d 集却回来 %d 条",
				plugin.ErrSchema, d.ProviderID, len(eps), len(streams))
		}
	} else {
		for i, ep := range eps {
			s, err := p.Resolve(ctx, plugin.WorkRef{Plugin: d.Provider, ID: d.ProviderID}, ep.EpNo)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", host(ep), err)
			}
			streams[i] = s
		}
	}

	out := make([]Direct, len(eps))
	for i, ep := range eps {
		if streams[i].URL == "" {
			return nil, fmt.Errorf("%s: %w", host(ep), plugin.ErrNoStream)
		}
		out[i] = Direct{URL: streams[i].URL, Key: streams[i].Key}
	}
	return out, nil
}

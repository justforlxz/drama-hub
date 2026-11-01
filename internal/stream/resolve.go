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

package stream

import (
	"context"
	"fmt"

	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/store"
)

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

// DirectURL 解析 ep 这一集的直链：ep → 所属剧（provider / provider_id）→ 插件 Resolve。
//
// 错误全部原样透出（插件哨兵 plugin.ErrNoStream / ErrNotFound / ErrUpstream …）：
// 边界包按契约一律映射 502，且不写库、不写缓存。
func (r *Resolver) DirectURL(ctx context.Context, ep store.Episode) (string, error) {
	d, err := r.st.DramaByID(ctx, ep.DramaID)
	if err != nil {
		return "", fmt.Errorf("查剧集 %d 所属剧目: %w", ep.ID, err)
	}
	p, ok := r.reg.Get(d.Provider)
	if !ok {
		return "", fmt.Errorf("剧集 %d 的源 %q 未注册插件", ep.ID, d.Provider)
	}
	s, err := p.Resolve(ctx, plugin.WorkRef{Plugin: d.Provider, ID: d.ProviderID}, ep.EpNo)
	if err != nil {
		return "", err
	}
	if s.URL == "" {
		// 插件返回空链而不报错也是「没有可播直链」：契约把这种状态归 ErrNoStream。
		return "", fmt.Errorf("剧集 %d（%s 第 %d 集）: %w", ep.ID, d.ProviderID, ep.EpNo, plugin.ErrNoStream)
	}
	return s.URL, nil
}

package stream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/store"
)

// 通道取值（MKACG-626 裁决 3，判据见 ChannelOf）。
const (
	ChannelWeb = "web" // 网页通道：明文源
	ChannelApp = "app" // App 通道：CENC 密文源，ffmpeg 要 -decryption_key
)

// ChannelOf 是通道的唯一判据：带内容密钥的流是 App 通道，其余是网页通道。
//
// 判据只能是「有没有 key」：插件契约里没有通道名字段，而字节流的分水岭恰好就是
// 它 —— 网页通道交付明文，密文只可能来自 App 通道（docs/PLUGIN-CONTRACT.md
// 「加密切片与 Key」）。判据放在本包是因为直链落库时要知道通道
// （episode_stream 的主键一半），segment 侧委托到这里，全仓仍只有一处实现。
func ChannelOf(d Direct) string {
	if len(d.Key) > 0 {
		return ChannelApp
	}
	return ChannelWeb
}

// Direct 是一次解析的结果：可播直链 + 可选的解密 key（plugin.Stream 的透传）。
//
// 「可播」的唯一判据是 URL 非空（docs/PLAYBACK-CONTRACT.md）；Key 只是附加属性，
// nil = 明文。URL 与 Key 同生命周期：一起落库、一起失效、一起重取，但都不进日志、
// 不进错误文本（docs/adr/0016-episode-stream-cache.md）。
type Direct struct {
	URL         string
	Key         []byte // 16 字节 AES-128；nil = 明文
	DurationSec int    // >0 表示源接口直接给出了该集时长（秒）
}

// Resolver 在请求时把一集解析成可播放直链（docs/PLAYBACK-CONTRACT.md「两个口子」）。
//
// 直链是短时凭据，但「短时」不等于「一次性」：解析结果连同 expires_at 落库
// （episode_stream），未过期就直接复用、零外呼；过期或被告知失效才重解析
// （docs/adr/0016-episode-stream-cache.md）。同一集的并发请求共用一次解析。
type Resolver struct {
	st       *store.Store
	settings *store.Settings
	reg      *plugin.Registry

	mu      sync.Mutex
	flights map[int64]*flight
}

// flight 是「这一集正在解析」的占位：所有者解析完把结果填进来并关闭 done，
// 其余等待者读同一份结果。
type flight struct {
	done   chan struct{}
	direct Direct
	err    error
}

// NewResolver 装配解析器。st / settings / reg 不可为 nil（装配根持有唯一实例）。
func NewResolver(st *store.Store, settings *store.Settings, reg *plugin.Registry) *Resolver {
	return &Resolver{st: st, settings: settings, reg: reg, flights: make(map[int64]*flight)}
}

// Direct 返回 ep 这一集的可播直链：未过期的库内行优先，否则现解析一次并落库。
//
// 错误全部原样透出（插件哨兵 plugin.ErrNoStream / ErrNotFound / ErrUpstream …）：
// 边界包按契约映射状态码。插件返回空链而不报错同样是「没有可播直链」，归
// plugin.ErrNoStream，且不会把空 URL 写进库。
func (r *Resolver) Direct(ctx context.Context, ep store.Episode) (Direct, error) {
	if d, ok, err := r.cached(ctx, ep.ID); err != nil {
		return Direct{}, err
	} else if ok {
		return d, nil
	}

	f, owner := r.claim(ep.ID)
	if !owner {
		return waitFlight(ctx, f)
	}
	// 抢到所有权之后必须复核一次缓存：上面的 cached 与这次 claim 之间，可能有
	// 另一个 owner 恰好解析完并落了库（它先 persist 再 finish），此刻轮到我当
	// owner，若不复核就会把同一集再外呼一遍。
	if d, ok, err := r.cached(ctx, ep.ID); err != nil {
		r.finish(ep.ID, f, Direct{}, err)
		return Direct{}, err
	} else if ok {
		r.finish(ep.ID, f, d, nil)
		return d, nil
	}

	s, err := r.fetch(ctx, ep)
	var d Direct
	if err == nil {
		d = Direct{URL: s.URL, Key: s.Key, DurationSec: s.DurationSec}
		err = r.persist(ctx, ep.ID, s)
		if err == nil && s.DurationSec > 0 && ep.DurationSec <= 0 {
			_, _ = r.st.BackfillEpisodeDuration(ctx, ep.ID, s.DurationSec)
		}
	}
	r.finish(ep.ID, f, d, err)
	return d, err
}

// Invalidate 作废某集的直链（所有通道），下一次 Direct 会重新解析。
//
// 边界包在 CDN 明确拒绝（401/403/404）时调用它：只作废这一集，不因为一条失败
// 清掉整部剧（docs/adr/0016-episode-stream-cache.md「失败自愈」）。
func (r *Resolver) Invalidate(ctx context.Context, episodeID int64) error {
	return r.st.InvalidateEpisodeStream(ctx, episodeID)
}

// cached 返回库内未过期的直链。ok=false 表示没有可用行（无行或已过期）。
//
// 过期判定在 Go 里做（expires_at 是不透明 TEXT，与 0001 的业务时间列同一约定）；
// 解析不了的时间按过期处理 —— 宁可贵一次重新解析，也不拿一条不知道有效期的链去播。
func (r *Resolver) cached(ctx context.Context, episodeID int64) (Direct, bool, error) {
	es, err := r.st.GetEpisodeStream(ctx, episodeID)
	if errors.Is(err, store.ErrNotFound) {
		return Direct{}, false, nil
	}
	if err != nil {
		return Direct{}, false, err
	}
	expires, err := time.Parse(time.RFC3339, es.ExpiresAt)
	if err != nil || !time.Now().Before(expires) {
		return Direct{}, false, nil
	}
	return Direct{URL: es.URL, Key: es.Key}, true, nil
}

// claim 抢占某集的解析权：owner=true 时调用方负责解析并 finish；否则拿到一个
// 正在解析的 flight，等它的结果。
func (r *Resolver) claim(episodeID int64) (f *flight, owner bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.flights[episodeID]; ok {
		return f, false
	}
	f = &flight{done: make(chan struct{})}
	r.flights[episodeID] = f
	return f, true
}

// finish 记录解析结果、唤醒等待者并回收占位。
func (r *Resolver) finish(episodeID int64, f *flight, d Direct, err error) {
	f.direct, f.err = d, err
	r.mu.Lock()
	delete(r.flights, episodeID)
	close(f.done)
	r.mu.Unlock()
}

// waitFlight 等一次在途解析的结果；ctx 先结束就放弃等待。
func waitFlight(ctx context.Context, f *flight) (Direct, error) {
	select {
	case <-f.done:
		return f.direct, f.err
	case <-ctx.Done():
		return Direct{}, ctx.Err()
	}
}

// fetch 向插件解析一次，不做任何缓存。
func (r *Resolver) fetch(ctx context.Context, ep store.Episode) (plugin.Stream, error) {
	// 播放面解析：插件出站日志据此打上 caller=play（MKACG-683）。
	ctx = plugin.WithCaller(ctx, plugin.CallerPlay)

	d, err := r.st.DramaByID(ctx, ep.DramaID)
	if err != nil {
		return plugin.Stream{}, fmt.Errorf("查剧集 %d 所属剧目: %w", ep.ID, err)
	}
	p, ok := r.reg.Get(d.Provider)
	if !ok {
		return plugin.Stream{}, fmt.Errorf("剧集 %d 的源 %q 未注册插件", ep.ID, d.Provider)
	}
	s, err := p.Resolve(ctx, plugin.WorkRef{Plugin: d.Provider, ID: d.ProviderID}, ep.EpNo)
	if err != nil {
		return plugin.Stream{}, err
	}
	if s.URL == "" {
		return plugin.Stream{}, fmt.Errorf("剧集 %d（%s 第 %d 集）: %w", ep.ID, d.ProviderID, ep.EpNo, plugin.ErrNoStream)
	}
	return s, nil
}

// persist 把一次解析结果连同过期时刻落库。
//
// expires_at：上游给了 ExpiresAt 就用它，否则 fetched_at + stream_url_ttl_sec。
// ttl 每次现读库（硬约束 5：改配置即时生效），不在进程里缓存。
func (r *Resolver) persist(ctx context.Context, episodeID int64, s plugin.Stream) error {
	ttl, err := r.settings.Int(ctx, store.KeyStreamURLTTL)
	if err != nil {
		return fmt.Errorf("读取直链有效时长: %w", err)
	}
	now := time.Now().UTC()
	expires := s.ExpiresAt.UTC()
	if s.ExpiresAt.IsZero() {
		expires = now.Add(time.Duration(ttl) * time.Second)
	}
	es := store.EpisodeStream{
		EpisodeID: episodeID,
		Channel:   ChannelOf(Direct{URL: s.URL, Key: s.Key}),
		URL:       s.URL,
		Key:       s.Key,
		ExpiresAt: expires.Format(time.RFC3339),
		FetchedAt: now.Format(time.RFC3339),
	}
	if err := r.st.UpsertEpisodeStream(ctx, es); err != nil {
		return err
	}
	return nil
}

// 预取失败的稳定分类（episode_stream_fail.reason 的取值）。
//
// 分类只在本包做一次：调用方（internal/segment 的预取器）不认识插件哨兵，
// 免得以后多一处「哪些错误算什么原因」的第二套判据（ADR-0017）。
const (
	FailureRejected    = "rejected"    // 上游明确拒绝（401/403/404 等，plugin.ErrRejected）
	FailureUpstream    = "upstream"    // 上游故障（超时、5xx、空响应，plugin.ErrUpstream）
	FailureSchema      = "schema"      // 上游结构不符契约（plugin.ErrSchema）
	FailureNoStream    = "no_stream"   // 该集没有可播直链（plugin.ErrNoStream）
	FailureLogin       = "login"       // 需要登录态或无权限（plugin.ErrLoginRequired）
	FailureUnsupported = "unsupported" // 仅提供不支持的流形态（plugin.ErrUnsupported）
	FailureOther       = "other"       // 其余（含库/注册表故障等无法归类的失败）
)

// FailureReason 把一次解析失败折成 episode_stream_fail.reason 的稳定取值。
//
// 判据只有 errors.Is 一次，幂等、可重放；未识别的错误归 other（不丢记录，
// 只丢精度）。前台错误矩阵（segment.playErr）按自己的语义映射状态码，
// 两者互不影响。
func FailureReason(err error) string {
	switch {
	case errors.Is(err, plugin.ErrRejected):
		return FailureRejected
	case errors.Is(err, plugin.ErrUpstream):
		return FailureUpstream
	case errors.Is(err, plugin.ErrSchema):
		return FailureSchema
	case errors.Is(err, plugin.ErrNoStream):
		return FailureNoStream
	case errors.Is(err, plugin.ErrLoginRequired):
		return FailureLogin
	case errors.Is(err, plugin.ErrUnsupported):
		return FailureUnsupported
	}
	return FailureOther
}

// SeriesResolver 是插件的可选能力：一次调用解析同一 series 的多集直链
// （红果的 Plugin.ResolveSeries，MKACG-677）。
//
// 与 sync.WorkWithEpisodeKeys 同一形态——不在 plugin.Plugin 里，由框架侧按需
// 断言。为什么要它：插件 Resolve 每集都要先取一次剧集页（拿 vid 与
// protectedPlayback），合并集播放 M 集就是 M+1 次页面请求；源侧实测约 1/10 的
// 页面请求会「200 + 空体 + 挂到超时」，请求数被放大等于 502 概率被放大。
// 批量能力把页面请求压到 1 次；插件不实现它时 DirectMany 退回逐集解析。
type SeriesResolver interface {
	ResolveSeries(ctx context.Context, ref plugin.WorkRef, epNos []int) ([]plugin.Stream, error)
}

// DirectMany 返回同一部剧多集的直链（eps 按播放单元顺序），顺序与 eps 一一对应。
//
// 与 Direct 共用同一份库内缓存：已过期的集不重复解析，缺的那部分才批量申请；
// 单集能力的插件逐集解析。返回结果同时落库，后续播放直接命中。
func (r *Resolver) DirectMany(ctx context.Context, eps []store.Episode) ([]Direct, error) {
	// 与 Direct 同一来源标记：批量解析也是播放（MKACG-683）。
	ctx = plugin.WithCaller(ctx, plugin.CallerPlay)
	if len(eps) == 0 {
		return nil, nil
	}
	dramaID := eps[0].DramaID
	for _, ep := range eps {
		if ep.DramaID != dramaID {
			return nil, fmt.Errorf("批量解析跨剧：剧集 %d 属于剧目 %d 而非 %d", ep.ID, ep.DramaID, dramaID)
		}
	}

	out := make([]Direct, len(eps))
	var missing []int
	for i, ep := range eps {
		d, ok, err := r.cached(ctx, ep.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			out[i] = d
			continue
		}
		missing = append(missing, i)
	}
	if len(missing) == 0 {
		return out, nil
	}

	// 抢占缺的那些集；同一集的并发请求共用一次解析。
	flights := make(map[int]*flight, len(missing))
	var owned []int
	for _, i := range missing {
		f, owner := r.claim(eps[i].ID)
		flights[i] = f
		if owner {
			owned = append(owned, i)
		}
	}
	ownedSet := make(map[int]bool, len(owned))
	for _, i := range owned {
		ownedSet[i] = true
	}
	if len(owned) > 0 {
		// 与 Direct 同一理由：claim 之后复核缓存 —— cached 到 claim 之间可能有
		// 别的 owner 解析完并落库（先 persist 再 finish），命中的集直接用那一行。
		all := append([]int(nil), owned...)
		still, err := r.recheck(ctx, eps, owned, flights, out)
		if err != nil {
			r.abandon(eps, all, flights, err)
			return nil, err
		}
		if len(still) > 0 {
			if err := r.fillMany(ctx, eps, still, flights, out); err != nil {
				r.abandon(eps, still, flights, err)
				return nil, err
			}
		}
	}
	for _, i := range missing {
		if ownedSet[i] {
			continue
		}
		d, err := waitFlight(ctx, flights[i])
		if err != nil {
			return nil, err
		}
		out[i] = d
	}
	return out, nil
}

// recheck 复核刚抢到所有权的集：cached 与 claim 之间可能已有别的 owner 解析完
// 并落库（先 persist 再 finish），命中的集直接用那一行并唤醒等待者，不再外呼。
// 返回仍缺直链的集（顺序与 owned 一致）。
func (r *Resolver) recheck(ctx context.Context, eps []store.Episode, owned []int, flights map[int]*flight, out []Direct) ([]int, error) {
	still := make([]int, 0, len(owned))
	for _, i := range owned {
		d, ok, err := r.cached(ctx, eps[i].ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			still = append(still, i)
			continue
		}
		out[i] = d
		r.finish(eps[i].ID, flights[i], d, nil)
	}
	return still, nil
}

// abandon 收尾自己负责但尚未完成的 flight（失败路径），否则等待者会挂到 ctx 超时。
func (r *Resolver) abandon(eps []store.Episode, owned []int, flights map[int]*flight, err error) {
	for _, i := range owned {
		select {
		case <-flights[i].done:
		default:
			r.finish(eps[i].ID, flights[i], Direct{}, err)
		}
	}
}

// fillMany 解析 owned 指向的集（一次批量入口），逐条落库并唤醒各自的等待者。
func (r *Resolver) fillMany(ctx context.Context, eps []store.Episode, owned []int, flights map[int]*flight, out []Direct) error {
	batch := make([]store.Episode, len(owned))
	for j, i := range owned {
		batch[j] = eps[i]
	}
	streams, err := r.fetchMany(ctx, batch)
	if err != nil {
		return err
	}
	for j, i := range owned {
		s := streams[j]
		d := Direct{URL: s.URL, Key: s.Key, DurationSec: s.DurationSec}
		err := r.persist(ctx, eps[i].ID, s)
		if err == nil && s.DurationSec > 0 && eps[i].DurationSec <= 0 {
			_, _ = r.st.BackfillEpisodeDuration(ctx, eps[i].ID, s.DurationSec)
		}
		r.finish(eps[i].ID, flights[i], d, err)
		if err != nil {
			return err
		}
		out[i] = d
	}
	return nil
}

// fetchMany 一次解析同一部剧多集（有批量能力就一次调用，否则逐集）。
func (r *Resolver) fetchMany(ctx context.Context, eps []store.Episode) ([]plugin.Stream, error) {
	dramaID := eps[0].DramaID
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
	for i, ep := range eps {
		if streams[i].URL == "" {
			return nil, fmt.Errorf("%s: %w", host(ep), plugin.ErrNoStream)
		}
	}
	return streams, nil
}

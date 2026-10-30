package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/scheduler"
	"github.com/justforlxz/drama-hub/internal/store"
)

// 缓存代理的建议参数（ADR-0004）。缓存淘汰由 rclone 负责，本服务只生成建议值：
// 参数不参与任何运行逻辑，也不进 settings（它们不是本服务的运行配置）。
// 取值来自设计基线 Q5（缓存 200 GB / 保留 30 天）。
const (
	cacheSuggestMaxSize = "200G"
	cacheSuggestMaxAge  = "720h"
	cacheSuggestFlags   = "--vfs-cache-mode full --vfs-cache-max-size " + cacheSuggestMaxSize +
		" --vfs-cache-max-age " + cacheSuggestMaxAge
)

// totalsView 是看板顶部要的全局计数。
type totalsView struct {
	Dramas      int    `json:"dramas"`
	Episodes    int    `json:"episodes"`
	Playable    int    `json:"playable_episodes"`
	EmbyItems   int    `json:"emby_items"`
	MergeGroups int    `json:"merge_groups"`
	LastSyncAt  string `json:"last_sync_at"`
}

// configView 是运行配置的现状快照。密钥类配置只报「是否已设置」，不回传值。
type configView struct {
	StrmPrefix      string `json:"strm_prefix"`
	SegTargetSec    int    `json:"seg_target_sec"`
	SegPrefetch     int    `json:"seg_prefetch"`
	PinDays         int    `json:"pin_days"`
	StreamSecretSet bool   `json:"stream_secret_set"`
	WebDAVUserSet   bool   `json:"webdav_user_set"`
	WebDAVPassSet   bool   `json:"webdav_pass_set"`
}

// cacheView 是缓存面的现状：是否走缓存代理（由 strm_prefix 决定）与建议参数。
type cacheView struct {
	Mode    string `json:"mode"`     // direct（直连本服务，不经缓存）| proxy（经缓存代理）
	Prefix  string `json:"prefix"`   // strm_prefix 的值，空表示直连
	MaxSize string `json:"max_size"` // 建议值，不参与运行逻辑
	MaxAge  string `json:"max_age"`  // 建议值，不参与运行逻辑
	Flags   string `json:"flags"`    // rclone 建议参数整串
	Note    string `json:"note"`
}

// sourceView 是一个播放源在状态页上的现状。
//
// enabled 取 plugin.<源>.enabled（缺失按 true），base_url / request_timeout_sec 取
// 对应配置项；reason 非空说明这个源当前不可用或不可调度，页面上直接显示。
//
// 调度字段（running / last_* / interval_sec / next_due_at）来自调度器：上次运行、
// 上次成功、上次错误都读 source_state，next_due_at 是请求时按 last_run_at + 现读
// interval 算出来的（库里没有这一列）。空字符串 = 还没发生过。
type sourceView struct {
	Name              string `json:"name"`
	Label             string `json:"label"`
	Enabled           bool   `json:"enabled"`
	BaseURL           string `json:"base_url"`
	RequestTimeoutSec int    `json:"request_timeout_sec"`
	Reason            string `json:"reason"`

	Running     bool   `json:"running"`
	LastRunAt   string `json:"last_run_at"`
	LastOkAt    string `json:"last_ok_at"`
	LastError   string `json:"last_error"`
	IntervalSec int    `json:"interval_sec"`
	NextDueAt   string `json:"next_due_at"`
}

// statusView 是 GET /api/status 的响应体：同步计数 + 运行配置 + 播放源 + 缓存面。
type statusView struct {
	Totals  totalsView   `json:"totals"`
	Config  configView   `json:"config"`
	Sources []sourceView `json:"sources"`
	Cache   cacheView    `json:"cache"`
}

// handleStatus 处理 GET /api/status：看板只读，不改任何状态。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dramas, err := s.st.ListDramas(ctx)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	stats, err := s.st.ListDramaStats(ctx)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	totals := totalsView{Dramas: len(dramas)}
	for _, st := range stats {
		totals.Episodes += st.Episodes
		totals.Playable += st.PlayableEpisodes
		totals.EmbyItems += st.EmbyItems
		totals.MergeGroups += st.MergeGroups
	}
	for _, d := range dramas {
		// sync_at 是 UTC RFC3339 文本，字典序即时间序，不需要解析。
		if d.SyncAt > totals.LastSyncAt {
			totals.LastSyncAt = d.SyncAt
		}
	}

	cfg, err := s.readConfig(ctx)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	schedule, err := s.schedule.Status(ctx)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, statusView{
		Totals:  totals,
		Config:  cfg,
		Sources: s.readSources(ctx, schedule),
		Cache:   newCacheView(cfg.StrmPrefix),
	})
}

// readConfig 读一遍运行配置。读不到的必填项显示为「未设置」而不是让整页失败：
// 看板的职责是让人看见缺什么。
func (s *Server) readConfig(ctx context.Context) (configView, error) {
	prefix, err := s.settings.String(ctx, store.KeyStrmPrefix)
	if err != nil {
		return configView{}, fmt.Errorf("读 strm_prefix: %w", err)
	}
	target, err := s.settings.Int(ctx, store.KeySegTargetSec)
	if err != nil {
		return configView{}, fmt.Errorf("读 seg_target_sec: %w", err)
	}
	prefetch, err := s.settings.Int(ctx, store.KeySegPrefetch)
	if err != nil {
		return configView{}, fmt.Errorf("读 seg_prefetch: %w", err)
	}
	pinDays, err := s.settings.Int(ctx, store.KeyPinDays)
	if err != nil {
		return configView{}, fmt.Errorf("读 pin_days: %w", err)
	}
	out := configView{
		StrmPrefix:   prefix,
		SegTargetSec: target,
		SegPrefetch:  prefetch,
		PinDays:      pinDays,
	}
	for _, item := range []struct {
		key store.SettingKey
		set *bool
	}{
		{store.KeyStreamSecret, &out.StreamSecretSet},
		{store.KeyWebDAVUser, &out.WebDAVUserSet},
		{store.KeyWebDAVPass, &out.WebDAVPassSet},
	} {
		set, err := s.settingIsSet(ctx, item.key)
		if err != nil {
			return configView{}, err
		}
		*item.set = set
	}
	return out, nil
}

// readSources 读一遍各播放源的配置，并合上调度器给的调度现状。
//
// 读不出来不让看板整页失败：该源标成未启用并把原因写进 reason —— 看板的职责是
// 让人看见缺什么。插件未启用或配置缺失时服务照常启动，原因就在这里可见。
func (s *Server) readSources(ctx context.Context, schedule []scheduler.SourceStatus) []sourceView {
	byName := make(map[string]scheduler.SourceStatus, len(schedule))
	for _, st := range schedule {
		byName[st.Name] = st
	}
	out := make([]sourceView, 0, len(s.sources))
	for _, src := range s.sources {
		v := s.readSource(ctx, src)
		applySchedule(&v, byName[src.Name])
		out = append(out, v)
	}
	return out
}

// applySchedule 把一个源的调度现状填进它的视图。时间写成 UTC RFC3339 文本（与库里
// 的写法一致），零值写空串；时区转换归页面 JS（fmtTime）。
func applySchedule(v *sourceView, st scheduler.SourceStatus) {
	v.Running = st.Running
	v.LastRunAt = formatTime(st.LastRunAt)
	v.LastOkAt = formatTime(st.LastOkAt)
	v.LastError = st.LastError
	v.IntervalSec = st.IntervalSec
	v.NextDueAt = formatTime(st.NextDueAt)
}

// formatTime 把调度器给的时间写成响应里的文本；零值写空串（页面显示「—」）。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// readSource 读一个播放源的三项配置（键格式由 store.PluginSettingKey 给）。
func (s *Server) readSource(ctx context.Context, src Source) sourceView {
	key := func(item string) store.SettingKey { return store.PluginSettingKey(src.Name, item) }
	v := sourceView{Name: src.Name, Label: src.Label}
	if v.Label == "" {
		v.Label = src.Name
	}
	enabled, enabledErr := s.settings.Bool(ctx, key(store.PluginItemEnabled))
	baseURL, baseErr := s.settings.String(ctx, key(store.PluginItemBaseURL))
	timeoutSec, timeoutErr := s.settings.Int(ctx, key(store.PluginItemRequestTimeout))
	v.Enabled = enabledErr == nil && enabled
	v.BaseURL = baseURL
	v.RequestTimeoutSec = timeoutSec

	// 三项都读出来再判原因：读失败的那一项会盖掉默认值，必须让人看见。
	switch {
	case enabledErr != nil:
		v.Reason = settingErrReason(key(store.PluginItemEnabled), enabledErr)
	case baseErr != nil:
		v.Reason = settingErrReason(key(store.PluginItemBaseURL), baseErr)
	case timeoutErr != nil:
		v.Reason = settingErrReason(key(store.PluginItemRequestTimeout), timeoutErr)
	case !v.Enabled:
		v.Reason = "已在 settings 停用（" + string(key(store.PluginItemEnabled)) +
			"=false）：不参与刷新调度"
	}
	return v
}

// settingErrReason 把一条配置读取失败写成状态页上的一句话。
func settingErrReason(key store.SettingKey, err error) string {
	return "读 " + string(key) + " 失败：" + err.Error()
}

// settingIsSet 判断一个必填配置项是否已写入（不读值本身）。
func (s *Server) settingIsSet(ctx context.Context, key store.SettingKey) (bool, error) {
	_, err := s.settings.String(ctx, key)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrSettingMissing):
		return false, nil
	default:
		return false, fmt.Errorf("读 %s: %w", key, err)
	}
}

// newCacheView 由 strm_prefix 推出缓存面现状：空 = 直连本服务（没有缓存），
// 有值 = 播放字节经该前缀（缓存代理）转发。
func newCacheView(prefix string) cacheView {
	mode := "proxy"
	if prefix == "" {
		mode = "direct"
	}
	return cacheView{
		Mode:    mode,
		Prefix:  prefix,
		MaxSize: cacheSuggestMaxSize,
		MaxAge:  cacheSuggestMaxAge,
		Flags:   cacheSuggestFlags,
		Note:    "分片缓存由缓存代理淘汰（ADR-0004）；max_size / max_age 是建议值，不参与本服务运行逻辑。",
	}
}

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/jellyfin"
	"github.com/justforlxz/drama-hub/internal/scheduler"
	"github.com/justforlxz/drama-hub/internal/store"
)

// /api/status 只报真正参与运行的配置（MKACG-616）。1.0/ADR-0004 时代的「缓存代理
// 建议参数」（rclone --vfs-cache-mode / max-size / max-age）不参与本服务任何逻辑、
// 页面却把它们排在「缓存」标题下，属于「显示的不是生效的」——ADR-0007 之后缓存归本
// 服务，那组字段已整段删除（对应 MKACG-617 的 cache.* 遗留字段结论）。

// totalsView 是看板顶部要的全局计数。
type totalsView struct {
	Dramas     int    `json:"dramas"`
	Episodes   int    `json:"episodes"`
	EmbyItems  int    `json:"emby_items"`
	LastSyncAt string `json:"last_sync_at"`
}

// configView 是运行配置的现状快照：全部字段都是真在跑的现读值。密钥类配置只报
// 「是否已设置」，不回传值。
//
// 2.0 明确不做缓存预热（MKACG-603 范围表），1.0 预留的 seg_prefetch / pin_days 两个
// 死键已删除（MKACG-616）：看板不再展示没有任何消费者的配置项。
type configView struct {
	StrmPrefix   string `json:"strm_prefix"`
	CacheDir     string `json:"cache_dir"`
	CacheMaxMB   int    `json:"cache_max_mb"`
	CacheLRUDays int    `json:"cache_lru_days"`
	// MergeChunkSize 是「几集并一条对外剧集」（docs/adr/0010）；改完下一次重建
	// （同步完成 / 首播回填）生效。
	MergeChunkSize  int  `json:"merge_chunk_size"`
	StreamSecretSet bool `json:"stream_secret_set"`
	WebDAVUserSet   bool `json:"webdav_user_set"`
	WebDAVPassSet   bool `json:"webdav_pass_set"`
	// 刷新调度的三项：状态页要逐键标「生效时机」，其中 interval 是页面可直接改的
	// 那一项（≤60 秒后调度器按新值算下一轮）。
	RefreshIntervalSec int `json:"refresh_interval_sec"`
	RefreshBatch       int `json:"refresh_batch"`
	RefreshTimeoutSec  int `json:"refresh_timeout_sec"`
}

// sourceView 是一个播放源在状态页上的现状。
//
// enabled 取 plugin.<源>.enabled（缺失按 true）；reason 非空说明这个源当前不可用或
// 不可调度，页面上直接显示。
//
// base_url / request_timeout_sec 是构造期注入插件的配置（改后需重启），所以这里同时
// 给两组值：不带后缀的是 settings 现值（改了还没重启），*_effective 是启动时真正注入
// 的值（正在跑的那个）。restart_required = 两者不一致 → 页面上直说「重启后生效」。
//
// 调度字段（running / last_* / interval_sec / next_due_at）来自调度器：上次运行、
// 上次成功、上次错误都读 source_state，next_due_at 是请求时按 last_run_at + 现读
// interval 算出来的（库里没有这一列）。空字符串 = 还没发生过。
type sourceView struct {
	Name                       string `json:"name"`
	Label                      string `json:"label"`
	Enabled                    bool   `json:"enabled"`
	BaseURL                    string `json:"base_url"`
	RequestTimeoutSec          int    `json:"request_timeout_sec"`
	BaseURLEffective           string `json:"base_url_effective"`
	RequestTimeoutSecEffective int    `json:"request_timeout_sec_effective"`
	RestartRequired            bool   `json:"restart_required"`
	Reason                     string `json:"reason"`

	Running     bool   `json:"running"`
	LastRunAt   string `json:"last_run_at"`
	LastOkAt    string `json:"last_ok_at"`
	LastError   string `json:"last_error"`
	IntervalSec int    `json:"interval_sec"`
	NextDueAt   string `json:"next_due_at"`
}

// jellyfinView 是 Jellyfin 媒体库维护的现状（一分类一库，ADR-0012）。
//
// api_key 永不出现在响应里：只给 api_key_set。configured=false 时 missing 说明缺哪几个
// 键 —— 这是「配了却没生效」唯一需要人看的一句话。created / uncertain / orphans /
// conflicts 是上一轮的结果（内存状态，重启后回到「未运行」）。uncertain 是「请求超时、
// Jellyfin 侧结果未知」的条数：不能并进 created（没收到 204），也不代表没建成。
// failed 是被 Jellyfin 明确拒掉的分类名（实测 12.1：库路径在容器内不存在回 400）。
type jellyfinView struct {
	Configured    bool     `json:"configured"`
	Missing       []string `json:"missing"`
	URL           string   `json:"url"`
	LibraryRoot   string   `json:"library_root"`
	LibraryPrefix string   `json:"library_prefix"`
	APIKeySet     bool     `json:"api_key_set"`
	TimeoutSec    int      `json:"timeout_sec"`

	Running   bool   `json:"running"`
	LastRunAt string `json:"last_run_at"`
	LastOkAt  string `json:"last_ok_at"`
	LastError string `json:"last_error"`
	Created   int    `json:"created"`
	// Unchanged / Removed / DurationMS 是本轮「做了什么」的其余部分（MKACG-664）：
	// 已存在且没动的条数、删掉的条数、整轮耗时。本服务不改已存在的库，
	// 所以没有「更新」这个计数。
	Unchanged  int      `json:"unchanged"`
	Removed    int      `json:"removed"`
	DurationMS int64    `json:"duration_ms"`
	Uncertain  int      `json:"uncertain"`
	Failed     []string `json:"failed"`
	// DeleteFailed 是删库失败、下一轮会重试的分类名（MKACG-668）：删除失败不能静默，
	// 结果面要能点名是哪几个、原因见 last_error。
	DeleteFailed []string `json:"delete_failed"`
	Disabled     []string `json:"disabled"`
	Orphans      []string `json:"orphans"`
	Conflicts    []string `json:"conflicts"`

	// 进行中的进度（MKACG-664）：页面轮询本块，按 started_at + phase_label + processed/total
	// 显示「正在跑 · 跑到哪一步 · 已处理几项」。phase 是稳定标识（空 = 没在跑），
	// phase_label 是给人看的说法 —— 措辞只在这一个地方维护。
	StartedAt  string `json:"started_at"`
	Phase      string `json:"phase"`
	PhaseLabel string `json:"phase_label"`
	Processed  int    `json:"processed"`
	Total      int    `json:"total"`
}

// jellyfinPhaseLabels 把一轮同步的阶段标识翻成页面上的一句话。
var jellyfinPhaseLabels = map[string]string{
	jellyfin.PhaseList:       "读取现有媒体库",
	jellyfin.PhaseCategories: "列分类",
	jellyfin.PhaseDiff:       "比对现有库",
	jellyfin.PhaseCreate:     "创建媒体库",
	jellyfin.PhaseDelete:     "删除已关闭分类的库",
	jellyfin.PhaseScan:       "触发扫描",
}

// jellyfinPhaseLabel 取阶段的展示文案；未知或空阶段返回空串（页面只显示「运行中」）。
func jellyfinPhaseLabel(phase string) string { return jellyfinPhaseLabels[phase] }

// statusView 是 GET /api/status 的响应体：同步计数 + 运行配置 + 播放源 + 媒体库。
type statusView struct {
	Totals   totalsView   `json:"totals"`
	Config   configView   `json:"config"`
	Sources  []sourceView `json:"sources"`
	Jellyfin jellyfinView `json:"jellyfin"`
	// AdminTokenSet 是「后台写操作能不能用」：false = 没配 DRAMA_HUB_ADMIN_TOKEN，
	// 所有非读的 /api/ 请求都会 401。令牌本身不进任何响应，这里只报有没有。
	AdminTokenSet bool `json:"admin_token_set"`
}

// jellyfinViewOf 把 Manager 的现状翻成响应体。时间写成 UTC RFC3339 文本（与库里一致），
// 零值写空串，时区转换归页面 JS（fmtTime）。
func jellyfinViewOf(state jellyfin.State) jellyfinView {
	return jellyfinView{
		Configured:    state.Configured,
		Missing:       nonNilStrings(state.Missing),
		URL:           state.URL,
		LibraryRoot:   state.LibraryRoot,
		LibraryPrefix: state.LibraryPrefix,
		APIKeySet:     state.APIKeySet,
		TimeoutSec:    state.TimeoutSec,
		Running:       state.Running,
		LastRunAt:     formatTime(state.LastRunAt),
		LastOkAt:      formatTime(state.LastOKAt),
		LastError:     state.LastError,
		Created:       state.Created,
		Unchanged:     state.Unchanged,
		Removed:       state.Removed,
		DurationMS:    state.DurationMS,
		Uncertain:     state.Uncertain,
		Failed:        nonNilStrings(state.Failed),
		DeleteFailed:  nonNilStrings(state.DeleteFailed),
		Disabled:      nonNilStrings(state.Disabled),
		Orphans:       nonNilStrings(state.Orphans),
		Conflicts:     nonNilStrings(state.Conflicts),
		StartedAt:     formatTime(state.StartedAt),
		Phase:         state.Phase,
		PhaseLabel:    jellyfinPhaseLabel(state.Phase),
		Processed:     state.Processed,
		Total:         state.Total,
	}
}

// nonNilStrings 把 nil 切片写成 []：页面按数组渲染，null 要多一条分支。
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
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
		totals.EmbyItems += st.EmbyItems
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
		Totals:        totals,
		Config:        cfg,
		Sources:       s.readSources(ctx, schedule),
		Jellyfin:      jellyfinViewOf(s.jellyfin.State(ctx)),
		AdminTokenSet: s.adminToken != "",
	})
}

// readConfig 读一遍运行配置。读不到的必填项显示为「未设置」而不是让整页失败：
// 看板的职责是让人看见缺什么。
func (s *Server) readConfig(ctx context.Context) (configView, error) {
	prefix, err := s.settings.String(ctx, store.KeyStrmPrefix)
	if err != nil {
		return configView{}, fmt.Errorf("读 strm_prefix: %w", err)
	}
	// B2：显示归一化后的值（去尾 `/`）。手改进库的行也照样显示成真正参与拼接的形态。
	prefix = strings.TrimRight(prefix, "/")
	cacheDir, err := s.settings.String(ctx, store.KeyCacheDir)
	if err != nil {
		return configView{}, fmt.Errorf("读 cache_dir: %w", err)
	}
	cacheMaxMB, err := s.settings.Int(ctx, store.KeyCacheMaxMB)
	if err != nil {
		return configView{}, fmt.Errorf("读 cache_max_mb: %w", err)
	}
	cacheLRUDays, err := s.settings.Int(ctx, store.KeyCacheLRUDays)
	if err != nil {
		return configView{}, fmt.Errorf("读 cache_lru_days: %w", err)
	}
	mergeChunk, err := s.settings.Int(ctx, store.KeyMergeChunkSize)
	if err != nil {
		return configView{}, fmt.Errorf("读 merge_chunk_size: %w", err)
	}
	refreshSec, err := s.settings.Int(ctx, store.KeyRefreshIntervalSec)
	if err != nil {
		return configView{}, fmt.Errorf("读 refresh_interval_sec: %w", err)
	}
	refreshBatch, err := s.settings.Int(ctx, store.KeyRefreshBatch)
	if err != nil {
		return configView{}, fmt.Errorf("读 refresh_batch: %w", err)
	}
	refreshTimeout, err := s.settings.Int(ctx, store.KeyRefreshTimeoutSec)
	if err != nil {
		return configView{}, fmt.Errorf("读 refresh_timeout_sec: %w", err)
	}
	out := configView{
		StrmPrefix:         prefix,
		CacheDir:           cacheDir,
		CacheMaxMB:         cacheMaxMB,
		CacheLRUDays:       cacheLRUDays,
		MergeChunkSize:     mergeChunk,
		RefreshIntervalSec: refreshSec,
		RefreshBatch:       refreshBatch,
		RefreshTimeoutSec:  refreshTimeout,
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
	// 构造期注入的值来自装配根（启动时读到的就是它们），与现值不一致 = 改了还没重启。
	v.BaseURLEffective = src.BootBaseURL
	v.RequestTimeoutSecEffective = src.BootRequestTimeoutSec
	v.RestartRequired = baseErr == nil && timeoutErr == nil &&
		(baseURL != src.BootBaseURL || timeoutSec != src.BootRequestTimeoutSec)

	// 三项都读出来再判原因：读失败的那一项会盖掉默认值，必须让人看见。
	switch {
	case enabledErr != nil:
		v.Reason = settingErrReason(key(store.PluginItemEnabled), enabledErr)
	case baseErr != nil:
		v.Reason = settingErrReason(key(store.PluginItemBaseURL), baseErr)
	case timeoutErr != nil:
		v.Reason = settingErrReason(key(store.PluginItemRequestTimeout), timeoutErr)
	case !v.Enabled:
		// enabled=false 只管调度：手动同步（POST /api/dramas）与播放（/video）仍然可用。
		v.Reason = "已在 settings 停用（" + string(key(store.PluginItemEnabled)) +
			"=false）：不参与刷新调度；手动同步与播放仍可用"
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

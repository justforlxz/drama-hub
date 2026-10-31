// Package main assembles the drama-hub server. No business logic here.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/plugin/hongguo"
	"github.com/justforlxz/drama-hub/internal/scheduler"
	"github.com/justforlxz/drama-hub/internal/segment"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
	"github.com/justforlxz/drama-hub/internal/sync"
	"github.com/justforlxz/drama-hub/internal/vfs"
	"github.com/justforlxz/drama-hub/internal/web"
	"github.com/justforlxz/drama-hub/internal/webdav"
)

// Build-time variables, overridden via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	// Startup wiring only. Panics are acceptable during assembly; a panic on a
	// request path is a defect.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(envOr("DRAMA_HUB_LOG_LEVEL", "info")),
	}))
	slog.SetDefault(logger)
	logger.Info("starting drama-hub", "version", version, "commit", commit)

	dbPath := envOr("DRAMA_HUB_DB", "./data/drama.db")
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logger.Error("创建数据库目录失败", "dir", dir, "err", err)
			os.Exit(1)
		}
	}
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		logger.Error("打开数据库失败", "path", dbPath, "err", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	settings := store.NewSettings(db.DB)
	// 空卷的首次配置入口：env → settings，只在键缺失时写一次（重复启动不覆盖手改值）。
	bootstrapSettings(context.Background(), settings, logger)
	// 拼错的 DRAMA_HUB_* 会被静默忽略，这本身就是「配了没生效」；点名报出来。
	warnUnknownEnv(logger)
	plugins, boots, err := newPlugins(context.Background(), settings, logger)
	if err != nil {
		logger.Error("注册插件失败", "err", err)
		os.Exit(1)
	}

	st := store.NewStore(db.DB)
	sourcePlugins := pluginList(plugins)
	syncers := newSyncers(sourcePlugins, st, settings, logger)
	syncer := syncers[hongguo.Name]
	sched := scheduler.New(st, settings, sourcePlugins, syncers, logger)

	// 本地缓存（海报/分片/索引）与播放面的直链解析都只有一份实例：请求路径与
	// 淘汰循环共用同一个 *cache.Cache，直链解析共用同一个 *stream.Resolver。
	caches := cache.New(st, settings, logger)
	// 空卷首次启动就把 <cache_dir> 的四个目录建出来：cache_dir 配错在启动日志里
	// 就有一条 warn，不用等第一次播放。失败不挡启动（缓存坏了不影响只读的 WebDAV 面）。
	if err := caches.EnsureLayout(context.Background()); err != nil {
		logger.Warn("缓存目录不可用", "err", err)
	}
	resolver := stream.NewResolver(st, plugins)

	// 后台写操作的令牌（POST / DELETE / PATCH）。没配就 fail closed：写端点一律 401，
	// 状态页会说明「未设置 DRAMA_HUB_ADMIN_TOKEN，后台改配置不可用」。
	adminToken := os.Getenv("DRAMA_HUB_ADMIN_TOKEN")
	if adminToken == "" {
		logger.Warn("未设置 DRAMA_HUB_ADMIN_TOKEN：后台写操作（同步/删除/改配置）一律 401")
	}
	addr := envOr("DRAMA_HUB_ADDR", ":8080")
	srv := &http.Server{
		Addr: addr,
		Handler: newHandler(logger, st, settings, syncer, sched, sourceList(plugins, boots), caches, resolver,
			playbackHeaders(boots), adminToken),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 缓存淘汰与 HTTP 服务同寿命：启动即跑一轮，之后每 5 分钟一轮，ctx 一取消就停
	// （docs/CACHE-MODEL.md「淘汰」）。与请求路径无关，失败只记录、下一轮重试。
	go caches.RunEviction(ctx)

	// 调度器与 HTTP 服务同寿命：ctx 一取消它就停派新轮，并等手上的轮收尾。
	schedCtx, stopScheduler := context.WithCancel(ctx)
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		sched.Run(schedCtx)
	}()
	defer func() {
		stopScheduler()
		select {
		case <-schedulerDone:
		case <-time.After(schedulerStopTimeout):
			logger.Warn("调度器未在时限内退出", "timeout", schedulerStopTimeout)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server exited", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown failed", "err", err)
			os.Exit(1)
		}
	}
}

// newHandler 装配顶层路由。cmd 是唯一的装配根：路由表在这里一眼可见，
// 领域包只提供自己的 handler，不关心挂载点。
//
// 播放面（docs/PLAYBACK-CONTRACT.md）只有两个口子：`/video/{id}.m3u8` 与
// `/video/{id}/{seq}.ts`，单集与合并集同一路径形态，id 自带 HMAC 签名、不要 Basic。
// `/webdav/` 是只读元数据面。1.0 的 `/dav/` 与 `/s/{token}/*` 已退役，不留兼容层
// （`.strm` 是按请求渲染的，下一次读取就是新内容）。
//
// 管理面挂在 `/` 上：`internal/web` 自己按完整路径分派三页界面与 `/api/` 子树，
// 未登记路径由它返回 404。/webdav/ 与 /video/ 的 pattern 比 `/` 更具体，不受影响。
//
// 请求日志中间件只在装配根挂一次：/healthz、/webdav/、/video/、/ 与未知路径都经它，
// 每请求恰一条访问日志（见 internal/httpx）。
//
// playbackHeaders 是播放面取直链时 ffmpeg / ffprobe 要带的出站身份（`名称: 值`
// 每项一行），由 main 从 pluginBoot 派生后传入（MKACG-624）：本函数不认识具体源。
func newHandler(logger *slog.Logger, st *store.Store, settings *store.Settings, syncer web.SeriesSyncer, schedule web.Schedule, sources []web.Source, caches *cache.Cache, resolver *stream.Resolver, playbackHeaders []string, adminToken string) http.Handler {
	cutter := segment.NewCutter(st, settings, logger, caches, resolver.DirectURL, playbackHeaders)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle(webdav.MountPath, webdav.NewHandler(vfs.NewTree(st, settings, signID, logger), settings, logger))
	// 播放面两条路由由 internal/segment 自己提供（领域包只提供 handler，挂载点在此）。
	mux.HandleFunc(segment.PlaylistRoute, cutter.ServePlaylist)
	mux.HandleFunc(segment.SegmentRoute, cutter.ServeSegment)
	mux.Handle("/", web.NewServer(st, settings, syncer, schedule, sources, adminToken, logger))
	return httpx.RequestLogger(logger, mux)
}

// schedulerStopTimeout 是关闭时等调度器收尾的上限：手上的轮收到 ctx 取消后只剩
// 一次状态写回，正常远小于这个值。
const schedulerStopTimeout = 5 * time.Second

// pluginList 按注册顺序把注册表翻成 []plugin.Plugin（调度器按它决定调度哪些源）。
func pluginList(reg *plugin.Registry) []plugin.Plugin {
	out := make([]plugin.Plugin, 0, len(reg.Names()))
	for _, name := range reg.Names() {
		out = append(out, mustPlugin(reg, name))
	}
	return out
}

// newSyncers 给每个源造一个写库编排（一个源的剧只写进它自己的 provider）。
//
// 海报本地化对每个源共用同一个 cache.CoverCache：落盘布局按 (provider, provider_id)
// 分文件，不同源不会互相覆盖。
//
// 封面出站的超时不在这里定型：cache 每次下载现读 settings 的 cover_timeout_sec
// （MKACG-620）。它不跟插件超时走 —— 封面 URL 是上游页面里的三方 CDN 绝对地址
// （`p3-novel.byteimg.com`），插件 base_url 与 request_timeout_sec 管不到这条出站；
// client 因此不带 Timeout，超时由每次请求的 ctx 决定（DEV-STANDARDS「错误处理」）。
func newSyncers(sources []plugin.Plugin, st *store.Store, settings *store.Settings, logger *slog.Logger) map[string]scheduler.Syncer {
	covers := cache.NewCoverCache(st, settings, &http.Client{})
	out := make(map[string]scheduler.Syncer, len(sources))
	for _, p := range sources {
		out[p.Name()] = sync.New(p, covers, logger)
	}
	return out
}

// 插件配置的兜底值（契约默认值见 docs/PLUGIN-CONTRACT.md「配置」）。
const defaultPluginTimeoutSec = 30

// pluginBoot 是一个源在启动时真正注入插件的构造期配置。
//
// 插件契约硬性要求 2：插件只保存构造期配置，`*http.Client` 的 Timeout 在这里定型
// （docs/PLUGIN-CONTRACT.md）。所以 base_url / request_timeout_sec 不是热读项，
// 改值要重启才生效；状态页拿它显示「生效值」与 restart_required（api_status.go）。
type pluginBoot struct {
	BaseURL    string // settings 原值；"" = 插件内置默认（hongguo.DefaultBaseURL）
	TimeoutSec int
}

// newPlugins 显式注册内置插件（ADR-0006 第 5 条：没有 init() 自动注册，装配根逐个
// Register）。返回的 map 按源的 Name() 记录「启动时真正注入的配置」，只给状态页用。
//
// 配置读不出来（值非法、库被手改）时用插件默认值并 warn：配置错不该让服务起不来，
// 原因与现状在状态页看得见（plugin.<源>.* 三行）。
//
// 播放面的 ffmpeg `-headers` 也从这里的 `pluginBoot.BaseURL` 派生（MKACG-624）：
// 身份只有一份（插件里的常量），地址也只有一份（boots），不再多传一个参数。
func newPlugins(ctx context.Context, settings *store.Settings, logger *slog.Logger) (*plugin.Registry, map[string]pluginBoot, error) {
	baseURL, err := settings.String(ctx, store.PluginSettingKey(hongguo.Name, store.PluginItemBaseURL))
	if err != nil {
		logger.Warn("读插件 base_url 失败，用插件内置地址", "plugin", hongguo.Name, "err", err)
		baseURL = ""
	}
	timeoutSec, err := settings.Int(ctx, store.PluginSettingKey(hongguo.Name, store.PluginItemRequestTimeout))
	if err != nil {
		logger.Warn("读插件 request_timeout_sec 失败，用默认值", "plugin", hongguo.Name, "err", err)
		timeoutSec = defaultPluginTimeoutSec
	}
	reg := plugin.NewRegistry()
	if err := reg.Register(hongguo.New(hongguo.Config{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
		Logger:     logger,
	})); err != nil {
		return nil, nil, err
	}
	boots := map[string]pluginBoot{
		hongguo.Name: {BaseURL: baseURL, TimeoutSec: timeoutSec},
	}
	return reg, boots, nil
}

// playbackHeaders 把启动时注入给插件的配置翻成播放面的出站身份（MKACG-624）：
// 身份（UA / Accept / …）的真值在插件里，地址取 boots 里那份 base_url ——
// 与页面抓取用的是同一个值，不另传一个地址参数，也不在 segment 里写第二份 UA。
//
// ffmpeg / ffprobe 不走 http.Client，只能靠 `-headers`（docs/PLAYBACK-CONTRACT.md）。
func playbackHeaders(boots map[string]pluginBoot) []string {
	return hongguo.PlaybackHeaderLines(boots[hongguo.Name].BaseURL)
}

// mustPlugin 取已注册的插件。名字写死在内置清单里，取不到只可能是装配 bug，
// 启动阶段 panic 是允许的（DEV-STANDARDS「错误处理」）。
func mustPlugin(reg *plugin.Registry, name string) plugin.Plugin {
	p, ok := reg.Get(name)
	if !ok {
		panic("装配错误：插件 " + name + " 未注册")
	}
	return p
}

// sourceList 把注册表里的源翻成状态页要展示的清单（标签是人看的，不放注册表），
// 并带上启动时真正注入插件的构造期配置——状态页要拿它和 settings 现值对比，
// 说明「这个值已经生效」还是「改了要重启」。
func sourceList(reg *plugin.Registry, boots map[string]pluginBoot) []web.Source {
	labels := map[string]string{hongguo.Name: "红果官网 SSR"}
	out := make([]web.Source, 0, len(reg.Names()))
	for _, name := range reg.Names() {
		boot := boots[name]
		out = append(out, web.Source{
			Name:                  name,
			Label:                 labels[name],
			BootBaseURL:           boot.BaseURL,
			BootRequestTimeoutSec: boot.TimeoutSec,
		})
	}
	return out
}

// handleHealthz 是存活探测：只证明进程还能接请求。
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

// signID 把 stream.ID 适配为 vfs.IDFunc：vfs（rank 30）只声明它需要一个「(secret, uid)
// → 对外播放 id」的函数，算法在装配根接上（id 的生产方与校验方共用 stream.ID）。
func signID(secret, uid string) string { return stream.ID(secret, uid) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func logLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

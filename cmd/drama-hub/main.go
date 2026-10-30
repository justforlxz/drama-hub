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
	plugins, err := newPlugins(context.Background(), settings, logger)
	if err != nil {
		logger.Error("注册插件失败", "err", err)
		os.Exit(1)
	}

	st := store.NewStore(db.DB)
	covers := cache.NewCoverCache(st, settings, &http.Client{Timeout: coverTimeout})
	syncer := sync.New(mustPlugin(plugins, hongguo.Name), covers)

	addr := envOr("DRAMA_HUB_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(logger, st, settings, syncer, sourceList(plugins)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
// /healthz、/dav/（M4 只读 WebDAV）、/s/（M5 出流 + M6 分片 + M7 合并集）与 `/`
// （R1 管理面：三页界面 + `/api/` 管理 API）都已接上真实实现。`/s/` 子树按 pattern
// 具体度分工：分片端点、playlist 端点比 `/s/` 更具体，其余落到 stream.Server 的
// `/s/` 兜底（单集出流或 501）。领域包之间互不 import（同 rank），跨包的签名/分派
// 在装配根接上。
//
// 管理面挂在 `/` 上：`internal/web` 自己按完整路径分派三页界面与 `/api/` 子树，
// 未登记路径由它返回 404（「未知路径 → 404」的语义不变，只是兜底者从顶层 mux
// 换成了 web 的 mux）。/dav/ 与 /s/ 的 pattern 比 `/` 更具体，不受影响。
//
// 请求日志中间件只在装配根挂一次：/healthz、/dav/、/s/、/ 与未知路径都经它，
// 每请求恰一条访问日志（token 段掩码，见 internal/httpx）。
func newHandler(logger *slog.Logger, st *store.Store, settings *store.Settings, syncer web.SeriesSyncer, sources []web.Source) http.Handler {
	cutter := segment.NewCutter(st, settings, logger, stream.Verify)
	streamSrv := stream.NewServer(st, settings, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle(webdav.MountPath, webdav.NewHandler(vfs.NewTree(st, settings, signToken), settings, logger))
	// 出流首个 URL（DESIGN §5.2）：uid 是合并集句柄时返回自造 m3u8，否则走单集 MP4
	// 透传。Go 的 pattern 不能在路径段内按前缀分派，所以在这里按 uid 分派（cmd 可
	// import 全部 internal 包，两个领域包仍互不 import）。
	mux.HandleFunc("GET /s/{token}/e/{uid}", func(w http.ResponseWriter, r *http.Request) {
		if segment.IsMergedUID(r.PathValue("uid")) {
			cutter.ServePlaylist(w, r)
			return
		}
		streamSrv.ServeHTTP(w, r)
	})
	// 合并集 playlist 的标准别名（部分客户端会自行追加 /master.m3u8）。
	mux.HandleFunc(segment.PlaylistRoute, func(w http.ResponseWriter, r *http.Request) {
		if !segment.IsMergedUID(r.PathValue("uid")) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		cutter.ServePlaylist(w, r)
	})
	// 分片端点：签名校验用 stream.Verify 注入（segment 不能 import 同 rank 的 stream）。
	mux.Handle(segment.Route, cutter)
	// `/s/` 兜底：未单独注册的 `/s/` 子路径仍是 501（「已规划未实现」的既有语义）。
	mux.Handle("/s/", streamSrv)
	mux.Handle("/", web.NewServer(st, settings, syncer, sources, logger))
	return httpx.RequestLogger(logger, mux)
}

// 插件配置的兜底值（契约默认值见 docs/PLUGIN-CONTRACT.md「配置」）。
const defaultPluginTimeoutSec = 30

// coverTimeout 是海报下载单次出站超时。海报是百 KB 量级的图片、来自 CDN，
// 用插件超时（30 s）会让一张卡住的图拖住整部剧的同步，因此单独取更短的值。
// 它是代码常量而非 settings：没有用户会为「下载海报等多久」调参（硬约束 5 的反面）。
const coverTimeout = 15 * time.Second

// newPlugins 显式注册内置插件（ADR-0006 第 5 条：没有 init() 自动注册，装配根逐个
// Register）。插件配置现读 settings（硬约束 5）：base_url 空 = 用插件内置地址，
// request_timeout_sec 决定出站 client 的超时。
//
// 配置读不出来（值非法、库被手改）时用插件默认值并 warn：配置错不该让服务起不来，
// 原因与现状在状态页看得见（plugin.<源>.* 三行）。
func newPlugins(ctx context.Context, settings *store.Settings, logger *slog.Logger) (*plugin.Registry, error) {
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
		return nil, err
	}
	return reg, nil
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

// sourceList 把注册表里的源翻成状态页要展示的清单（标签是人看的，不放注册表）。
func sourceList(reg *plugin.Registry) []web.Source {
	labels := map[string]string{hongguo.Name: "红果官网 SSR"}
	out := make([]web.Source, 0, len(reg.Names()))
	for _, name := range reg.Names() {
		out = append(out, web.Source{Name: name, Label: labels[name]})
	}
	return out
}

// handleHealthz 是存活探测：只证明进程还能接请求。
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

// signToken 把 stream.Sign 适配为 vfs.TokenFunc：vfs（rank 30）不 import stream
// （rank 40），签名算法在装配根接上，见 docs/DEV-STANDARDS.md「虚拟文件树与渲染」。
func signToken(secret, uid string) string { return stream.Sign([]byte(secret), uid) }

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

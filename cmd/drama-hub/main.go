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

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
	"github.com/justforlxz/drama-hub/internal/web"
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

	addr := envOr("DRAMA_HUB_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(logger, store.NewStore(db.DB), store.NewSettings(db.DB)),
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
// /healthz 与 /api/ 之外的挂载点返回 501（已规划未实现）；/s/ 已接入出流，
// 其子树内未实现的路径（分片、合并集）由 stream.Server 自己兜底 501。不注册 "/"
// catch-all —— 未装配路径由 ServeMux 自然兜底 404，这是「未知路径 → 404」的前提。
//
// 请求日志中间件只在装配根挂一次：/healthz、/dav/、/s/、/api/（含 web 内部）
// 与未知路径都经它，每请求恰一条访问日志（token 段掩码，见 internal/httpx）。
func newHandler(logger *slog.Logger, st *store.Store, settings *store.Settings) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("/dav/", handleNotImplemented)
	mux.Handle("/s/", stream.NewServer(st, settings, logger))
	mux.Handle("/api/", web.NewServer(logger))
	return httpx.RequestLogger(logger, mux)
}

// handleHealthz 是存活探测：只证明进程还能接请求。
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

// handleNotImplemented 表示挂载点已规划、实现属后续阶段。
// pattern 不带 method 前缀，任意方法命中同一语义，而不是 405。
func handleNotImplemented(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
}

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

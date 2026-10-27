// Package main assembles the drama-hub server. No business logic here.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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

	addr := envOr("DRAMA_HUB_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           web.NewServer(logger),
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

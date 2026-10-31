package httpx

import (
	"log/slog"
	"net/http"
	"time"
)

// RequestLogger 在装配根包住 next：记录开始时间、捕获响应状态码，handler 返回
// 后写一条结构化访问日志。字段固定为 method / path / status / duration_ms；
// path 只取 r.URL.Path（天然不含 query），不记 header / body。
//
// 2.0 之后 URL 里没有凭据段：播放面的 `/video/{id}` 是签名 id（`e42-<12 位十六进制>`
// 本身不是密钥），WebDAV 凭据在 Authorization 头里。因此不再需要按路径段掩码。
//
// 只允许在装配根挂一次：在子树上再挂一层会让同一请求产生两条日志。
func RequestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.LogAttrs(r.Context(), levelForStatus(rec.status), "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		)
	})
}

// statusRecorder 捕获 handler 显式写下的状态码；handler 未调用 WriteHeader 时
// 按 net/http 的隐式语义记为 200。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader 记录状态码后透传给底层 ResponseWriter。
func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// levelForStatus 按响应类别定级别：5xx → Error，4xx → Warn，其余 Info。
func levelForStatus(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

package httpx

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// RequestLogger 在装配根包住 next：记录开始时间、捕获响应状态码，handler 返回
// 后写一条结构化访问日志。字段固定为 method / path / status / duration_ms；
// path 经 redactPath 掩码，不记 query / header / body。
//
// 只允许在装配根挂一次：在子树上再挂一层会让同一请求产生两条日志。
func RequestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.LogAttrs(r.Context(), levelForStatus(rec.status), "http request",
			slog.String("method", r.Method),
			slog.String("path", redactPath(r.URL.Path)),
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

// redactPath 掩码出流路径里的 token 段：以 /s/ 开头时，把紧随其后的一段替换为
// {token}。依据 ADR-0003，token 在 URL 路径里且 cache-key 稳定性要求它可见，
// 因此只能按路径段掩码；日志只记 r.URL.Path（天然不含 query），再从根上杜绝
// query 里的 token / 签名泄漏。
func redactPath(path string) string {
	rest, ok := strings.CutPrefix(path, "/s/")
	if !ok || rest == "" {
		return path
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return "/s/{token}" + rest[i:]
	}
	return "/s/{token}"
}

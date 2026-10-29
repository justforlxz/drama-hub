package stream

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/justforlxz/drama-hub/internal/httpx"
)

// upstreamHeaders 是唯一允许从上游回传给客户端的头。其余上游头一律不透传：
// 本服务只承诺「字节 + 长度 + 范围」，多传的头会让缓存代理与 Emby 看到本服务
// 不承诺的语义。
var upstreamHeaders = []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"}

// newUpstreamClient 构造出站客户端。
//
// DisableCompression 是硬要求：Go 默认 Transport 会给无 Range 的 GET 自动加
// Accept-Encoding: gzip 并透明解压，从而抹掉响应里的 Content-Length ——
// 「HEAD/GET 长度一致」直接失效（本地 httptest 对照实验实测）。
//
// 不设 Client.Timeout：那是对整个响应体（含长视频）计时，会截断大文件。
// 连不上 / 不回头由 DialContext 与 ResponseHeaderTimeout 覆盖，body 传输交给
// 请求 ctx（客户端断开即取消），满足「出站 HTTP 一律带 timeout 或 ctx 超时」。
func newUpstreamClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableCompression:    true,
	}}
}

// proxy 把上游字节中继给客户端：方法原样（GET/HEAD）、Range 原样透传。
//
// 状态码只在 200 / 206 / 416 时透传，其余（含 403/404）一律 502 且不转发上游
// body。Range 不在本地解析：206 与越界的 416 都由上游判定（实测上游两者都正确）。
// Go 默认 client 在内部跟随上游重定向，任何 3xx 都不会发给客户端（硬约束 7）。
//
// 先拿到上游响应并定下状态码，再写任何客户端字节 —— 所有上游失败都产出干净的
// 502，绝不会先写 200 再失败。已知边界：上游在 body 传输中途失败时响应头已发出，
// 只能截断，不为此引入全量缓冲。
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, uid, playURL string) {
	ctx := r.Context()
	upstreamStatus := 0
	var written int64
	defer func() { s.logUpstream(ctx, uid, playURL, upstreamStatus, written) }()

	req, err := http.NewRequestWithContext(ctx, r.Method, playURL, nil)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("构造上游请求: %w", httpx.ErrUpstream))
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// 客户端已断开：连接没了，写任何响应都是噪声，只记日志。
			return
		}
		httpx.WriteError(w, fmt.Errorf("请求上游: %w", httpx.ErrUpstream))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	upstreamStatus = resp.StatusCode

	switch upstreamStatus {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
		for _, key := range upstreamHeaders {
			if v := resp.Header.Get(key); v != "" {
				w.Header().Set(key, v)
			}
		}
		w.WriteHeader(upstreamStatus)
		if r.Method == http.MethodHead {
			return // HEAD 不读上游 body，但保留上游的 Content-Length
		}
		n, copyErr := io.Copy(w, resp.Body)
		written = n
		if copyErr != nil {
			// 头已发出，无法改写成 502；只记录。
			s.logger.WarnContext(ctx, "stream body 传输中断", "uid", uid, "err", copyErr)
		}
	default:
		httpx.WriteError(w, fmt.Errorf("上游返回 %d: %w", resp.StatusCode, httpx.ErrUpstream))
	}
}

// logUpstream 记录一条出站上游日志：uid / upstream_host / upstream_status / bytes。
//
// 禁止记录完整 play_url（带签名）与 token；定位需要路径时按 DEV-STANDARDS 只
// 记 host + 路径前 32 字符，这里只取 host。
func (s *Server) logUpstream(ctx context.Context, uid, playURL string, status int, written int64) {
	host := ""
	if u, err := url.Parse(playURL); err == nil {
		host = u.Host
	}
	level := slog.LevelInfo
	if status < 200 || status >= 400 {
		level = slog.LevelError
	}
	s.logger.LogAttrs(ctx, level, "stream upstream",
		slog.String("uid", uid),
		slog.String("upstream_host", host),
		slog.Int("upstream_status", status),
		slog.Int64("bytes", written),
	)
}

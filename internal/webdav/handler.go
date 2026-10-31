package webdav

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	xwebdav "golang.org/x/net/webdav"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/vfs"
)

// MountPath 是挂载点。cmd 用它注册路由，Handler 用它生成 PROPFIND 的 href
// （RFC 4918 要求 href 是绝对路径；Prefix 交给 x/net/webdav 处理，不做 StripPrefix）。
const MountPath = "/webdav/"

// Handler 是只读 WebDAV 端点的 http.Handler：Basic 认证 → x/net/webdav → vfs.Tree。
//
// 认证用 settings 的 webdav_user / webdav_pass，每请求现读（硬约束 5：改值即时生效，
// 不缓存）。协议部分委托 x/net/webdav，本包只提供 FileSystem 与认证。
type Handler struct {
	settings *store.Settings
	inner    *xwebdav.Handler
}

// NewHandler 装配 WebDAV 端点。settings 不可为 nil；logger 只用于协议层错误
// （正常请求不产生第二条日志——访问日志归装配根的 httpx.RequestLogger）。
func NewHandler(tree Tree, settings *store.Settings, logger *slog.Logger) *Handler {
	h := &Handler{settings: settings}
	h.inner = &xwebdav.Handler{
		Prefix:     MountPath,
		FileSystem: NewFS(tree),
		// 锁定是协议层要求（rclone 会发 LOCK/UNLOCK）；本服务只读，内存锁足够，
		// 不落盘、也无跨进程语义（硬约束 4：单二进制单实例）。
		LockSystem: xwebdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			// x/net/webdav 只把状态码写回客户端，失败详情不经它输出；仅在出错时记一条，
			// 避免每个正常请求多一条日志（DEV-STANDARDS：每请求恰一条访问日志）。
			if err != nil {
				logger.Warn("webdav 协议错误", "method", r.Method, "path", r.URL.Path, "err", err)
			}
		},
	}
	return h
}

// ServeHTTP 先认证，再挂上请求内树快照转交协议处理器。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		// WWW-Authenticate 是 Basic 方案的要求：缺了它，rclone / 浏览器不会重试带凭据。
		w.Header().Set("WWW-Authenticate", `Basic realm="drama-hub", charset="UTF-8"`)
		httpx.WriteError(w, httpx.ErrUnauthorized)
		return
	}
	// B1：.strm 在 strm_prefix 为空时要写出绝对地址，基地址只能从请求里来。
	ctx := vfs.WithBaseURL(r.Context(), requestBaseURL(r))
	h.inner.ServeHTTP(w, r.WithContext(withSnapshot(ctx)))
}

// authorized 校验 Basic 凭据。
//
// 凭据取 settings（每请求现读，改值即时生效）。读失败（必填项缺失）不算「认证通过」，
// 一律 401 而不是 500：库没配好时不应把挂载点敞开。比较用 constant-time，避免按字节
// 短路泄漏长度/前缀信息。
func (h *Handler) authorized(r *http.Request) bool {
	user, err := h.settings.String(r.Context(), store.KeyWebDAVUser)
	if err != nil {
		return false
	}
	pass, err := h.settings.String(r.Context(), store.KeyWebDAVPass)
	if err != nil {
		return false
	}
	gotUser, gotPass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(user)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(gotPass), []byte(pass)) == 1
	return userOK && passOK
}

// requestBaseURL 由请求推导本服务的对外基地址 `scheme://host`。
//
// 反向代理后 Host 是外部域名、真实 scheme 在 X-Forwarded-Proto 里；两者都没有时按
// 连接是否 TLS 判断。Host 为空（不合法的 HTTP/1.0 请求）返回空串，渲染退回相对路径。
func requestBaseURL(r *http.Request) string {
	if r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = normalizeProto(proto)
	}
	return scheme + "://" + r.Host
}

// normalizeProto 取 X-Forwarded-Proto 的第一个值（代理链上会是 `https,http`）。
func normalizeProto(raw string) string {
	proto, _, _ := strings.Cut(raw, ",")
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto == "" {
		return "http"
	}
	return proto
}

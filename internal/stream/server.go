package stream

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
)

// Server 是单集出流端点的 http.Handler。它自带 ServeMux，完整路径（含 `/s/`
// 前缀）原样传入本包，由 cmd 把该 Server 挂到 `/s/` 上 —— 与 web.Server 对
// `/api/` 的约定一致。
type Server struct {
	store    *store.Store
	settings *store.Settings
	logger   *slog.Logger
	client   *http.Client
	mux      *http.ServeMux
}

// NewServer 装配出流路由。st / settings 不可为 nil；logger 用于出站上游日志。
//
// 只注册一个端点：GET /s/{token}/e/{uid}（Go 1.22 的 GET 模式同时匹配 HEAD，
// 无需为 HEAD 单独注册）。`/s/` 子树其余路径保持 501（已规划未实现），分片与
// 合并集属后续里程碑；不注册任何 catch-all 到 `/s/` 之外。
func NewServer(st *store.Store, settings *store.Settings, logger *slog.Logger) *Server {
	s := &Server{
		store:    st,
		settings: settings,
		logger:   logger,
		client:   newUpstreamClient(),
		mux:      http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /s/{token}/e/{uid}", s.handleEpisode)
	s.mux.HandleFunc("/s/", handleNotImplemented)
	return s
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleNotImplemented 表示 `/s/` 子树里已规划、尚未实现的路径（分片、合并集）。
// 不带 method 前缀 → 任意方法是 501 而不是 405。
func handleNotImplemented(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
}

// uidPattern 是单集 uid 的形态：`e` + episode.id 的十进制。uid 派生自稳定的
// episode.id（不是幂等重建的 emby_item），签名 URL 的缓存键才长期稳定。
// 合并集 uid 由 M7 定义，不得匹配本模式（DESIGN D3 要求 uid 能区分内容类型）。
var uidPattern = regexp.MustCompile(`^e([0-9]+)$`)

// parseEpisodeUID 把 uid 解析为 episode.id；不匹配 `^e[0-9]+$` 或超出 int64
// 范围返回 false，调用方映射 404。
func parseEpisodeUID(uid string) (int64, bool) {
	m := uidPattern.FindStringSubmatch(uid)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// handleEpisode 处理单集出流：鉴权 → 解析 uid → 查集 → 中继上游字节。
//
// 状态码矩阵见 docs/DEV-STANDARDS.md「出流（stream）」：secret 读失败 500、
// 签名不通过 401、uid 非法或剧集不存在 404、play_url 为空 502、上游失败 502。
// 错误文本里不得出现 token 或 secret。
func (s *Server) handleEpisode(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	uid := r.PathValue("uid")

	// 每请求读 secret，不缓存：改 settings 后旧 URL 立即失效（硬约束 5），
	// 这正是 ADR-0003 的既定后果。
	secret, err := s.settings.String(r.Context(), store.KeyStreamSecret)
	if err != nil {
		// 错误文本只带 key 名，store 不会回显配置值。
		httpx.WriteError(w, fmt.Errorf("读取出流密钥: %w", err))
		return
	}
	if !Verify([]byte(secret), uid, token) {
		httpx.WriteError(w, httpx.ErrUnauthorized)
		return
	}

	id, ok := parseEpisodeUID(uid)
	if !ok {
		httpx.WriteError(w, fmt.Errorf("uid %q 不是单集句柄: %w", uid, httpx.ErrNotFound))
		return
	}
	ep, err := s.store.EpisodeByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, fmt.Errorf("剧集 %s: %w", uid, httpx.ErrNotFound))
		return
	}
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("查剧集 %s: %w", uid, err))
		return
	}
	if ep.PlayURL == "" {
		httpx.WriteError(w, fmt.Errorf("剧集 %s 没有播放地址: %w", uid, httpx.ErrUpstream))
		return
	}
	s.proxy(w, r, uid, ep.PlayURL)
}

package web

import (
	"log/slog"
	"net/http"
)

// Server 汇总管理 API 的 HTTP 路由。它实现 http.Handler，由 cmd 挂到
// `/api/` 前缀下；完整路径（含 `/api` 前缀）原样传到本包，ServeMux 不剥前缀。
type Server struct {
	logger *slog.Logger
	mux    *http.ServeMux
}

// NewServer 构造路由表。logger 用于请求与上游日志，不可为 nil。
func NewServer(logger *slog.Logger) *Server {
	s := &Server{logger: logger, mux: http.NewServeMux()}
	// 框架阶段只有占位：管理 API 整棵子树「已规划未实现」，真实端点属后续阶段。
	// 不带 method 前缀 → 任意方法、任意子路径都是 501，而不是 405。
	s.mux.HandleFunc("/", s.handleNotImplemented)
	return s
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleNotImplemented(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
}

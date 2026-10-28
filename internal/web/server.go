package web

import (
	"log/slog"
	"net/http"
)

// Server 汇总全部 HTTP 路由。它实现 http.Handler，可直接交给 http.Server。
type Server struct {
	logger *slog.Logger
	mux    *http.ServeMux
}

// NewServer 构造路由表。logger 用于请求与上游日志，不可为 nil。
func NewServer(logger *slog.Logger) *Server {
	s := &Server{logger: logger, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	return s
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

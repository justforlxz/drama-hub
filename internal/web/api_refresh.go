package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/scheduler"
)

// refreshRequest 是 POST /api/refresh 的请求体。
type refreshRequest struct {
	Source string `json:"source"`
}

// refreshView 是 202 响应体：只回执「哪个源的一轮已经被派出去」。
type refreshView struct {
	Source  string `json:"source"`
	Started bool   `json:"started"`
}

// handleRefresh 处理 POST /api/refresh：立刻跑一轮该源，不等 next_due。
//
// 状态码按 docs/SCHEDULER-MODEL.md「手动触发」：202 已启动 / 404 无此源或未启用 /
// 409 该源正在跑（手动与定时共用一把单飞锁）。202 只表示「派出去了」，这一轮的
// 结果在状态页的「上次运行 / 上次错误」里看。
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, err)
		return
	}
	if err := s.schedule.Trigger(r.Context(), req.Source); err != nil {
		httpx.WriteError(w, mapScheduleErr(err))
		return
	}
	writeJSON(w, http.StatusAccepted, refreshView{Source: req.Source, Started: true})
}

// mapScheduleErr 把调度器的哨兵接到传输层类别：源不存在 / 已停用 → 404，
// 正在跑 → 409，其余（配置读不出来等）原样返回（StatusCode 兜底 500）。
func mapScheduleErr(err error) error {
	switch {
	case errors.Is(err, scheduler.ErrSourceNotFound), errors.Is(err, scheduler.ErrSourceDisabled):
		return fmt.Errorf("%w: %v", httpx.ErrNotFound, err)
	case errors.Is(err, scheduler.ErrSourceBusy):
		return fmt.Errorf("%w: %v", httpx.ErrConflict, err)
	default:
		return err
	}
}

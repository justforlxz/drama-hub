package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/jellyfin"
)

// jellyfinLibrariesRequest 是 POST /api/jellyfin/libraries 的请求体，可以整个省略。
//
// prune 默认 false：分类消失时只报告孤儿库、不删。删除是显式动作 —— 库里可能有用户
// 手建的库，误删的代价远大于多留一个孤儿。prune 不进页面 UI（破坏性动作只走 API）。
type jellyfinLibrariesRequest struct {
	Prune bool `json:"prune"`
}

// jellyfinLibrariesResponse 是触发成功的响应体：202 只表示「一轮已派出去」。
type jellyfinLibrariesResponse struct {
	Started bool `json:"started"`
}

// handleJellyfinLibraries 处理 POST /api/jellyfin/libraries：派一轮媒体库同步。
//
// 202 已派出 / 409 上一轮还没结束 / 400 三项配置没配齐（body 指出缺哪个键）。
// 结果不在响应里：跑完看 GET /api/jellyfin/libraries（页面轮询的就是它，MKACG-664），
// /api/status 的 jellyfin 块是同一份视图。
func (s *Server) handleJellyfinLibraries(w http.ResponseWriter, r *http.Request) {
	var req jellyfinLibrariesRequest
	// 请求体可省：curl 不带 body 也要能触发（破坏性动作必须显式给 body）。
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			httpx.WriteError(w, err)
			return
		}
	}
	if err := s.jellyfin.Start(r.Context(), req.Prune); err != nil {
		httpx.WriteError(w, mapJellyfinErr(err))
		return
	}
	writeJSON(w, http.StatusAccepted, jellyfinLibrariesResponse{Started: true})
}

// handleJellyfinLibrariesState 处理 GET /api/jellyfin/libraries：一轮同步的现状与进度。
//
// 页面在派发后轮询它（MKACG-664），所以刻意不去读 drama / episode 表 —— 那部分由
// /api/status 负责，一秒一次的轮询只该读内存状态与五项配置。刷新页面后读到的仍是
// 同一份状态，这就是「上次同步：时间 + 结果」常驻的来源。
func (s *Server) handleJellyfinLibrariesState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, jellyfinViewOf(s.jellyfin.State(r.Context())))
}

// mapJellyfinErr 把 jellyfin 包的哨兵接到传输层：
// 未配置 → 400（错误文本原样带回，点出缺哪个键），正在跑 → 409，其余 → 500。
func mapJellyfinErr(err error) error {
	switch {
	case errors.Is(err, jellyfin.ErrNotConfigured):
		return fmt.Errorf("%w: %v", httpx.ErrInvalid, err)
	case errors.Is(err, jellyfin.ErrBusy):
		return fmt.Errorf("%w: %v", httpx.ErrConflict, err)
	default:
		return err
	}
}

package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/jellyfin"
	"github.com/justforlxz/drama-hub/internal/store"
)

// jellyfinLibrariesResponse 是触发成功的响应体：202 只表示「一轮已派出去」。
type jellyfinLibrariesResponse struct {
	Started bool `json:"started"`
}

// handleJellyfinLibraries 处理 POST /api/jellyfin/libraries：派一轮媒体库同步。
//
// 202 已派出 / 409 上一轮还没结束 / 400 三项配置没配齐（body 指出缺哪个键）。
// 没有请求体参数：删不删库由分类开关决定（MKACG-668），不再有 prune —— 开关就是
// 删除开关，这一轮读到哪个分类关着，就删它对应的库。
//
// 结果不在响应里：跑完看 GET /api/jellyfin/libraries（页面轮询的就是它，MKACG-664），
// /api/status 的 jellyfin 块是同一份视图。
func (s *Server) handleJellyfinLibraries(w http.ResponseWriter, r *http.Request) {
	if err := s.jellyfin.Start(r.Context()); err != nil {
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

// jellyfinCategoryView 是分类开关列表的一行。
type jellyfinCategoryView struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// jellyfinCategoriesResponse 是 GET /api/jellyfin/categories 的响应体。
type jellyfinCategoriesResponse struct {
	Categories []jellyfinCategoryView `json:"categories"`
}

// jellyfinCategoryRequest 是 PATCH /api/jellyfin/categories 的请求体。
//
// enabled 用指针：省略它等于没说改成什么，必须 400，而不是把分类默默关掉。
type jellyfinCategoryRequest struct {
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled"`
}

// handleJellyfinCategories 处理 GET /api/jellyfin/categories：全部分类 + 开关状态。
//
// 分类口径与建库同源（vfs 根目录），开关状态来自分类开关表 —— 两者都现读，所以
// 页面打开时的开关与实际下一轮同步会执行的判定一致。未配置 Jellyfin 时照样可读
// （先拨开关、后配地址是合理顺序），只是不会真的同步。
func (s *Server) handleJellyfinCategories(w http.ResponseWriter, r *http.Request) {
	categories, err := s.jellyfin.Categories(r.Context())
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	out := jellyfinCategoriesResponse{Categories: make([]jellyfinCategoryView, 0, len(categories))}
	for _, category := range categories {
		out.Categories = append(out.Categories, jellyfinCategoryView{Name: category.Name, Enabled: category.Enabled})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleJellyfinCategoryToggle 处理 PATCH /api/jellyfin/categories：开关一个分类。
//
// 只写库，不碰 Jellyfin：拨开关的瞬间什么都不发生，删除落在下一轮周期同步里；
// 「立即同步」本来就是马上跑一轮，所以那时当场删。这样开关动作永远不是破坏性的
// 即时操作，也永远不需要在请求里等 Jellyfin。
func (s *Server) handleJellyfinCategoryToggle(w http.ResponseWriter, r *http.Request) {
	var req jellyfinCategoryRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, err)
		return
	}
	if req.Name == "" || req.Enabled == nil {
		httpx.WriteError(w, fmt.Errorf("%w: 需要 name 与 enabled", httpx.ErrInvalid))
		return
	}
	if err := s.st.SetJellyfinCategoryEnabled(r.Context(), req.Name, *req.Enabled); err != nil {
		if errors.Is(err, store.ErrInvalidSetting) {
			httpx.WriteError(w, fmt.Errorf("%w: %v", httpx.ErrInvalid, err))
			return
		}
		httpx.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jellyfinCategoryView{Name: req.Name, Enabled: *req.Enabled})
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

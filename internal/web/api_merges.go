package web

import (
	"context"
	"net/http"

	"github.com/justforlxz/drama-hub/internal/httpx"
)

// mergeMemberView 是合并组里的一个成员：ord 是时间轴顺序（从 1 起）。
type mergeMemberView struct {
	EpisodeID int64 `json:"episode_id"`
	Ord       int   `json:"ord"`
	EpNo      int   `json:"ep_no"`
}

// mergeGroupView 是合并编排页要的一个合并组：标签 + 按时间轴顺序的成员。
type mergeGroupView struct {
	ID      int64             `json:"id"`
	Label   string            `json:"label"`
	Members []mergeMemberView `json:"members"`
}

// mergeGroupList 是某剧合并组的响应体；GET 与 POST 返回同一形态。
type mergeGroupList struct {
	Groups []mergeGroupView `json:"groups"`
}

// mergeViews 拼装某剧的合并组列表。合并组数量很小（一部剧个位数），每组一次
// 成员查询，不额外优化。
func (s *Server) mergeViews(ctx context.Context, dramaID int64) ([]mergeGroupView, error) {
	groups, err := s.st.ListMergeGroups(ctx, dramaID)
	if err != nil {
		return nil, err
	}
	out := make([]mergeGroupView, 0, len(groups))
	for _, g := range groups {
		members, err := s.st.ListMergeMembers(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		views := make([]mergeMemberView, 0, len(members))
		for _, m := range members {
			views = append(views, mergeMemberView{EpisodeID: m.EpisodeID, Ord: m.Ord, EpNo: m.EpNo})
		}
		out = append(out, mergeGroupView{ID: g.ID, Label: g.Label, Members: views})
	}
	return out, nil
}

// handleListMergeGroups 处理 GET /api/dramas/{id}/merge-groups。
func (s *Server) handleListMergeGroups(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if _, err := s.st.DramaByID(r.Context(), id); err != nil {
		httpx.WriteError(w, mapStoreErr(err))
		return
	}
	views, err := s.mergeViews(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mergeGroupList{Groups: views})
}

// handleCreateMergeGroup 处理 POST /api/dramas/{id}/merge-groups。
//
// 成员顺序 = episode_ids 的顺序（数组下标 + 1 即 ord）。校验全部在 store 里
// （至少 2 集、同剧、不重复、不与已有组合重叠），这里只做 JSON 形态检查：
// store.ErrInvalid → 400，且不落库、不重建派生表。
// 成功返回 201 + 整份合并组列表，页面直接套用。
func (s *Server) handleCreateMergeGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	var in struct {
		Label      string  `json:"label"`
		EpisodeIDs []int64 `json:"episode_ids"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		httpx.WriteError(w, err)
		return
	}
	if _, err := s.st.CreateMergeGroup(r.Context(), id, in.Label, in.EpisodeIDs); err != nil {
		httpx.WriteError(w, mapStoreErr(err))
		return
	}
	views, err := s.mergeViews(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, mergeGroupList{Groups: views})
}

// handleDeleteMergeGroup 处理 DELETE /api/merge-groups/{id}：删组连带重建该剧
// 派生表（store 在同一事务里做）。不存在返回 404。
func (s *Server) handleDeleteMergeGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if err := s.st.DeleteMergeGroup(r.Context(), id); err != nil {
		httpx.WriteError(w, mapStoreErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

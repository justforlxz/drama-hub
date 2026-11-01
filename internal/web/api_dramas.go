package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
)

// dramaView 是一部剧在管理面上的视图：drama 行 + 派生计数。
//
// episode_total 是上游声明的集数，episodes 是库里真实落库的集数（两者不等说明
// 上一次同步不完整，列表页直接并排显示）。
type dramaView struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	Year         int    `json:"year"`
	Provider     string `json:"provider"`
	ProviderID   string `json:"provider_id"`
	EpisodeTotal int    `json:"episode_total"`
	Episodes     int    `json:"episodes"`
	EmbyItems    int    `json:"emby_items"`
	MergeGroups  int    `json:"merge_groups"`
	SyncAt       string `json:"sync_at"`
}

// dramaList 是剧目列表的响应体；GET 与 POST 返回同一形态，页面直接套用。
type dramaList struct {
	Dramas []dramaView `json:"dramas"`
}

// episodeView 是一集的视图。只剩库里的字段：直链不落库（0004 已删 play_url），
// 「能不能播」由 /video 请求时向插件解析，管理面不冒充知道。
type episodeView struct {
	ID          int64  `json:"id"`
	EpNo        int    `json:"ep_no"`
	Title       string `json:"title"`
	DurationSec int    `json:"duration_sec"`
	Vid         string `json:"vid"`
}

// episodeList 是某剧集列表的响应体。
type episodeList struct {
	Episodes []episodeView `json:"episodes"`
}

// dramaViews 拼装剧目列表：一次 ListDramas + 一次 ListDramaStats，不做 N+1。
func (s *Server) dramaViews(ctx context.Context) ([]dramaView, error) {
	dramas, err := s.st.ListDramas(ctx)
	if err != nil {
		return nil, err
	}
	stats, err := s.st.ListDramaStats(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]store.DramaStats, len(stats))
	for _, st := range stats {
		byID[st.DramaID] = st
	}

	out := make([]dramaView, 0, len(dramas))
	for _, d := range dramas {
		st := byID[d.ID]
		out = append(out, dramaView{
			ID:           d.ID,
			Title:        d.Title,
			Year:         d.Year,
			Provider:     d.Provider,
			ProviderID:   d.ProviderID,
			EpisodeTotal: d.EpisodeTotal,
			Episodes:     st.Episodes,
			EmbyItems:    st.EmbyItems,
			MergeGroups:  st.MergeGroups,
			SyncAt:       d.SyncAt,
		})
	}
	return out, nil
}

// handleListDramas 处理 GET /api/dramas。
func (s *Server) handleListDramas(w http.ResponseWriter, r *http.Request) {
	views, err := s.dramaViews(r.Context())
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dramaList{Dramas: views})
}

// handleCreateDrama 处理 POST /api/dramas：按上游剧 id 同步入库。
//
// 「增剧目」的唯一入口是数据源同步 —— 剧目与集的真值都在上游，手工插空剧只会
// 产生一条永远没有集的记录。同步失败统一归 502：数据源在远端，失败几乎总是上游
// 或页面结构问题，失败原因原样进响应体（插件的错误文本不含直链与签名）。
//
// 状态码按库里「有没有新行」分：新建 201、已存在（这次只更新）200（MKACG-623）。
// 重复 POST 同一 series_id 时 store 只更新那一行、不新增，回 201 Created 等于对
// 调用方说了假话 —— 所以先查一次是否存在，再决定同步后回哪个码。判断留在本包，
// 不下沉到 store（那层随后要整体重写）。
//
// 成功返回整份剧目列表，页面直接套用。
func (s *Server) handleCreateDrama(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SeriesID string `json:"series_id"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		httpx.WriteError(w, err)
		return
	}
	sid, ok := normalizeSeriesID(in.SeriesID)
	if !ok {
		httpx.WriteError(w, fmt.Errorf("%w: series_id 必须是上游剧 id（纯数字）或 /player/<id> 链接", httpx.ErrInvalid))
		return
	}
	_, foundErr := s.st.DramaByProviderID(r.Context(), s.syncer.Name(), sid)
	created := errors.Is(foundErr, store.ErrNotFound)
	if foundErr != nil && !created {
		httpx.WriteError(w, foundErr)
		return
	}
	if err := s.syncer.SyncSeries(r.Context(), s.st, sid); err != nil {
		s.logger.WarnContext(r.Context(), "同步剧目失败", "sid", sid, "err", err)
		httpx.WriteError(w, fmt.Errorf("%w: 同步剧 %s 失败: %v", httpx.ErrUpstream, sid, err))
		return
	}
	views, err := s.dramaViews(r.Context())
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, dramaList{Dramas: views})
}

// handleDeleteDrama 处理 DELETE /api/dramas/{id}：删剧连带删集、合并组与派生表
// （schema 的 ON DELETE CASCADE）。不存在返回 404。
func (s *Server) handleDeleteDrama(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if err := s.st.DeleteDrama(r.Context(), id); err != nil {
		httpx.WriteError(w, mapStoreErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListEpisodes 处理 GET /api/dramas/{id}/episodes：合并编排页要按源集挑选
// 成员，因此返回该剧的全部集（按 ep_no 升序）。剧不存在返回 404。
func (s *Server) handleListEpisodes(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if _, err := s.st.DramaByID(r.Context(), id); err != nil {
		httpx.WriteError(w, mapStoreErr(err))
		return
	}
	eps, err := s.st.ListEpisodes(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	out := make([]episodeView, 0, len(eps))
	for _, ep := range eps {
		out = append(out, episodeView{
			ID:          ep.ID,
			EpNo:        ep.EpNo,
			Title:       ep.Title,
			DurationSec: ep.DurationSec,
			Vid:         ep.Vid,
		})
	}
	writeJSON(w, http.StatusOK, episodeList{Episodes: out})
}

// normalizeSeriesID 把管理面的输入归一为上游剧 id：裸 id（纯数字）或
// `https://<站点>/player/<id>` 这类链接都接受，其余一律拒绝。
//
// 只认纯数字是刻意的：sid 会拼进上游路径，放宽字符集等于把路径注入带进来。
// 纯函数，单测直接覆盖。
func normalizeSeriesID(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if i := strings.Index(s, "/player/"); i >= 0 {
		s = s[i+len("/player/"):]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if s == "" || len(s) > 20 {
		return "", false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return s, true
}

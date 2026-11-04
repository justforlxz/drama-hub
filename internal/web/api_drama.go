package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
)

// chunkEpisode 是一集在上游库里的元信息（对外的集号与播放地址在 chunk 上）。
type chunkEpisode struct {
	EpNo        int    `json:"ep_no"`
	Title       string `json:"title"`
	DurationSec int    `json:"duration_sec"`
}

// episodeChunk 是一个对外剧集（合并集或单集）：块内成员 + 签名后的播放地址。
//
// from / to 是库内 ep_no 区间，count 是块内真实集数（区间可能不连续，两回事）。
// play_id 只由后端签名（IDFunc，secret 取 settings 的 stream_secret），页面拿不到
// 签名算法，也就不可能自己拼出可播的 id。
type episodeChunk struct {
	EpNo     int            `json:"ep_no"`
	From     int            `json:"from"`
	To       int            `json:"to"`
	Count    int            `json:"count"`
	PlayID   string         `json:"play_id"`
	PlayURL  string         `json:"play_url"`
	Episodes []chunkEpisode `json:"episodes"`
}

// dramaEpisodesView 是剧详情页的数据面：分块（块边界与对外集号由
// store.ListEmbyItems → deriveEmbyItems 派生，本包不复制那套算法）、当前合并粒度，
// 以及这部剧在虚拟树里的目录路径（拼 WebDAV 串用）。
type dramaEpisodesView struct {
	ID             int64          `json:"id"`
	MergeChunkSize int            `json:"merge_chunk_size"`
	WebDAVPath     string         `json:"webdav_path"`
	Chunks         []episodeChunk `json:"chunks"`
}

// handleDramaEpisodes 处理 GET /api/dramas/{id}/episodes。
//
// 分块只有一条路径：store.ListEmbyItems（内部 deriveEmbyItems），本包只负责把块与
// 库内集对上（块是排序集列表的连续切片，用 ep_no 区间筛 ListEpisodes 即精确成员）、
// 并把每块的对外播放 id 过一遍签名。改 merge_chunk_size 后重新取，块随之变化。
func (s *Server) handleDramaEpisodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := parseDramaID(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	d, err := s.st.DramaByID(ctx, id)
	if err != nil {
		// store 的哨兵不 import httpx（分层），在边界翻成 404。
		if errors.Is(err, store.ErrNotFound) {
			httpx.WriteError(w, fmt.Errorf("%w: 剧 %d 不存在", httpx.ErrNotFound, id))
			return
		}
		httpx.WriteError(w, fmt.Errorf("读剧 %d: %w", id, err))
		return
	}
	items, err := s.st.ListEmbyItems(ctx, id)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("列对外剧集 drama_id=%d: %w", id, err))
		return
	}
	eps, err := s.st.ListEpisodes(ctx, id)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("列剧集 drama_id=%d: %w", id, err))
		return
	}
	size, err := s.settings.Int(ctx, store.KeyMergeChunkSize)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("读 %s: %w", store.KeyMergeChunkSize, err))
		return
	}
	secret, err := s.settings.String(ctx, store.KeyStreamSecret)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("读 %s: %w", store.KeyStreamSecret, err))
		return
	}
	prefix, err := s.settings.String(ctx, store.KeyStrmPrefix)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("读 %s: %w", store.KeyStrmPrefix, err))
		return
	}
	dir, err := s.dramaPath(ctx, d)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("计算剧目录 drama_id=%d: %w", id, err))
		return
	}

	chunks := make([]episodeChunk, 0, len(items))
	for _, item := range items {
		members := chunkEpisodesIn(eps, item.EpFrom, item.EpTo)
		playID := s.signID(secret, item.UID)
		chunks = append(chunks, episodeChunk{
			EpNo:     item.EpNo,
			From:     item.EpFrom,
			To:       item.EpTo,
			Count:    len(members),
			PlayID:   playID,
			PlayURL:  playURL(prefix, playID),
			Episodes: members,
		})
	}
	writeJSON(w, http.StatusOK, dramaEpisodesView{
		ID:             id,
		MergeChunkSize: size,
		WebDAVPath:     dir,
		Chunks:         chunks,
	})
}

// handleDramaCover 处理 GET /api/dramas/{id}/cover：本地已有海报就直接给字节，
// 没有就按需回源一次（与 WebDAV 的 poster.jpg 共用同一个 CoverCache 实例）。
//
// 没有封面、回源失败、剧不存在、id 非法一律 404 —— 封面是可选资产，缺席不是错误。
// 成功时 Content-Type 由字节嗅探（不信任上游声明的类型），并给出精确 Content-Length。
func (s *Server) handleDramaCover(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := parseDramaID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := s.st.DramaByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		httpx.WriteError(w, fmt.Errorf("读剧 %d: %w", id, err))
		return
	}
	body, available, err := s.covers.Poster(ctx, d.Provider, d.ProviderID, d.CoverURL)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("取封面 drama_id=%d: %w", id, err))
		return
	}
	if !available || len(body) == 0 {
		http.NotFound(w, r)
		return
	}
	contentType := http.DetectContentType(body)
	if !strings.HasPrefix(contentType, "image/") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeBody(w, http.StatusOK, contentType, body)
}

// parseDramaID 解析路径里的剧 id：只接受正整数，其余归为 400（httpx.ErrInvalid）。
func parseDramaID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: 剧 id 必须是正整数（收到 %q）", httpx.ErrInvalid, raw)
	}
	return id, nil
}

// chunkEpisodesIn 返回 ep_no 落在 [from, to] 的集，按 ListEpisodes 的顺序（ep_no 升序）。
func chunkEpisodesIn(eps []store.Episode, from, to int) []chunkEpisode {
	out := make([]chunkEpisode, 0, to-from+1)
	for _, ep := range eps {
		if ep.EpNo < from || ep.EpNo > to {
			continue
		}
		out = append(out, chunkEpisode{EpNo: ep.EpNo, Title: ep.Title, DurationSec: ep.DurationSec})
	}
	return out
}

// playURL 拼出块的可复制播放地址：`<strm_prefix>/video/<play_id>.m3u8`。
//
// strm_prefix 为空（相对部署）时退回同源相对路径 —— 复制到别处不可用，但页面上
// 看到的就是这台服务上的真实地址。
func playURL(prefix, playID string) string {
	if prefix == "" {
		return "/video/" + playID + ".m3u8"
	}
	return strings.TrimSuffix(prefix, "/") + "/video/" + playID + ".m3u8"
}

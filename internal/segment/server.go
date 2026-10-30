package segment

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
)

// ServeHTTP 实现分片端点。装配根（cmd）在顶层 mux 上注册 Route（比 `/s/`
// 更具体），命中 `GET /s/{token}/seg/{uid}/{seq}.ts`。uid 分两个空间：单集
// `e<episode.id>`（本集片内序）与合并集 `m<merge_group.id>`（跨集连续 seq 空间，
// 由 NewTimeline 回答 seq → (集号, 集内秒)）。
func (c *Cutter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	uid := r.PathValue("uid")
	seq, ok := parseSeqFile(r.PathValue("file"))
	if !ok {
		httpx.WriteError(w, fmt.Errorf("分片文件名 %q: %w", r.PathValue("file"), httpx.ErrNotFound))
		return
	}

	// 每请求读 secret，不缓存（硬约束 5）：换 secret 后旧 URL 立即失效，
	// 与 stream 的周期读取一致。
	secret, err := c.settings.String(r.Context(), store.KeyStreamSecret)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("读取出流密钥: %w", err))
		return
	}
	if !c.verify([]byte(secret), uid, token) {
		httpx.WriteError(w, httpx.ErrUnauthorized)
		return
	}

	if IsMergedUID(uid) {
		c.serveMergedSlice(w, r, uid, seq)
		return
	}

	episodeID, ok := parseEpisodeUID(uid)
	if !ok {
		httpx.WriteError(w, fmt.Errorf("uid %q 不是单集句柄: %w", uid, httpx.ErrNotFound))
		return
	}

	ep, err := c.st.EpisodeByID(r.Context(), episodeID)
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

	segs, err := c.st.ListSegments(r.Context(), episodeID)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("读分片索引 %s: %w", uid, err))
		return
	}
	if len(segs) == 0 {
		// 索引未完成（ADR-0001）：503 + Retry-After，不出半截流。
		writeSliceError(w, fmt.Errorf("剧集 %s 的分片索引未就绪: %w", uid, httpx.ErrUnavailable))
		return
	}

	// 单集的 seq 空间即本集片内序。
	if seq >= len(segs) || seq < 0 {
		httpx.WriteError(w, fmt.Errorf("剧集 %s 无分片 %d: %w", uid, seq, httpx.ErrNotFound))
		return
	}
	seg := segs[seq]
	c.serveCut(w, r, cutRequest{uid: uid, seq: seq, playURL: ep.PlayURL, start: seg.StartSec, dur: seg.Dur})
}

// serveMergedSlice 服务合并集分片：合并组 → 跨集时间轴 → seq 命中的 (集, 集内秒)。
func (c *Cutter) serveMergedSlice(w http.ResponseWriter, r *http.Request, uid string, seq int) {
	groupID, ok := parseMergedUID(uid)
	if !ok {
		httpx.WriteError(w, fmt.Errorf("uid %q 不是合并集句柄: %w", uid, httpx.ErrNotFound))
		return
	}
	ep, start, dur, offset, ok, err := c.mergedCut(r.Context(), groupID, seq)
	if err != nil {
		writeSliceError(w, err)
		return
	}
	if !ok {
		httpx.WriteError(w, fmt.Errorf("合并集 %s 无分片 %d: %w", uid, seq, httpx.ErrNotFound))
		return
	}
	if ep.PlayURL == "" {
		httpx.WriteError(w, fmt.Errorf("剧集 %d 没有播放地址: %w", ep.ID, httpx.ErrUpstream))
		return
	}
	c.serveCut(w, r, cutRequest{uid: uid, seq: seq, playURL: ep.PlayURL, start: start, dur: dur, offset: offset})
}

// cutRequest 是一次切片请求的全部参数（单集与合并集共用）。
type cutRequest struct {
	uid     string
	seq     int
	playURL string
	start   float64 // 集内起点（秒）
	dur     float64 // 片长（秒）
	offset  float64 // 合并时间轴上的绝对起点（秒）；单集为 0
}

// serveCut 是单集与合并集共用的切片写出路径：过并发闸 → ffmpeg 现切 → 200 MPEG-TS。
//
// ffmpeg 失败 → 502，失败结果不落库、不缓存（本服务不落盘任何切片）。
func (c *Cutter) serveCut(w http.ResponseWriter, r *http.Request, req cutRequest) {
	if err := c.acquire(r.Context()); err != nil {
		return // 客户端已断开：不写响应，只让请求结束
	}
	body, cutErr := cut(r.Context(), req.playURL, req.start, req.dur, req.offset)
	c.release()
	if cutErr != nil {
		c.logger.WarnContext(r.Context(), "segment slice failed",
			"uid", req.uid, "seq", req.seq, "start_sec", req.start, "err", cutErr)
		httpx.WriteError(w, fmt.Errorf("切片 %s 分片 %d: %w", req.uid, req.seq, httpx.ErrUpstream))
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// parseSeqFile 解析 `<seq>.ts` 文件名：必须以 `.ts` 结尾，seq 是**纯十进制数字**
// （允许前导零：缓存 key 是整串 URL，本包不改写客户端 URL）。显式拒绝符号与非
// 数字字符——strconv.Atoi 会接受 `+1`/`-1`，那不是本包产出的 URL 形态。
func parseSeqFile(name string) (int, bool) {
	seqStr, ok := strings.CutSuffix(name, ".ts")
	if !ok || seqStr == "" {
		return 0, false
	}
	for _, r := range seqStr {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	seq, err := strconv.Atoi(seqStr)
	if err != nil {
		return 0, false
	}
	return seq, true
}

// parseEpisodeUID 解析单集 uid `e<episode.id>`。不匹配或超出 int64 返回 false，
// 调用方映射 404。合并集 uid 由 M7 定义，不得匹配本模式（DESIGN D3）。
func parseEpisodeUID(uid string) (int64, bool) {
	if !strings.HasPrefix(uid, "e") {
		return 0, false
	}
	id, err := strconv.ParseInt(uid[1:], 10, 64)
	if err != nil || id < 0 {
		return 0, false
	}
	return id, true
}

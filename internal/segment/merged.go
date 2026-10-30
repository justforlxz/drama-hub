package segment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
)

// playVersion 是自造 playlist 的 HLS 版本（DESIGN §2 示例同款）。
const playVersion = 4

// PlaylistRoute 是合并集播放列表端点的装配 pattern。它与 `GET /s/{token}/e/{uid}`
// （单集出流）互不覆盖：本 pattern 多一段 `/master.m3u8`，因此 cmd 把两个 handler
// 挂在同一 `/s/` 子树下时由 ServeMux 按最具体者选择（与分片端点的做法一致）。
const PlaylistRoute = "GET /s/{token}/e/{uid}/master.m3u8"

// mergedUIDPrefix 是合并集句柄前缀：`m` + merge_group 主键（DESIGN D3）。
// 与单集 `e` 前缀互斥，因此合并集 uid 永不匹配 `^e[0-9]+$`。
const mergedUIDPrefix = "m"

// IsMergedUID 判断 uid 是否是合并集句柄（`m` + merge_group 主键十进制）。
//
// 装配根用它把 `/s/{token}/e/{uid}` 的合并集请求路由到本包的播放列表 handler：
// 单集走 stream，合并集走 Cutter（Go 的路由 pattern 不能在路径段内按前缀分派）。
func IsMergedUID(uid string) bool {
	_, ok := parseMergedUID(uid)
	return ok
}

// ServePlaylist 实现合并集播放列表端点：鉴权 → 展开合并时间轴 → 渲染 VOD playlist。
//
// 分片索引未就绪（组内任一集没有 segment 行）返回 503 + Retry-After，与单集分片
// 一致：不产出半截时间轴。
func (c *Cutter) ServePlaylist(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	secret, err := c.settings.String(r.Context(), store.KeyStreamSecret)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("读取出流密钥: %w", err))
		return
	}
	if !c.verify([]byte(secret), uid, r.PathValue("token")) {
		httpx.WriteError(w, httpx.ErrUnauthorized)
		return
	}
	groupID, ok := parseMergedUID(uid)
	if !ok {
		httpx.WriteError(w, fmt.Errorf("uid %q 不是合并集句柄: %w", uid, httpx.ErrNotFound))
		return
	}
	slices, err := c.mergedSlices(r.Context(), groupID)
	if err != nil {
		writeSliceError(w, err)
		return
	}
	// 分片 URI 复用请求里的 token：同一合并集 uid 签出的 token 对 playlist 与分片
	// 都有效（ADR-0003 按 uid 签名），客户端不需要第二张 URL。
	body := BuildPlaylist(uid, r.PathValue("token"), slices)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// mergedSlices 把合并组展开成合并时间轴（seq 从 0 起连续，跨集不断号）。
//
// 组不存在、组内没有成员、成员集不存在 → 404 类别；成员的分片索引未就绪 →
// ErrUnavailable（503 + Retry-After，ADR-0001：不出半截流）。
func (c *Cutter) mergedSlices(ctx context.Context, groupID int64) ([]Slice, error) {
	members, err := c.st.ListMergeMembers(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("读合并组成员 %d: %w", groupID, err)
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("合并组 %d 没有成员: %w", groupID, httpx.ErrNotFound)
	}

	eps := make([]EpisodeSegments, 0, len(members))
	for _, member := range members {
		ep, err := c.st.EpisodeByID(ctx, member.EpisodeID)
		if err != nil {
			return nil, fmt.Errorf("查合并组成员剧集 %d: %w", member.EpisodeID, err)
		}
		segs, err := c.st.ListSegments(ctx, member.EpisodeID)
		if err != nil {
			return nil, fmt.Errorf("读成员 %d 分片索引: %w", member.EpisodeID, err)
		}
		if len(segs) == 0 {
			return nil, fmt.Errorf("合并组成员剧集 %d 的分片索引未就绪: %w", member.EpisodeID, httpx.ErrUnavailable)
		}
		eps = append(eps, EpisodeSegments{EpisodeID: ep.ID, EpNo: ep.EpNo, Segments: segs})
	}
	return NewTimeline(eps).slices, nil
}

// BuildPlaylist 把合并时间轴渲染成 VOD playlist 字节（纯函数）。
//
// 每片一行 `#EXTINF:<真实时长>,` + 分片 URI；集边界（EpisodeID 变化）插
// `#EXT-X-DISCONTINUITY`（DESIGN §2）。TARGETDURATION 取最大片长向上取整（HLS
// 要求它 ≥ 每个 EXTINF）。分片 URI 指回本服务的 `/s/{token}/seg/{uid}/{seq}.ts`，
// uid 是合并集句柄、token 是请求里的签名。
//
// 字节长度 = len(返回值)，调用方用它做 Content-Length（硬约束 3）。
func BuildPlaylist(uid, token string, slices []Slice) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:" + strconv.Itoa(playVersion) + "\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	maxDur := 0.0
	for _, s := range slices {
		if s.Dur > maxDur {
			maxDur = s.Dur
		}
	}
	b.WriteString("#EXT-X-TARGETDURATION:" + strconv.Itoa(int(math.Ceil(maxDur))) + "\n")

	prevEpisode := int64(-1)
	for _, s := range slices {
		if prevEpisode != -1 && s.EpisodeID != prevEpisode {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		b.WriteString("#EXTINF:" + formatSeconds(s.Dur) + ",\n")
		b.WriteString("/s/" + token + "/seg/" + uid + "/" + strconv.Itoa(s.Seq) + ".ts\n")
		prevEpisode = s.EpisodeID
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String())
}

// parseMergedUID 解析合并集句柄 `m<group_id>`；不匹配或超出 int64 返回 false。
func parseMergedUID(uid string) (int64, bool) {
	if !strings.HasPrefix(uid, mergedUIDPrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(uid[len(mergedUIDPrefix):], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// mergedCut 解析合并时间轴上的一个 seq，返回 (命中的剧集, 集内起点, 片长,
// 合并时间轴绝对起点)。
//
// ok=false 表示 seq 越界（调用方映射 404）；其它错误按 ErrUnavailable / 404 类别
// 由调用方 writeSliceError 统一写出。
func (c *Cutter) mergedCut(ctx context.Context, groupID int64, seq int) (store.Episode, float64, float64, float64, bool, error) {
	slices, err := c.mergedSlices(ctx, groupID)
	if err != nil {
		return store.Episode{}, 0, 0, 0, false, err
	}
	if seq < 0 || seq >= len(slices) {
		return store.Episode{}, 0, 0, 0, false, nil
	}
	sl := slices[seq]
	ep, err := c.st.EpisodeByID(ctx, sl.EpisodeID)
	if err != nil {
		return store.Episode{}, 0, 0, 0, false, fmt.Errorf("查分片所属剧集 %d: %w", sl.EpisodeID, err)
	}
	return ep, sl.StartSec, sl.Dur, sl.OffsetSec, true, nil
}

// writeSliceError 按分片端点的错误矩阵写出响应：索引未就绪 → 503 + Retry-After
// （ADR-0001：不出半截流），其余走 httpx 统一映射。
func writeSliceError(w http.ResponseWriter, err error) {
	if errors.Is(err, httpx.ErrUnavailable) {
		w.Header().Set("Retry-After", "5")
	}
	httpx.WriteError(w, err)
}

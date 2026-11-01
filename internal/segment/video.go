package segment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
)

// 播放面路由（docs/PLAYBACK-CONTRACT.md「两个口子」）。装配根把两个 pattern 挂在
// 顶层 mux 上；`{id}` 承载整段 `<uid>-<签名>`，`{file}` 承载 `<seq>.ts`（Go 的路由
// pattern 不支持段内通配 `{seq}.ts`，因此用整段承载）。
const (
	// PlaylistRoute 是播放列表端点：`GET|HEAD /video/{id}.m3u8`。
	PlaylistRoute = "GET /video/{id}"
	// SegmentRoute 是分片端点：`GET|HEAD /video/{id}/{seq}.ts`。
	SegmentRoute = "GET /video/{id}/{file}"
)

// m3u8Suffix 是播放列表的扩展名（契约：单集与合并集同一路径形态）。
const m3u8Suffix = ".m3u8"

// ServePlaylist 实现 `GET /video/{id}.m3u8`：鉴权 → 展开播放单元 → 按需建索引 →
// 渲染 VOD playlist。
//
// 状态码（docs/PLAYBACK-CONTRACT.md「GET /video/{id}.m3u8」）：secret 读失败 500、
// 签名不对/缺 `-` 401、uid 非法或库里没有 404、解析不到直链 502、索引建立超时
// 503 + Retry-After。
func (c *Cutter) ServePlaylist(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	raw := r.PathValue("id")
	id, ok := strings.CutSuffix(raw, m3u8Suffix)
	if !ok || id == "" {
		httpx.WriteError(w, fmt.Errorf("%w: 播放列表路径 %q 必须以 %s 结尾", httpx.ErrNotFound, raw, m3u8Suffix))
		return
	}
	secret, err := c.settings.String(ctx, store.KeyStreamSecret)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("读取出流密钥: %w", err))
		return
	}
	uid, ok := stream.ParseID(secret, id)
	if !ok {
		httpx.WriteError(w, httpx.ErrUnauthorized)
		return
	}

	u, err := c.unit(ctx, uid)
	if err != nil {
		writeVideoError(w, err)
		return
	}
	// 播放列表也现解析直链：某个成员拿不到直链时按契约回 502，而不是放出
	// 一份「能读、一播就失败」的时间轴。
	tl, err := c.timelineFor(ctx, u, true)
	if err != nil {
		writeVideoError(w, err)
		return
	}

	body := BuildPlaylist(id, tl.slices)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// ServeSegment 实现 `GET /video/{id}/{seq}.ts`：鉴权 → 时间轴定位 → 缓存命中即回 /
// 未命中切片落缓存再回。
//
// 缓存返回的 []byte 在 single-flight 下是多个等待者共享的只读字节，这里只读不写
// （ServeContent 只读）。Range 与 HEAD 都由 http.ServeContent 处理：分片是完整
// 不可变对象，不需要分片生成。
func (c *Cutter) ServeSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	seq, ok := parseSeqFile(r.PathValue("file"))
	if !ok {
		httpx.WriteError(w, fmt.Errorf("%w: 分片文件名 %q", httpx.ErrNotFound, r.PathValue("file")))
		return
	}
	secret, err := c.settings.String(ctx, store.KeyStreamSecret)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("读取出流密钥: %w", err))
		return
	}
	id := r.PathValue("id")
	uid, ok := stream.ParseID(secret, id)
	if !ok {
		httpx.WriteError(w, httpx.ErrUnauthorized)
		return
	}

	u, err := c.unit(ctx, uid)
	if err != nil {
		writeVideoError(w, err)
		return
	}
	// 分片端点只解析它真正要切的那一集（合并集的不同 seq 属于不同源集）。
	tl, err := c.timelineFor(ctx, u, false)
	if err != nil {
		writeVideoError(w, err)
		return
	}
	if seq < 0 || seq >= len(tl.slices) {
		httpx.WriteError(w, fmt.Errorf("%w: 播放单元 %s 没有分片 %d", httpx.ErrNotFound, uid, seq))
		return
	}
	sl := tl.slices[seq]
	key := cache.SegmentKey(uid, seq, sl.CodecSig)
	body, err := c.caches.GetOrGenerate(ctx,
		cache.Spec{Kind: store.ArtifactKindSegment, Key: key, Ext: ".ts"},
		func(ctx context.Context) ([]byte, error) {
			// 直链只在真要切片时才解析：命中缓存的分片请求零外呼
			// （契约「命中即回文件」，也是缓存存在的意义）。
			d, err := c.direct(ctx, tl, sl.EpisodeID)
			if err != nil {
				return nil, err
			}
			return cut(ctx, d, sl.StartSec, sl.Dur, sl.OffsetSec, c.headerBlock)
		})
	if err != nil {
		c.logger.WarnContext(ctx, "segment slice failed",
			"uid", uid, "seq", seq, "start_sec", sl.StartSec, "err", err)
		// 生成失败（ffmpeg）是 502 且不写缓存；缓存自身故障按 500 报，不冒充上游问题。
		if errors.Is(err, httpx.ErrUpstream) {
			writeVideoError(w, fmt.Errorf("切片 %s 分片 %d（%v）: %w", uid, seq, err, httpx.ErrUpstream))
			return
		}
		httpx.WriteError(w, fmt.Errorf("写分片 %s %d: %w", uid, seq, err))
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

// playbackUnit 是一个播放单元：单集或合并组，eps 按播放顺序排列。
type playbackUnit struct {
	eps []store.Episode
}

// unit 把 uid 展开成播放单元。
//
// 「合法 uid」由 emby_item 派生表决定（硬约束 6 + 契约「不飘移」第 3 条）：某集被并进
// 合并组后它的 `e<id>` 立刻不是播放单元，旧 URL 必须 404 —— 所以先查派生表再取集。
// 单集一项；合并集按 merge_item.ord 顺序取成员剧集。
// uid 语法不认识、派生表没有、组合没有成员 → httpx.ErrNotFound（404）。
func (c *Cutter) unit(ctx context.Context, uid string) (playbackUnit, error) {
	if _, err := c.st.EmbyItemByUID(ctx, uid); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return playbackUnit{}, fmt.Errorf("%w: 播放单元 %s", httpx.ErrNotFound, uid)
		}
		return playbackUnit{}, fmt.Errorf("查播放单元 %s: %w", uid, err)
	}
	if groupID, ok := parseMergedUID(uid); ok {
		members, err := c.st.ListMergeMembers(ctx, groupID)
		if err != nil {
			return playbackUnit{}, fmt.Errorf("读合并组成员 %d: %w", groupID, err)
		}
		if len(members) == 0 {
			return playbackUnit{}, fmt.Errorf("%w: 合并组 %s 不存在或没有成员", httpx.ErrNotFound, uid)
		}
		eps := make([]store.Episode, 0, len(members))
		for _, m := range members {
			ep, err := c.st.EpisodeByID(ctx, m.EpisodeID)
			if errors.Is(err, store.ErrNotFound) {
				return playbackUnit{}, fmt.Errorf("%w: 合并组成员剧集 %d", httpx.ErrNotFound, m.EpisodeID)
			}
			if err != nil {
				return playbackUnit{}, fmt.Errorf("查合并组成员剧集 %d: %w", m.EpisodeID, err)
			}
			eps = append(eps, ep)
		}
		return playbackUnit{eps: eps}, nil
	}

	episodeID, ok := parseEpisodeUID(uid)
	if !ok {
		return playbackUnit{}, fmt.Errorf("%w: uid %q 不是播放单元句柄", httpx.ErrNotFound, uid)
	}
	ep, err := c.st.EpisodeByID(ctx, episodeID)
	if errors.Is(err, store.ErrNotFound) {
		return playbackUnit{}, fmt.Errorf("%w: 剧集 %s", httpx.ErrNotFound, uid)
	}
	if err != nil {
		return playbackUnit{}, fmt.Errorf("查剧集 %s: %w", uid, err)
	}
	return playbackUnit{eps: []store.Episode{ep}}, nil
}

// resolvedTimeline 是播放单元的时间轴加上本次已解析的直链与 key（集 id → 直链）。
//
// 直链只活在这次请求里：不落库、不缓存、不进 Timeline（时间轴是纯时长视图）。
type resolvedTimeline struct {
	slices []Slice
	direct map[int64]stream.Direct
}

// timelineFor 把播放单元展开成时间轴；索引缺失、或现存索引属于另一个通道的集
// 同步重建一次索引。
//
// resolveAll=true（playlist）：每个成员都现解析直链一次，拿不到就 502 —— 契约要求
// 播放列表在「该集解析不到直链」时直接失败。通道判据（web / app）就在这次解析的
// 结果里，所以「索引是不是别的通道建的」只有这条路径能判，也只需要这条路径判：
// 播放器总是先取 playlist 再取分片。
// resolveAll=false（分片）：只在需要建索引时才解析（探测要源地址），其余的集留给
// 调用方按需解析，免得一次分片请求把整组都解析一遍；已有索引的分片请求零外呼
// （契约「命中即回文件」）。
//
// 索引建立整体受 indexDeadline 约束：超时 → 503 + Retry-After（httpx.ErrUnavailable）。
func (c *Cutter) timelineFor(ctx context.Context, u playbackUnit, resolveAll bool) (resolvedTimeline, error) {
	idxCtx, cancel := context.WithTimeout(ctx, c.indexTimeout)
	defer cancel()

	eps := make([]EpisodeSegments, 0, len(u.eps))
	direct := make(map[int64]stream.Direct, len(u.eps))
	for _, ep := range u.eps {
		segs, err := c.st.ListSegments(ctx, ep.ID)
		if err != nil {
			return resolvedTimeline{}, fmt.Errorf("读分片索引剧集 %d: %w", ep.ID, err)
		}
		if resolveAll || len(segs) == 0 {
			d, err := c.resolve(ctx, ep)
			if err != nil {
				return resolvedTimeline{}, fmt.Errorf("解析剧集 %d 直链（%v）: %w", ep.ID, err, httpx.ErrUpstream)
			}
			direct[ep.ID] = d
			// 没有索引，或者索引是另一个通道（含没有通道段的老索引）留下的：重建。
			// 不重建就会把旧通道的片数、时长与分片键永久沿用下去（626 裁决 3）。
			if len(segs) == 0 || indexStale(segs, channelOf(d)) {
				if segs, err = c.ensureSegments(idxCtx, ep, d); err != nil {
					return resolvedTimeline{}, err
				}
			}
		}
		eps = append(eps, EpisodeSegments{EpisodeID: ep.ID, EpNo: ep.EpNo, Segments: segs})
	}
	return resolvedTimeline{slices: NewTimeline(eps).slices, direct: direct}, nil
}

// direct 取某一集的直链与 key：本次请求已解析过就复用，否则现解析一次。
//
// 解析失败包 httpx.ErrUpstream（契约：Resolve 失败一律 502），且绝不写库、不写缓存
// —— 调用方是缓存生成函数，返回错误即「不落缓存」。
func (c *Cutter) direct(ctx context.Context, tl resolvedTimeline, episodeID int64) (stream.Direct, error) {
	if d, ok := tl.direct[episodeID]; ok {
		return d, nil
	}
	ep, err := c.st.EpisodeByID(ctx, episodeID)
	if err != nil {
		return stream.Direct{}, fmt.Errorf("查分片所属剧集 %d: %w", episodeID, err)
	}
	d, err := c.resolve(ctx, ep)
	if err != nil {
		return stream.Direct{}, fmt.Errorf("解析剧集 %d 直链（%v）: %w", episodeID, err, httpx.ErrUpstream)
	}
	return d, nil
}

// parseSeqFile 解析 `<seq>.ts` 文件名：必须以 `.ts` 结尾，seq 是**纯十进制数字**
// （允许前导零：缓存 key 由 (uid, seq, codec_sig) 决定，本包不改写客户端 URL）。
// 显式拒绝符号与非数字字符——strconv.Atoi 会接受 `+1`/`-1`，那不是本包产出的 URL 形态。
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
// 调用方映射 404。合并集 uid 用 `m` 前缀，两者互不匹配（DESIGN D3）。
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

// writeVideoError 按播放面的错误矩阵写出响应：索引未就绪/超时 → 503 + Retry-After
// （不出半截流），其余走 httpx 统一映射（404 / 502 / 500）。
func writeVideoError(w http.ResponseWriter, err error) {
	if errors.Is(err, httpx.ErrUnavailable) {
		w.Header().Set("Retry-After", "5")
	}
	httpx.WriteError(w, err)
}

package segment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
)

// 播放面路由（docs/PLAYBACK-CONTRACT.md「两个口子」）。装配根把两个 pattern 挂在
// 顶层 mux 上；`{id}` 承载整段 `<uid>-<签名>`，`{file}` 承载 `<seq>.m4s`（Go 的路由
// pattern 不支持段内通配 `{seq}.m4s`，因此用整段承载）。
const (
	// PlaylistRoute 是播放列表端点：`GET|HEAD /video/{id}.m3u8`。
	PlaylistRoute = "GET /video/{id}"
	// SegmentRoute 是分片端点：`GET|HEAD /video/{id}/{seq}.m4s`。
	SegmentRoute = "GET /video/{id}/{file}"
)

// m3u8Suffix 是播放列表的扩展名（契约：单集与合并集同一路径形态）。
const m3u8Suffix = ".m3u8"

// ServePlaylist 实现 `GET /video/{id}.m3u8`：鉴权 → 展开播放单元 → 补齐每集时长 →
// 渲染 VOD playlist。
//
// 状态码（docs/PLAYBACK-CONTRACT.md「GET /video/{id}.m3u8」）：secret 读失败 500、
// 签名不对/缺 `-` 401、uid 非法或库里没有 404、解析不到直链 502、时长探测超时
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
	tl, err := c.timelineFor(ctx, u)
	if err != nil {
		writeVideoError(w, err)
		return
	}

	body := BuildPlaylist(id, tl.slices)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	// 响应已写完再记会话：采集在任何情况下都不能影响出流。
	c.recordOpen(ctx, r, uid, u.eps)
}

// ServeSegment 实现 `GET /video/{id}/{seq}.m4s`：鉴权 → 定位第 seq 集 → 缓存命中即回 /
// 未命中整集转封装落缓存再回。
//
// 缓存返回的 []byte 在 single-flight 下是多个等待者共享的只读字节，这里只读不写
// （ServeContent 只读）。Range 与 HEAD 都由 http.ServeContent 处理：分片是完整
// 不可变对象，不需要分段生成。
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
	// 「每集一片」：BuildPlaylist 按成员顺序编号，所以第 seq 片就是第 seq 集。
	if seq < 0 || seq >= len(u.eps) {
		httpx.WriteError(w, fmt.Errorf("%w: 播放单元 %s 没有分片 %d", httpx.ErrNotFound, uid, seq))
		return
	}
	ep := u.eps[seq]

	channel, direct, resolved, err := c.episodeChannel(ctx, ep)
	if err != nil {
		writeVideoError(w, err)
		return
	}
	key := cache.SegmentKey(episodeUID(ep.ID), channel)
	body, err := c.caches.GetOrGenerate(ctx,
		cache.Spec{Kind: store.ArtifactKindSegment, Key: key, Ext: segmentSuffix},
		func(ctx context.Context) ([]byte, error) {
			// 直链只在真要生成时才解析：命中缓存的分片请求零外呼
			// （契约「命中即回文件」，也是缓存存在的意义）。
			src := direct
			if !resolved {
				var rerr error
				if src, rerr = c.resolve(ctx, ep); rerr != nil {
					return nil, resolveFailure(ep.ID, rerr)
				}
			}
			out, err := remux(ctx, c.logger, src, c.fetchHeaders(src))
			if err != nil && errors.Is(err, errRejected) {
				// CDN 拒绝：作废这一集的直链、重解析一次、重试一次（ADR-0016）。
				fresh, refreshErr := c.refreshRejected(ctx, ep, channel)
				if refreshErr != nil {
					return nil, refreshErr
				}
				src = fresh
				out, err = remux(ctx, c.logger, src, c.fetchHeaders(src))
			}
			if err != nil {
				return nil, err
			}
			c.touchStream(ctx, ep.ID)
			// 每次真实生成一条 Info：AC「同一集第二次播放 ffmpeg 调用 0 次」的计次依据。
			c.logger.InfoContext(ctx, "segment generated",
				"episode_id", ep.ID, "channel", channel, "bytes", len(out))
			return out, nil
		})
	if err != nil {
		c.logger.WarnContext(ctx, "segment remux failed",
			"uid", uid, "seq", seq, "episode_id", ep.ID, "channel", channel, "err", err)
		// 生成失败（ffmpeg）是 502 且不写缓存；解析直链被拒（要登录）与「只有加密流」
		// 按插件哨兵的语义原样报出（403 / 501），不冒充上游失败；缓存自身故障按 500
		// 报，不冒充上游问题（MKACG-676）。
		switch {
		case errors.Is(err, httpx.ErrUpstream):
			writeVideoError(w, fmt.Errorf("转封装 %s 分片 %d（%v）: %w", uid, seq, err, httpx.ErrUpstream))
		case errors.Is(err, httpx.ErrForbidden), errors.Is(err, httpx.ErrUnsupported):
			writeVideoError(w, err)
		default:
			httpx.WriteError(w, fmt.Errorf("写分片 %s %d: %w", uid, seq, err))
		}
		return
	}

	// 集边界的时间戳：缓存里的字节一律 0 基（键只由「集 + 通道」决定），出流时按本片在
	// 播放单元里的起点平移 tfdt —— ffmpeg 系播放端（Emby 转码/DirectStream、mpv）按 PTS
	// 解码，只有 DISCONTINUITY 不重定基准会丢帧（ADR-0011 D6'）。
	start, err := c.timelineStart(ctx, uid, u.eps, seq)
	if err != nil {
		writeVideoError(w, err)
		return
	}
	served, err := shiftTimeline(body, start)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("平移分片 %s %d 的时间轴: %w", uid, seq, err))
		return
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(served))
	// 分片已经出流，再记进度（位置取本片在单元时间轴上的起点）。
	c.recordProgress(ctx, r, uid, seq, int(start.Seconds()), int64(len(served)))
}

// timelineStart 返回第 seq 片在播放单元时间轴上的起点（前面各集的 duration_sec 之和），
// 并在前置集时长还没回填时先补齐。
//
// 为什么必须补齐而不是「算不准就按 0」：起点决定出站片的 tfdt，算错就是集边界 PTS 回退、
// ffmpeg 系播放端丢帧（功能检测实测：直接请求分片、不取 playlist 时 4 片首 PTS 全 0，
// 拼成 playlist 后 1550 包只解出 800 帧）。所以缺时长时在这里现补齐 —— 解析一次直链 +
// ffprobe 取时长 + 回填（`ensureDuration`，与 playlist 路径同一份实现），补齐后写库，
// 后续请求回到「零外呼」。
//
// 代价边界：只有「直接请求分片 / 首播并发（分片早于 playlist 到达）」这一种情形会外呼，
// 次数 = 前面还没回填的集数（≤ 一个合并块的成员数，默认 10），且只发生一次。
// 补齐失败（上游解析不到 / 探测失败 / 超时）按播放面的错误矩阵返回，绝不出一片时间轴错误
// 的字节。
func (c *Cutter) timelineStart(ctx context.Context, uid string, eps []store.Episode, seq int) (time.Duration, error) {
	if start, exact := sliceStart(eps, seq); exact {
		return start, nil
	}
	ensureCtx, cancel := context.WithTimeout(ctx, c.ensureTimeout)
	defer cancel()

	// 前置集里还没回填时长的那些，一次批量解析（同一 series 页面只取一次）。
	missing := make([]int, 0, seq)
	for i := 0; i < seq && i < len(eps); i++ {
		if eps[i].DurationSec <= 0 {
			missing = append(missing, i)
		}
	}
	batch := make([]store.Episode, len(missing))
	for j, i := range missing {
		batch[j] = eps[i]
	}
	directs, err := c.resolveDirects(ctx, batch)
	if err != nil {
		return 0, err
	}
	for j, i := range missing {
		seconds, err := c.ensureDuration(ensureCtx, eps[i], directs[j])
		if err != nil {
			return 0, err
		}
		eps[i].DurationSec = int(math.Round(seconds))
		c.logger.InfoContext(ctx, "timeline start backfilled",
			"uid", uid, "seq", seq, "episode_id", eps[i].ID, "duration_sec", eps[i].DurationSec)
	}

	start, exact := sliceStart(eps, seq)
	if !exact {
		// 补齐后仍然为 0：这一集的源探测不出时长（异常源）。此时没有正确的时间轴可用，
		// 记一条 warn 继续用已有值出流 —— 比让整块播不了更可接受，也不会污染缓存。
		c.logger.WarnContext(ctx, "segment timeline start is approximate",
			"uid", uid, "seq", seq, "reason", "probe 拿不到前面某集的时长")
	}
	return start, nil
}

// sliceStart 返回第 seq 片在播放单元时间轴上的起点（前面各集的 duration_sec 之和），
// 以及这些前置集的时长是否都已回填。纯函数：补齐由调用方（timelineStart）负责。
//
// 时长用 episode.duration_sec（唯一真值，首播回填），与 playlist 里 EXTINF 是同一个值，
// 所以「起点 == 前面各条 EXTINF 之和」，播放端按它解码不会回退。
func sliceStart(eps []store.Episode, seq int) (time.Duration, bool) {
	exact := true
	total := 0
	for _, ep := range eps[:min(seq, len(eps))] {
		if ep.DurationSec <= 0 {
			exact = false
			continue
		}
		total += ep.DurationSec
	}
	return time.Duration(total) * time.Second, exact
}

// episodeChannel 取某集在 segment 表里的通道，并返回本次可直接复用的直链。
//
// 行在就是一次零外呼的读：命中缓存的分片请求从此不解析直链。行不在时才现解析
// 一次，按解析结果把行写进去（正常情况下 playlist 已经建好，这里只是兜底）。
// resolved=true 表示 direct 是本次刚解析的，生成时直接复用，不解析第二次。
func (c *Cutter) episodeChannel(ctx context.Context, ep store.Episode) (channel string, direct stream.Direct, resolved bool, err error) {
	seg, err := c.st.GetSegment(ctx, ep.ID)
	switch {
	case err == nil:
		return seg.Channel, stream.Direct{}, false, nil
	case !errors.Is(err, store.ErrNotFound):
		return "", stream.Direct{}, false, fmt.Errorf("读分片身份 episode_id=%d: %w", ep.ID, err)
	}

	direct, err = c.resolve(ctx, ep)
	if err != nil {
		return "", stream.Direct{}, false, resolveFailure(ep.ID, err)
	}
	channel = channelOf(direct)
	if err := c.st.UpsertSegment(ctx, store.Segment{EpisodeID: ep.ID, Channel: channel}); err != nil {
		return "", stream.Direct{}, false, fmt.Errorf("写分片身份 episode_id=%d: %w", ep.ID, err)
	}
	return channel, direct, true, nil
}

// playbackUnit 是一个播放单元：单集或合并块，eps 按播放顺序排列。
type playbackUnit struct {
	eps []store.Episode
}

// unit 把 uid 展开成播放单元。
//
// 「合法 uid」由当前派生规则决定（硬约束 6 + 契约「不飘移」第 3 条）：某集被并进
// 合并块后它的 `e<id>` 立刻不是播放单元，改 N 后旧合并 uid 也不再匹配，旧 URL 必须
// 404 —— 展开与合法性判断都在 store.PlaybackEpisodes 一处（docs/adr/0014）。
// uid 语法不认识、不是当前派生出的播放单元 → httpx.ErrNotFound（404）。
func (c *Cutter) unit(ctx context.Context, uid string) (playbackUnit, error) {
	eps, err := c.st.PlaybackEpisodes(ctx, uid)
	if errors.Is(err, store.ErrNotFound) {
		return playbackUnit{}, fmt.Errorf("%w: 播放单元 %s", httpx.ErrNotFound, uid)
	}
	if err != nil {
		return playbackUnit{}, fmt.Errorf("展开播放单元 %s: %w", uid, err)
	}
	return playbackUnit{eps: eps}, nil
}

// timelineFor 把一个播放单元的成员集展开成时间轴：每集一片。
//
// 整块成员的直链一次解析（resolveDirects → 批量能力）：同一 series 的页面只取一次，
// 请求数不随成员数增长（MKACG-677）。拿不到就 502，不放一份「能读、一播就失败」的
// 播放列表。解析结果同时给出通道，顺手把 segment 行的 channel 对齐（626 裁决 3 的
// 机制原样保留：判据仍然只有 channelOf 一处）。时长取 episode.duration_sec，为 0 的
// 现 ffprobe 一次并回填（ADR-0011 D2：唯一真值，不再有第二份时长）。
//
// 整体受 ensureDeadline 约束：超时 → 503 + Retry-After（httpx.ErrUnavailable）。
func (c *Cutter) timelineFor(ctx context.Context, u playbackUnit) (*Timeline, error) {
	ensureCtx, cancel := context.WithTimeout(ctx, c.ensureTimeout)
	defer cancel()

	directs, err := c.resolveDirects(ctx, u.eps)
	if err != nil {
		return nil, err
	}
	eps := make([]TimelineEpisode, 0, len(u.eps))
	for i, ep := range u.eps {
		dur, err := c.ensureDuration(ensureCtx, ep, directs[i])
		if err != nil {
			return nil, err
		}
		if err := c.ensureChannel(ctx, ep.ID, channelOf(directs[i])); err != nil {
			return nil, err
		}
		eps = append(eps, TimelineEpisode{EpisodeID: ep.ID, Dur: dur})
	}
	return NewTimeline(eps), nil
}

// episodeUID 是播放单元句柄里「一集」的形态：`e<episode.id>`（DESIGN D3）。
// 分片缓存键用它做前半段——片段只由「哪一集 + 哪个通道」决定（ADR-0011 D3）。
func episodeUID(episodeID int64) string {
	return "e" + strconv.FormatInt(episodeID, 10)
}

// parseSeqFile 解析 `<seq>.m4s` 文件名：必须以 `.m4s` 结尾，seq 是**纯十进制数字**
// （允许前导零：缓存键由 (集, 通道) 决定，本包不改写客户端 URL）。
// 显式拒绝符号与非数字字符——strconv.Atoi 会接受 `+1`/`-1`，那不是本包产出的 URL 形态。
func parseSeqFile(name string) (int, bool) {
	seqStr, ok := strings.CutSuffix(name, segmentSuffix)
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

// resolveDirects 一次解析一组成员的直链（批量能力，同一 series 页面只取一次），
// 失败按播放面的错误矩阵报出。返回顺序与 eps 对齐。
func (c *Cutter) resolveDirects(ctx context.Context, eps []store.Episode) ([]stream.Direct, error) {
	if len(eps) == 0 {
		return nil, nil
	}
	directs, err := c.resolveMany(ctx, eps)
	if err != nil {
		return nil, fmt.Errorf("解析整块成员直链（%v）: %w", err, playErr(err))
	}
	if len(directs) != len(eps) {
		return nil, fmt.Errorf("解析 %d 集直链只回来 %d 条: %w", len(eps), len(directs), httpx.ErrUpstream)
	}
	return directs, nil
}

// resolveFailure 把「解析某一集直链失败」折成出流面的错误（类别见 playErr）。
func resolveFailure(epID int64, err error) error {
	return fmt.Errorf("解析剧集 %d 直链（%v）: %w", epID, err, playErr(err))
}

// playErr 把「解析直链失败」折成出流面的错误类别。
//
// 三类原因必须分开报（MKACG-676）：
//   - 需要登录态 / 该集无权限 → 403：用户该做的是配一次登录 Cookie，不是换源；
//   - 只提供拿不到 key 的加密流 → 501：这一集本服务就是播不了，重试无用；
//   - 其余（上游故障、被拒、格式看不懂）→ 502：源侧的问题。
//
// 折成同一个 502 时，前两种会被显示成「源坏了」，配置项也就失去意义。
func playErr(err error) error {
	switch {
	case errors.Is(err, plugin.ErrLoginRequired):
		return httpx.ErrForbidden
	case errors.Is(err, plugin.ErrUnsupported):
		return httpx.ErrUnsupported
	}
	return httpx.ErrUpstream
}

// writeVideoError 按播放面的错误矩阵写出响应：时长探测超时 → 503 + Retry-After
// （不出半截流），其余走 httpx 统一映射（404 / 403 / 501 / 502 / 500）。
func writeVideoError(w http.ResponseWriter, err error) {
	if errors.Is(err, httpx.ErrUnavailable) {
		w.Header().Set("Retry-After", "5")
	}
	httpx.WriteError(w, err)
}

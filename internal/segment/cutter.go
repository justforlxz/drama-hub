package segment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
)

// VerifyFunc 校验出流签名 token（ADR-0003）：secret 是 settings.stream_secret 的
// 字节，uid 是 URL 里的句柄。由装配根注入（cmd 传 stream.Verify），避免本包
// 反向依赖同 rank 的 internal/stream（check-deps 会判同级依赖）。
type VerifyFunc func(secret []byte, uid, token string) bool

// Route 是分片端点在装配根的注册 pattern。`{file}` 形如 `<seq>.ts`；seq 从 0 起，
// 前导零也接受（缓存代理按整串 URL 做 key，本包不改写客户端 URL）。
// Go 的路由 pattern 不支持段内通配（`{seq}.ts` 非法），因此用 `{file}` 承载整段。
const Route = "GET /s/{token}/seg/{uid}/{file}"

// Cutter 是分片引擎：关键帧索引、时间轴映射与切片端点共用同一个实例
// （store / settings / 并发闸只构造一次）。st / settings 不可为 nil。
type Cutter struct {
	st       *store.Store
	settings *store.Settings
	logger   *slog.Logger
	verify   VerifyFunc
	gate     chan struct{}
}

// NewCutter 装配分片引擎。verify 由装配根注入（ADR-0003，见 VerifyFunc）。
func NewCutter(st *store.Store, settings *store.Settings, logger *slog.Logger, verify VerifyFunc) *Cutter {
	return &Cutter{
		st:       st,
		settings: settings,
		logger:   logger,
		verify:   verify,
		gate:     make(chan struct{}, ffmpegSlots()),
	}
}

// ffmpegSlots 是 ffmpeg/ffprobe 并发闸上限：`min(4, max(1, NumCPU/2))`。
// 固定值，不做成可调项（docs/DEV-STANDARDS.md「配置」表）。闸只限制并发进程数，
// 超出的调用排队等待，不丢弃。
func ffmpegSlots() int {
	n := runtime.NumCPU() / 2
	switch {
	case n < 1:
		return 1
	case n > 4:
		return 4
	default:
		return n
	}
}

// acquire 在并发闸上排队；ctx 取消时放弃排队。
func (c *Cutter) acquire(ctx context.Context) error {
	select {
	case c.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Cutter) release() { <-c.gate }

// Index 用 ffprobe 预扫某集的源，把切点对齐到关键帧后整体写入 segment 表，
// 返回该集的分片索引。
//
// 调用点：索引是「一次预扫、非每次播放」的派生数据，触发方属 M7（合并编排）
// 与 R1（管理面）；本模块负责能力与持久化，不在请求路径上自动触发。
//
// 幂等：同一源反复调用得到同一份切点（ReplaceSegments 一事务内先删后插）。
// 失败不写任何行——探测失败时库里保留旧索引（或保持为空），不落半套切点：
// 半套/错误的切点会让后续切片错位，而「失败结果不缓存」是硬性验收。
//
// 错误：剧集不存在返回 store.ErrNotFound；源地址为空或 ffprobe 失败返回
// httpx.ErrUpstream；settings 读失败、写库失败按原样带上下文返回（500）。
func (c *Cutter) Index(ctx context.Context, episodeID int64) ([]store.Segment, error) {
	ep, err := c.st.EpisodeByID(ctx, episodeID)
	if err != nil {
		return nil, err // store.ErrNotFound 透传，调用方映射 404
	}
	if ep.PlayURL == "" {
		return nil, fmt.Errorf("剧集 %d 没有播放地址: %w", episodeID, httpx.ErrUpstream)
	}
	target, err := c.settings.Int(ctx, store.KeySegTargetSec)
	if err != nil {
		return nil, fmt.Errorf("读 seg_target_sec: %w", err)
	}

	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	info, err := probe(ctx, ep.PlayURL)
	c.release()
	if err != nil {
		return nil, err
	}

	segs := planSegments(info, episodeID, float64(target))
	if err := c.st.ReplaceSegments(ctx, episodeID, segs); err != nil {
		return nil, fmt.Errorf("写入分片索引: %w", err)
	}
	c.logger.InfoContext(ctx, "segment index rebuilt",
		"episode_id", episodeID, "segments", len(segs), "duration_sec", info.durationSec)
	return segs, nil
}

// sourceInfo 是 ffprobe 探测结果的最小形态。
type sourceInfo struct {
	keyframes   []float64 // 关键帧 pts（秒，升序）
	durationSec float64
	codecSig    string
}

// planSegments 把探测结果切成「目标时长附近、切点对齐关键帧」的分片（纯函数）。
//
// 规则：首片从 0 起（整集覆盖，总时长才等于源时长），之后每个切点取第一个
// 距当前切点 `>= target` 的关键帧。因为切点都落在关键帧上，`-c copy` 切片内部
// 不会漂移——这正是「目标 10s 实测 10.66s」要避免的错位；代价是真实片长 ≥ target
// （由 EXTINF 如实反映，不是缺陷）。末片收到源时长，可以短于 target。
//
// 不变量：Σ Dur == info.durationSec（± 浮点误差）。
func planSegments(info sourceInfo, episodeID int64, target float64) []store.Segment {
	seg := func(start, dur float64) store.Segment {
		return store.Segment{
			EpisodeID: episodeID, StartSec: start, Dur: dur,
			Keyframe: true, CodecSig: info.codecSig, Mode: "copy",
		}
	}
	if len(info.keyframes) == 0 || info.durationSec <= 0 {
		// 探测不到关键帧/时长（异常源）：退化为整段一片。切片时若源并不以关键帧
		// 开头，ffmpeg 会自行对齐到首个可拷贝帧，片内仍有内容而不是空片。
		return []store.Segment{seg(0, info.durationSec)}
	}

	segs := make([]store.Segment, 0, 16)
	start := 0.0
	for _, kf := range info.keyframes {
		// 首片固定从 0 起：源里首个关键帧通常就是 0；若略大于 0（编码器偏移），
		// 从 0 起切仍由 ffmpeg 对齐到首帧，且总时长不丢头。
		if kf-start+1e-9 < target {
			continue
		}
		segs = append(segs, seg(start, kf-start))
		start = kf
	}
	// 末片收到源时长（真实尾部，可能短于 target；时长差 ≤ 一帧）。
	if info.durationSec > start+1e-9 {
		segs = append(segs, seg(start, info.durationSec-start))
	}
	return segs
}

// 子进程硬超时：源直链卡死时以超时结束，不挂住请求（DEV-STANDARDS 出站超时）。
const (
	timeoutProbe = 30 * time.Second
	timeoutCut   = 60 * time.Second
)

// stderrTail 取子进程 stderr 的尾部作为失败原因。cmd.Output() 在 c.Stderr 为 nil
// 时把 stderr 挂在 *exec.ExitError 上（stdout 在 out 里），因此从那里取。
// 只留尾部 200 字节，且不落完整源 URL（见 safeHost）。
func stderrTail(err error, out []byte) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		out = ee.Stderr
	}
	s := strings.TrimSpace(string(out))
	if len(s) > 200 {
		s = s[len(s)-200:]
	}
	return s
}

// probe 执行 `ffprobe -skip_frame nokey`：只解出关键帧，收集 pts 与源时长。
//
// 失败返回 httpx.ErrUpstream（带 stderr 尾部与源 host；不含完整 URL —— 直链可能
// 带签名）。
func probe(ctx context.Context, src string) (sourceInfo, error) {
	dctx, cancel := context.WithTimeout(ctx, timeoutProbe)
	defer cancel()
	cmd := exec.CommandContext(dctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-skip_frame", "nokey",
		"-show_entries",
		"frame=pts_time:format=duration:stream=codec_name,profile,level,pix_fmt,codec_tag_string,time_base",
		"-of", "json",
		src,
	)
	out, err := cmd.Output()
	if err != nil {
		return sourceInfo{}, fmt.Errorf("ffprobe %s: %w（%s）", safeHost(src), httpx.ErrUpstream, stderrTail(err, out))
	}
	var decoded struct {
		Frames []struct {
			PtsTime string `json:"pts_time"`
		} `json:"frames"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecName      string `json:"codec_name"`
			Profile        string `json:"profile"`
			Level          int    `json:"level"`
			PixFmt         string `json:"pix_fmt"`
			CodecTagString string `json:"codec_tag_string"`
			TimeBase       string `json:"time_base"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		return sourceInfo{}, fmt.Errorf("解析 ffprobe 输出: %w（%v）", httpx.ErrUpstream, err)
	}

	var info sourceInfo
	for _, f := range decoded.Frames {
		pts, perr := strconv.ParseFloat(f.PtsTime, 64)
		if perr != nil || pts < 0 {
			continue
		}
		info.keyframes = append(info.keyframes, pts)
	}
	info.durationSec, _ = strconv.ParseFloat(decoded.Format.Duration, 64)
	if len(decoded.Streams) > 0 {
		s := decoded.Streams[0]
		info.codecSig = strings.Join([]string{
			s.CodecName, s.Profile, strconv.Itoa(s.Level), s.PixFmt, s.CodecTagString, s.TimeBase,
		}, "/")
	}
	return info, nil
}

// cut 执行 ffmpeg：`-ss` 前置（输入寻址，切点已是关键帧所以不漂移）→ `-c copy`
// → MPEG-TS 写到 stdout。返回完整字节；失败返回 httpx.ErrUpstream。
//
// offset 是该片在合并时间轴上的绝对起点（单集传 0）：非零时用 -output_ts_offset
// 把该片的 PTS/DTS 平移到绝对位置，跨集的连续时间轴因此没有时间戳回退——否则
// 每集从 0 重新计时，播放器会在集边界丢帧（实测 ffmpeg 会报 non-monotonically
// increasing dts，虽不致命但会掉帧）。
//
// 字节全部收进内存再返回，调用方才能「先定状态码再写响应」：ffmpeg 中途失败时
// 客户端看到的是干净的 502，不是半截流。单片 10s 约 2–5 MB（DESIGN §2）。
func cut(ctx context.Context, src string, start, dur, offset float64) ([]byte, error) {
	dctx, cancel := context.WithTimeout(ctx, timeoutCut)
	defer cancel()
	args := []string{
		"-v", "error",
		"-ss", formatSeconds(start),
		"-i", src,
		"-t", formatSeconds(dur),
		"-c", "copy",
	}
	if offset > 0 {
		args = append(args, "-output_ts_offset", formatSeconds(offset))
	}
	args = append(args, "-f", "mpegts", "pipe:1")
	cmd := exec.CommandContext(dctx, "ffmpeg", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg 切片 %s: %w（%s）", safeHost(src), httpx.ErrUpstream, stderrTail(err, out))
	}
	return out, nil
}

// formatSeconds 把秒格式化成 ffmpeg 接受的十进制（毫秒精度足够：切点已是关键帧）。
func formatSeconds(v float64) string {
	return strconv.FormatFloat(v, 'f', 3, 64)
}

// safeHost 只取源的 host（定位用），不落完整 URL —— 直链可能带签名。
func safeHost(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	rest := raw[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

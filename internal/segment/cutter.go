package segment

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
)

// ResolveFunc 在请求时解析一集的直链与解密 key（装配根注入 stream.Resolver.Direct）。
//
// 契约要求直链只在请求时解析、任何列都不存（docs/PLAYBACK-CONTRACT.md）：
// 本包不 import plugin，解析能力由上层以函数注入，错误原样透出。
type ResolveFunc func(ctx context.Context, ep store.Episode) (stream.Direct, error)

// Cutter 是播放面的中枢：`/video/{id}.m3u8` 与 `/video/{id}/{seq}.ts` 的 handler、
// 按需建立的关键帧索引、以及分片缓存共用同一个实例（store / settings / 缓存 /
// 并发闸只构造一次）。st / settings / caches / resolve 不可为 nil。
type Cutter struct {
	st       *store.Store
	settings *store.Settings
	logger   *slog.Logger
	caches   *cache.Cache
	resolve  ResolveFunc
	gate     chan struct{}
	// headerBlock 是取直链时传给 ffmpeg / ffprobe 的 `-headers` 值（CRLF 分隔）；
	// 空 = 不带。本包不认识任何源，这一份由装配根注入（见 NewCutter 的 headers）。
	headerBlock string
	// indexTimeout 是「按需建立索引」的硬上限，生产固定 indexDeadline；
	// 测试把它调短，才能在 5 分钟测试预算里验证 503 那条路径。
	indexTimeout time.Duration
}

// NewCutter 装配播放面。caches 是 S7 的本地缓存（分片落盘 + single-flight），
// resolve 由装配根注入（stream.Resolver.DirectURL）。
//
// headers 是 ffmpeg / ffprobe 取直链时要带的出站请求头（`名称: 值` 每项一行），
// 由装配根从插件侧的上游身份注入——源侧要浏览器 UA/Referer，而这两个子进程不走
// http.Client，只能靠 `-headers`（见 docs/PLAYBACK-CONTRACT.md）。nil = 不带。
func NewCutter(st *store.Store, settings *store.Settings, logger *slog.Logger, caches *cache.Cache, resolve ResolveFunc, headers []string) *Cutter {
	return &Cutter{
		st:          st,
		settings:    settings,
		logger:      logger,
		caches:      caches,
		resolve:     resolve,
		gate:        make(chan struct{}, ffmpegSlots()),
		headerBlock: headerBlock(headers),

		indexTimeout: indexDeadline,
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

// indexDeadline 是「按需建立关键帧索引」的硬上限（docs/PLAYBACK-CONTRACT.md：
// 30 s，超时 503 + Retry-After）。只有第一次播放某个 uid 时会走到这里。
const indexDeadline = 30 * time.Second

// ensureSegments 同步 ffprobe 建一次某集的分片索引并落库，顺手把实测时长回填到
// episode.duration_sec。「该不该建」由调用方（timelineFor）判定：库里没有行，或者
// 现有行的通道段与本次请求的通道不一致。
//
// 索引按需建立、禁止批量预建（库里 2847 万集，全库预扫不可完成）。建立超时
// （indexDeadline）返回 httpx.ErrUnavailable —— 边界包据此回 503 + Retry-After，
// 不出半截时间轴。探测失败不写任何行（库里保留旧索引或保持为空）。
func (c *Cutter) ensureSegments(ctx context.Context, ep store.Episode, d stream.Direct) ([]store.Segment, error) {
	target, err := c.settings.Int(ctx, store.KeySegTargetSec)
	if err != nil {
		return nil, fmt.Errorf("读 seg_target_sec: %w", err)
	}
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	info, probeErr := probe(ctx, d, c.headerBlock)
	c.release()
	if probeErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("剧集 %d 的关键帧索引建立超时: %w", ep.ID, httpx.ErrUnavailable)
		}
		return nil, probeErr
	}
	// 通道段并进签名：索引行与分片缓存键都用这一个值（channel.go）。
	info.codecSig = channelSig(channelOf(d), info.codecSig)
	// 首次建索引顺手把实测时长回填进 episode.duration_sec：ffprobe 已经拿到了真实
	// 时长，白丢它会让 .nfo 的 <runtime> 恒为 0（Emby 全库时长 0、进度条与续播失真）。
	// 回填排在写索引之前：反过来一旦索引写成功而回填失败，下次请求会命中已有索引
	// 直接返回，时长就再也补不上了。
	if _, err := c.st.BackfillEpisodeDuration(ctx, ep.ID, int(math.Round(info.durationSec))); err != nil {
		return nil, fmt.Errorf("回填剧集 %d 时长: %w", ep.ID, err)
	}
	segs := planSegments(info, ep.ID, float64(target))
	if err := c.st.ReplaceSegments(ctx, ep.ID, segs); err != nil {
		return nil, fmt.Errorf("写入分片索引 episode_id=%d: %w", ep.ID, err)
	}
	c.logger.InfoContext(ctx, "segment index built",
		"episode_id", ep.ID, "segments", len(segs), "duration_sec", info.durationSec,
		"channel", channelOf(d))
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

// headerBlock 把 `名称: 值` 列表拼成 ffmpeg / ffprobe 的 `-headers` 值：行间 CRLF，
// 末尾再补一个 CRLF（ffmpeg 以 CRLF 切分，末行没有终止符会被丢掉）。
// 空输入返回空串，调用方据此不加 `-headers`（测试与不需要头的源）。
func headerBlock(headers []string) string {
	if len(headers) == 0 {
		return ""
	}
	return strings.Join(headers, "\r\n") + "\r\n"
}

// headerArgs 返回要插在输入前的 `-headers` 参数；block 为空时返回 nil，
// 命令行里就一个多余参数都没有。
//
// ffmpeg 与 ffprobe 都支持它（`-headers` 是 http 协议的输入选项，位置在输入
// 地址之前）：ffprobe 里实测带与不带都探出同一个 h264 / 10.066667 秒，带上是
// 为了与 ffmpeg 走同一份出站身份。
func headerArgs(block string) []string {
	if block == "" {
		return nil
	}
	return []string{"-headers", block}
}

// decryptionArgs 返回要插在输入前的 `-decryption_key <hex>`（ffmpeg / ffprobe 的
// mov 解复用器选项，CENC-AES-CTR 由 ffmpeg 原生解）。明文流返回 nil：命令行里
// 一个多余参数都没有 —— 传空值会被 ffmpeg 当非法参数。
//
// key 只在这里变成命令行参数：不进日志、不进错误文本（docs/DEV-STANDARDS.md
// 「播放面」）。
func decryptionArgs(key []byte) []string {
	if len(key) == 0 {
		return nil
	}
	return []string{"-decryption_key", hex.EncodeToString(key)}
}

// probeArgs 拼 `ffprobe -skip_frame nokey` 的完整 argv（纯函数，argv 顺序就是契约：
// `-decryption_key` 与 `-headers` 必须落在源地址之前，否则 ffmpeg 只把它们当输出
// 选项、静默忽略）。
func probeArgs(src stream.Direct, headers string) []string {
	args := []string{
		"-v", "error",
		"-select_streams", "v:0",
		"-skip_frame", "nokey",
		"-show_entries",
		"frame=pts_time:format=duration:stream=codec_name,profile,level,pix_fmt,codec_tag_string,time_base",
		"-of", "json",
	}
	args = append(args, headerArgs(headers)...)
	args = append(args, decryptionArgs(src.Key)...)
	return append(args, src.URL)
}

// probe 执行 `ffprobe -skip_frame nokey`：只解出关键帧，收集 pts 与源时长。
//
// 失败返回 httpx.ErrUpstream（带 stderr 尾部与源 host；不含完整 URL —— 直链可能
// 带签名）。
//
// 密文源要单独判一次解密自检：key 错的时候 ffprobe 常常**以 0 退出**、只往 stderr
// 吐 `non-existing PPS …`，拿不到关键帧却也不算失败 —— 那种「成功」会把空索引写进
// 库、把空分片写进缓存。所以 key 非空时 stderr 必须为空（实测正确 key 下 0 字节），
// 非空即解密失败（MKACG-626 AC-8）。明文流不看 stderr：`-v error` 下源本身也会吐
// 无害告警，按它判失败会把好源打成 502。
func probe(ctx context.Context, src stream.Direct, headers string) (sourceInfo, error) {
	dctx, cancel := context.WithTimeout(ctx, timeoutProbe)
	defer cancel()
	cmd := exec.CommandContext(dctx, "ffprobe", probeArgs(src, headers)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return sourceInfo{}, fmt.Errorf("ffprobe %s: %w（%s）", safeHost(src.URL), httpx.ErrUpstream, stderrTail(err, stderr.Bytes()))
	}
	if len(src.Key) > 0 && stderr.Len() > 0 {
		return sourceInfo{}, fmt.Errorf("ffprobe %s 解密自检失败: %w（%s）",
			safeHost(src.URL), httpx.ErrUpstream, stderrTail(nil, stderr.Bytes()))
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

// cutArgs 拼 `ffmpeg` 切片的完整 argv（纯函数）：`-decryption_key` 与 `-headers`
// 落在 `-i` 之前，才是输入选项；`-ss` 也在 `-i` 之前（输入寻址）。
func cutArgs(src stream.Direct, start, dur, offset float64, headers string) []string {
	args := append([]string{"-v", "error"}, headerArgs(headers)...)
	args = append(args, decryptionArgs(src.Key)...)
	args = append(args,
		"-ss", formatSeconds(start),
		"-i", src.URL,
		"-t", formatSeconds(dur),
		"-c", "copy",
	)
	if offset > 0 {
		args = append(args, "-output_ts_offset", formatSeconds(offset))
	}
	return append(args, "-f", "mpegts", "pipe:1")
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
//
// src.Key 非空时解密交给 ffmpeg 原生 `-decryption_key`（不自己解 CENC、不落整集
// 明文）：同一条 `-ss … -c copy` 路径，只是多一个输入选项。
func cut(ctx context.Context, src stream.Direct, start, dur, offset float64, headers string) ([]byte, error) {
	dctx, cancel := context.WithTimeout(ctx, timeoutCut)
	defer cancel()
	cmd := exec.CommandContext(dctx, "ffmpeg", cutArgs(src, start, dur, offset, headers)...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg 切片 %s: %w（%s）", safeHost(src.URL), httpx.ErrUpstream, stderrTail(err, out))
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

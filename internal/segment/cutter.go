package segment

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/playback"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
)

// ResolveFunc 在请求时解析一集的直链与解密 key（装配根注入 stream.Resolver.Direct）。
//
// 直链的复用与落库由 stream.Resolver 负责（episode_stream，按 expires_at 判定）；
// 本包不 import plugin，解析能力由上层以函数注入，错误原样透出。
type ResolveFunc func(ctx context.Context, ep store.Episode) (stream.Direct, error)

// ResolveManyFunc 一次解析同一部剧多集的直链（装配根注入 stream.Resolver.DirectMany）。
//
// 播放列表一次要解析整块成员：逐集调 ResolveFunc 会把「一次页面请求」放大成 M 次
// （MKACG-677 的 502 来源之一）。批量能力由上层以函数注入，本包仍不 import plugin。
// 返回顺序必须与 eps 一一对应；长度不符按上游故障处理。
type ResolveManyFunc func(ctx context.Context, eps []store.Episode) ([]stream.Direct, error)

// InvalidateFunc 作废某集的直链（装配根注入 stream.Resolver.Invalidate）。
//
// CDN 明确拒绝（401/403/404）时用一次：只作废这一集，下一次解析拿新链
// （docs/adr/0016-episode-stream-cache.md「失败自愈」）。
type InvalidateFunc func(ctx context.Context, episodeID int64) error

// Cutter 是播放面的中枢：`/video/{id}.m3u8` 与 `/video/{id}/{seq}.m4s` 的 handler、
// 每集的时长探测（episode.duration_sec）、segment 行的通道对齐、以及分片缓存共用
// 同一个实例（store / settings / 缓存 / 并发闸只构造一次）。st / settings / caches /
// resolve 不可为 nil。
type Cutter struct {
	st       *store.Store
	settings *store.Settings
	logger   *slog.Logger
	caches   *cache.Cache
	resolve  ResolveFunc
	// resolveMany 是播放列表用的批量解析（一次页面请求解析整块成员）；单集兜底
	// 路径（分片先到）仍用 resolve。
	resolveMany ResolveManyFunc
	// invalidate 作废某集的直链（CDN 拒绝时用一次，只作废这一集）。
	invalidate InvalidateFunc
	gate       *ffmpegGate
	// headers 是取直链时传给 ffmpeg / ffprobe 的出站身份，两个通道各一份；
	// 用哪一份由本次解析出的直链决定（见 fetchHeaders）。本包不认识任何源，
	// 两份都由装配根注入（见 NewCutter 的 headers）。
	headers Headers
	// playback 是「正在播放」的内存会话表（MKACG-683）。只写不读、失败不影响出流：
	// 记录的时机在响应写完之后，nil 时整段跳过。
	playback *playback.Tracker
	// ensureTimeout 是「补齐播放列表所需信息」（每个成员的直链 + 时长）的硬上限，
	// 生产固定 ensureDeadline；测试把它调短，才能在 5 分钟测试预算里验证 503 那条路径。
	ensureTimeout time.Duration
	// 预取（ADR-0017）：pfCtx 是进程级 ctx（StartPrefetch 注入），pfRun 是当前剧的
	// 预取任务。两者都由 pfMu 保护；pfCtx 为 nil 时 kickPrefetch 是空操作。
	pfCtx context.Context
	pfMu  sync.Mutex
	pfRun *prefetchRun
	// 分片预热（MKACG-682）：按播放单元（uid）单飞，只预热当前块的剩余成员与后面
	// segment_prefetch_blocks 个块。生命周期同样挂在 pfCtx 上，由 spMu 保护。
	spMu  sync.Mutex
	spRun *segmentPrefetchRun
	// lastPlay 是最近一次播放入口请求的时间（unix nano）：播放列表与分片成功都会刷新。
	// 分片预热用它判断「前台是否已经静默」（播放结束 / 客户端断开），静默就不再生新分片。
	lastPlay atomic.Int64
}

// Headers 是 `ffmpeg` / `ffprobe` 取直链时的出站身份（`名称: 值` 每项一行），
// 两个通道各一份。**同一份身份不能同时喂给两个通道**（MKACG-638）：网页通道的
// 直链（`hongguoduanju.com/video/public/…`）要浏览器 UA + 源站根 `Referer`，
// 而 App 通道的媒体 CDN 对带站内 `Referer` 的请求直接 403 —— 实测二分：仅 `Referer`
// 403，无头 / 仅 UA 都是完整集。两份都由装配根从插件常量派生（`internal/segment`
// 不认识任何源，也不提供默认 UA；见 docs/PLAYBACK-CONTRACT.md「出站身份」）。
type Headers struct {
	Web []string // 网页通道：浏览器 UA + 源站根 Referer
	App []string // App 通道：浏览器 UA（不带 Referer）
}

// NewCutter 装配播放面。caches 是本地缓存（分片落盘 + single-flight），
// resolve / resolveMany / invalidate 由装配根注入（stream.Resolver 的同名方法）。
//
// headers 是 ffmpeg / ffprobe 取直链时要带的出站请求头，按通道两份：这两个子进程
// 不走 http.Client，只能靠 `-headers`。空切片 = 该通道不带（见 Headers）。
//
// playback 是「正在播放」的会话表（MKACG-683）：播放列表与分片成功写出后各记一笔。
func NewCutter(st *store.Store, settings *store.Settings, logger *slog.Logger, caches *cache.Cache, resolve ResolveFunc, resolveMany ResolveManyFunc, invalidate InvalidateFunc, headers Headers, pb *playback.Tracker) *Cutter {
	return &Cutter{
		st:          st,
		settings:    settings,
		logger:      logger,
		caches:      caches,
		resolve:     resolve,
		resolveMany: resolveMany,
		invalidate:  invalidate,
		gate:        newFFmpegGate(ffmpegSlots()),
		headers:     headers,
		playback:    pb,

		ensureTimeout: ensureDeadline,
	}
}

// fetchHeaders 按本次解析出的直链挑一份通道身份，拼成 `-headers` 的值。
//
// 通道判据只有 channelOf 一处，出站身份跟着它走：判据发现两条通道的字节流不同，
// 出站身份也必须不同，否则一条通道的头会把另一条通道的请求打成 403（MKACG-638）。
func (c *Cutter) fetchHeaders(d stream.Direct) string {
	if channelOf(d) == channelApp {
		return headerBlock(c.headers.App)
	}
	return headerBlock(c.headers.Web)
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

// acquire 在前台并发闸上排队；ctx 取消时放弃排队。前台优先于后台预热（见 ffmpegGate）。
func (c *Cutter) acquire(ctx context.Context) error { return c.gate.acquire(ctx) }

func (c *Cutter) release() { c.gate.release() }

// ensureDeadline 是「补齐播放列表所需信息」的硬上限（docs/PLAYBACK-CONTRACT.md：
// 30 s，超时 503 + Retry-After）。只有第一次播放某个 uid 时会走到 ffprobe。
const ensureDeadline = 30 * time.Second

// ensureDuration 返回某集的时长（秒）。
//
// episode.duration_sec 是唯一真值（ADR-0011 D2）：非 0 直接用它；为 0（插件恒给 0，
// 这一集还没人播过）时现 ffprobe 一次并回填 —— 回填只填 0，之后 playlist 的
// EXTINF、TARGETDURATION 与 .nfo 的 <runtime> 都从这一个值派生（docs/adr/0014）。
//
// CDN 明确拒绝（401/403/404）时按 ADR-0016 自愈：作废这一集的直链、重解析一次、
// 重试一次；再失败才上报。重解析后通道变了就报错，不把 App 字节当网页字节用。
//
// 探测失败不写任何东西；超时（ensureDeadline）返回 httpx.ErrUnavailable，边界包据此
// 回 503 + Retry-After，不出半截时间轴。
func (c *Cutter) ensureDuration(ctx context.Context, ep store.Episode, d stream.Direct) (float64, error) {
	if ep.DurationSec > 0 {
		return float64(ep.DurationSec), nil
	}
	if err := c.acquire(ctx); err != nil {
		return 0, err
	}
	seconds, probeErr := probeDuration(ctx, c.logger, d, c.fetchHeaders(d))
	c.release()
	if probeErr != nil && errors.Is(probeErr, errRejected) {
		fresh, refreshErr := c.refreshRejected(ctx, ep, channelOf(d))
		if refreshErr != nil {
			return 0, refreshErr
		}
		d = fresh
		if err := c.acquire(ctx); err != nil {
			return 0, err
		}
		seconds, probeErr = probeDuration(ctx, c.logger, d, c.fetchHeaders(d))
		c.release()
	}
	if probeErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 0, fmt.Errorf("剧集 %d 的时长探测超时: %w", ep.ID, httpx.ErrUnavailable)
		}
		return 0, probeErr
	}
	rounded := int(math.Round(seconds))
	if _, err := c.st.BackfillEpisodeDuration(ctx, ep.ID, rounded); err != nil {
		return 0, fmt.Errorf("回填剧集 %d 时长: %w", ep.ID, err)
	}
	c.touchStream(ctx, ep.ID)
	c.logger.InfoContext(ctx, "episode duration probed",
		"episode_id", ep.ID, "duration_sec", rounded, "channel", channelOf(d))
	return float64(rounded), nil
}

// refreshRejected 在 CDN 明确拒绝后作废这一集的直链并重解析一次。
//
// 只动这一集（不因为一条失败清掉整部剧）；重解析失败按播放面错误矩阵上报，
// 不静默重试到超时。重解析后通道若翻转就报错：本次缓存的键与出站身份是旧通道的，
// 把新通道的字节写进去会串味，下一次播放列表请求会按新通道重建。
func (c *Cutter) refreshRejected(ctx context.Context, ep store.Episode, channel string) (stream.Direct, error) {
	if err := c.invalidate(ctx, ep.ID); err != nil {
		// 作废失败不阻断重解析：重解析会覆盖同一行（也许这次就成功了）。
		c.logger.WarnContext(ctx, "episode stream invalidate failed",
			"episode_id", ep.ID, "channel", channel, "err", err)
	}
	fresh, err := c.resolve(ctx, ep)
	if err != nil {
		return stream.Direct{}, resolveFailure(ep.ID, err)
	}
	if got := channelOf(fresh); got != channel {
		return stream.Direct{}, fmt.Errorf("剧集 %d 的通道由 %s 变为 %s: %w",
			ep.ID, channel, got, httpx.ErrUpstream)
	}
	c.logger.InfoContext(ctx, "episode stream refreshed",
		"episode_id", ep.ID, "channel", channel)
	return fresh, nil
}

// touchStream 记下「这一集的直链刚被确认能用」。写失败只记 warn：它不影响本次播放，
// 只是排障信息（ADR-0016 的 last_ok_at）。
func (c *Cutter) touchStream(ctx context.Context, episodeID int64) {
	at := time.Now().UTC().Format(time.RFC3339)
	if err := c.st.TouchEpisodeStream(ctx, episodeID, at); err != nil {
		c.logger.WarnContext(ctx, "episode stream touch failed",
			"episode_id", episodeID, "err", err)
	}
}

// ensureChannel 让 segment 行与本次解析出的通道一致：没行才插入，通道翻了才更新。
//
// 每次取 playlist 都会现解析直链，所以这里不额外外呼。分片端点复用这一行：
// 行在就不解析直链 —— 「命中缓存零外呼」靠的就是它（626 裁决 3 的机制原样保留，
// 判据仍然只有 channelOf 一处）。
func (c *Cutter) ensureChannel(ctx context.Context, episodeID int64, channel string) error {
	seg, err := c.st.GetSegment(ctx, episodeID)
	switch {
	case err == nil && seg.Channel == channel:
		return nil
	case err == nil:
		// 通道翻转：覆盖。旧通道的片键不同，留在缓存里由 LRU 淘汰，永不命中。
	case !errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("读取分片身份 episode_id=%d: %w", episodeID, err)
	}
	if err := c.st.UpsertSegment(ctx, store.Segment{EpisodeID: episodeID, Channel: channel}); err != nil {
		return fmt.Errorf("写入分片身份 episode_id=%d: %w", episodeID, err)
	}
	return nil
}

// 子进程硬超时：源直链卡死时以超时结束，不挂住请求（DEV-STANDARDS 出站超时）。
const (
	timeoutProbe = 30 * time.Second
	// timeoutRemux 是整集 remux 的子进程硬超时。整集要读完 CDN 全程（10 分钟集 ≈ 40 MB），
	// 60 s 在弱网上会误杀；单集一片之后每次生成只做一次整集转封装（ADR-0011）。
	timeoutRemux = 300 * time.Second
)

// 子进程失败的**稳定类别**：客户端可见的错误文本里只出现这几个常量。
//
// 为什么不能带原文：ffprobe / ffmpeg 会把**完整输入 URL** 打进报错（实测
// `<url>?n80_tk=…: Server returned 403 Forbidden`），而 App 片源的直链带签名
// 查询串。错误文本会进 502 响应体（httpx.WriteError 直接用 err.Error()），
// 契约要求解出的 URL 查询串 MUST NOT 出现在日志与错误文本里（MKACG-626 /
// MKACG-639）。只回类别还顺带免疫「上游换了措辞」：换措辞最多让类别退化成
// `<op> failed`，不会把新措辞漏出去。
const (
	classProbeFailed   = "ffprobe failed"
	classRemuxFailed   = "ffmpeg failed"
	classDecryptFailed = "ffprobe decryption failed"
	classRejected      = "upstream rejected"
)

// failureClass 把子进程 stderr 折成稳定类别。上游拒绝（401 / 403 / 404 / Forbidden）
// 单列一类：它与「源本身坏了」的处置不同（换直链/换身份 vs 换源），排障时要能分开，
// 也是 ADR-0016「标记失效并重解析一次」的唯一触发条件。
func failureClass(fallback string, stderr []byte) string {
	rejected := bytes.Contains(stderr, []byte("401")) ||
		bytes.Contains(stderr, []byte("403")) ||
		bytes.Contains(stderr, []byte("404")) ||
		bytes.Contains(stderr, []byte("Forbidden")) ||
		bytes.Contains(stderr, []byte("Unauthorized")) ||
		bytes.Contains(stderr, []byte("Not Found"))
	if rejected {
		return classRejected
	}
	return fallback
}

// errRejected 是「上游明确拒绝」的哨兵：类别文本仍是 classRejected，但调用方可以用
// errors.Is 判定并触发一次「作废 + 重解析」（ADR-0016），不用比对错误字符串。
var errRejected = errors.New(classRejected)

// subprocessError 按稳定类别构造子进程失败错误：类别进文本，哨兵可判定。
func subprocessError(class string) error {
	if class == classRejected {
		return fmt.Errorf("%w: %w", errRejected, httpx.ErrUpstream)
	}
	return fmt.Errorf("%s: %w", class, httpx.ErrUpstream)
}

// stderrTail 取子进程 stderr 的尾部（**只给日志用，绝不进错误文本**）。
// cmd.Stderr 指向 buffer 时 stderr 在那里；未设置时 cmd.Output() 把它挂在
// *exec.ExitError 上，因此从那里取。只留尾部 200 字节。
func stderrTail(err error, buf []byte) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		buf = ee.Stderr
	}
	s := strings.TrimSpace(string(buf))
	if len(s) > 200 {
		s = s[len(s)-200:]
	}
	return s
}

// withheldDetail 是「有 stderr 但不适合进日志」的占位标记。
const withheldDetail = "<withheld>"

// loggableStderr 把 stderr 尾部折成可以进日志的一条备注。
//
// 判据是「出现地址/查询串特征就整条丢弃」，不是「像不像 URL」：上面那条报错里的
// 直链被 200 字节截断后可能只剩查询串中段（`6110…&n80=1&n80_tk=…`），开头已经
// 没有 `http://` 了。日志禁止记带签名的播放直链（docs/DEV-STANDARDS.md「日志」），
// 因此宁可整条不记，由 host + 类别 + uid/ep_no 定位。
func loggableStderr(tail string) string {
	if tail == "" {
		return ""
	}
	if strings.Contains(tail, "://") || strings.ContainsAny(tail, "?&=") {
		return withheldDetail
	}
	return tail
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

// probeArgs 拼 `ffprobe` 的完整 argv（纯函数，argv 顺序就是契约：
// `-decryption_key` 与 `-headers` 必须落在源地址之前，否则 ffmpeg 只把它们当输出
// 选项、静默忽略）。
//
// 只问时长：每集一片之后不再需要关键帧与编码签名（ADR-0011 删掉了那两样）。
func probeArgs(src stream.Direct, headers string) []string {
	args := []string{
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "json",
	}
	args = append(args, headerArgs(headers)...)
	args = append(args, decryptionArgs(src.Key)...)
	return append(args, src.URL)
}

// probeDuration 执行 ffprobe，取出源时长（秒）。
//
// 失败返回 httpx.ErrUpstream，**错误文本只有稳定类别**（`ffprobe failed` /
// `upstream rejected`）：stderr 原文（含完整直链与签名查询串）绝不回给客户端，
// 只按 loggableStderr 的判据进日志，定位靠 host + 类别 + 集标识。
//
// 密文源要单独判一次解密自检：key 错的时候 ffprobe 常常**以 0 退出**、只往 stderr
// 吐 `non-existing PPS …`，拿不到时长却也不算失败 —— 那种「成功」会把 0 写进
// episode.duration_sec。所以 key 非空时 stderr 必须为空（实测正确 key 下 0 字节），
// 非空即解密失败（MKACG-626 AC-8）。明文流不看 stderr：`-v error` 下源本身也会吐
// 无害告警，按它判失败会把好源打成 502。
func probeDuration(ctx context.Context, logger *slog.Logger, src stream.Direct, headers string) (float64, error) {
	dctx, cancel := context.WithTimeout(ctx, timeoutProbe)
	defer cancel()
	cmd := exec.CommandContext(dctx, "ffprobe", probeArgs(src, headers)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		class := failureClass(classProbeFailed, stderr.Bytes())
		logger.WarnContext(ctx, "ffprobe failed",
			"host", safeHost(src.URL), "class", class,
			"stderr", loggableStderr(stderrTail(err, stderr.Bytes())))
		return 0, subprocessError(class)
	}
	if len(src.Key) > 0 && stderr.Len() > 0 {
		logger.WarnContext(ctx, "ffprobe decryption self-check failed",
			"host", safeHost(src.URL),
			"stderr", loggableStderr(stderrTail(nil, stderr.Bytes())))
		return 0, subprocessError(classDecryptFailed)
	}
	var decoded struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		return 0, fmt.Errorf("解析 ffprobe 输出: %w（%v）", httpx.ErrUpstream, err)
	}
	seconds, _ := strconv.ParseFloat(decoded.Format.Duration, 64)
	return seconds, nil
}

// remuxArgs 拼整集转封装的完整 argv（纯函数）：`-decryption_key` 与 `-headers`
// 落在 `-i` 之前，才是输入选项。
//
// 一条路径、不切、无 `-ss/-t`/无 `-output_ts_offset`（ADR-0011 D4）：整集一次
// `-c copy` 写成 fragmented MP4，片头自带 `ftyp+moov` 所以每片自包含、可单独 ffprobe；
// 集边界的时间戳重置由 `#EXT-X-DISCONTINUITY` 表达，不再靠平移 PTS 兜。
func remuxArgs(src stream.Direct, headers string) []string {
	args := append([]string{"-v", "error"}, headerArgs(headers)...)
	args = append(args, decryptionArgs(src.Key)...)
	args = append(args,
		"-i", src.URL,
		"-c", "copy",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	)
	return args
}

// remux 执行 ffmpeg：整集 `-c copy` 转封装成 fMP4 写到 stdout，返回完整字节。
//
// 失败返回 httpx.ErrUpstream，**错误文本只有稳定类别**（`ffmpeg failed` /
// `upstream rejected`）—— ffmpeg 的 stderr 里带着完整直链与签名查询串，只按
// loggableStderr 的判据进日志（MKACG-639）。
//
// 字节全部收进内存再返回，调用方才能「先定状态码再写响应」：ffmpeg 中途失败时
// 客户端看到的是干净的 502，不是半截流。
//
// src.Key 非空时解密交给 ffmpeg 原生 `-decryption_key`（不自己解 CENC、不落整集
// 明文）：同一条 `-c copy` 路径，只是多一个输入选项。
func remux(ctx context.Context, logger *slog.Logger, src stream.Direct, headers string) ([]byte, error) {
	var buf bytes.Buffer
	if err := remuxTo(ctx, logger, src, headers, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// countingWriter 数已经写出的字节：失败重试前要确认「一个字节都还没写」，
// 否则第二遍会追加在第一遍半截内容后面。
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// remuxTo 是 remux 的流式形态：ffmpeg 的 stdout 直接写进 w，字节一到就交给调用方
// （MKACG-686：冷启动时首个分片落盘即可对外服务，不必等整集转封装完成）。
//
// 错误语义与 remux 完全一致：失败返回 httpx.ErrUpstream，**错误文本只有稳定类别**，
// ffmpeg 的 stderr 带签名直链，只按 loggableStderr 的判据进日志（MKACG-639）。
// 返回错误时 w 里可能已经有半截字节：调用方要么丢弃（缓存生成失败即删临时文件），
// 要么断开响应 —— 绝不能把它当成完整产物。
func remuxTo(ctx context.Context, logger *slog.Logger, src stream.Direct, headers string, w io.Writer) error {
	dctx, cancel := context.WithTimeout(ctx, timeoutRemux)
	defer cancel()
	cmd := exec.CommandContext(dctx, "ffmpeg", remuxArgs(src, headers)...)
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := stderrTail(err, stderr.Bytes())
		class := failureClass(classRemuxFailed, []byte(tail))
		logger.WarnContext(ctx, "ffmpeg remux failed",
			"host", safeHost(src.URL), "class", class,
			"stderr", loggableStderr(tail))
		return subprocessError(class)
	}
	return nil
}

// formatSeconds 把秒格式化成三位十进制（EXTINF 用；整数秒时长写成 `123.000`）。
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

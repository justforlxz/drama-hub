// Package hongguo 是播放源插件契约的第一个实现：红果短剧官网 SSR。
//
// 只做「上游字节 → 领域值」的翻译：不写库、不开事务、不 import store / vfs / web /
// 调度器（ADR-0006）。名字、配置键与错误语义的真值见 docs/PLUGIN-CONTRACT.md。
package hongguo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// Name 是插件在库里的稳定标识，直接进 drama.provider / episode.source。
//
// 取 "web" 而不是 "hongguo"：库里已有 provider='web' 的数据，改名要写数据迁移而
// 2.0 不需要（理由与被否决的备选见 docs/DATA-MODEL.md「provider 标识」）。
// 一经发布不得更改——改它等于改库里的历史数据。
const Name = "web"

// DefaultBaseURL 是官网站点根。配置 plugin.web.base_url 为空时用它。
const DefaultBaseURL = "https://hongguoduanju.com"

// sitemapIndexPath 是源侧目录索引，枚举（S5）与自检（Health）都以它为入口。
const sitemapIndexPath = "/sitemap/hongguoduanju/index.xml"

// 上游要的浏览器身份（MKACG-624）。
//
// 实测（2026-10-31）：无头请求与浏览器头请求在官网页面与 /video 直链上都是
// 200/206，源侧当下不按 UA/Referer 拒绝。仍然发浏览器身份，因为源侧风控是既定
// 事实——实测跑到约 1 万次 /player 后全量回 444（见 statusDropped），而 Go 默认
// 出站 UA（Go-http-client/1.1）是最显眼的非浏览器标识；参考实现的原话是「官网
// 不做鉴权，但要一个像浏览器的请求」。
//
// 这几个值是「浏览器长什么样」的事实，不会有人去调（调了就不再是浏览器），所以
// 是代码常量而不是 settings 键（AGENTS.md 硬约束 5 的反面：没有用户会为
// 「UA 长什么样」调参；可调的是「等多久」，那是 settings 的 cover_timeout_sec）。
const (
	// outboundUA 是发往上游的 User-Agent：桌面 Chrome/122。
	outboundUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"
	// outboundAccept 是浏览器导航请求的 Accept。
	outboundAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"
	// outboundAcceptLanguage 是中文浏览器的 Accept-Language。
	outboundAcceptLanguage = "zh-CN,zh;q=0.9"
)

// referer 返回发往上游的 Referer：站点根。
//
// 官网页面与 /video 直链是同一 origin（实测 14 部剧的 contentUrl 全在
// hongguoduanju.com/video/public/ 下，没有第三方视频 CDN），所以播放链路也用这一个，
// 而不是参考实现给移动端 CDN 的 novelquickapp.com。
func referer(baseURL string) string {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return strings.TrimRight(baseURL, "/") + "/"
}

// PlaybackHeaderLines 返回取 /video 直链时要带的请求头（`名称: 值`，顺序固定），
// 供 ffmpeg / ffprobe 的 -headers 使用：它们不走 http.Client，只能拿到这一串。
//
// 只含 UA 与 Referer：直链是媒体请求，页面那套 Accept / Accept-Language 不适用。
// 身份的真值仍在本包的常量里（一份身份两处用），由装配根注入 internal/segment。
func PlaybackHeaderLines(baseURL string) []string {
	return []string{"User-Agent: " + outboundUA, "Referer: " + referer(baseURL)}
}

const (
	// defaultTimeout 是 HTTPClient 为 nil 时的兜底出站超时。契约的配置默认值
	// plugin.web.request_timeout_sec = 30（docs/PLUGIN-CONTRACT.md「配置」），
	// 装配根按配置构造 client；这里是它的同一份默认值。
	defaultTimeout = 30 * time.Second
	// maxBodyBytes 是单次响应体读取上限：实测列表页 / 集页约 213 KB，`/category`
	// 一页约 281 KB，4 MiB 足够且能挡住上游异常放大 / 无限流。超限按 ErrSchema
	// （页面异常放大不是可重试故障）。
	maxBodyBytes = 4 << 20
	// maxSitemapBytes 是 sitemap 的读取上限。子 sitemap 不是「一页」：实测 29 份
	// 每份都是 5,242,7xx 字节（整份 43,556 个 loc），比页面大一个数量级。
	// 给它单独一个上限，而不是把页面闸放宽到 8 MiB —— 页面闸是对「上游异常放大」
	// 的判据，不该为了 sitemap 放弃。
	maxSitemapBytes = 8 << 20
	// statusDropped 是上游把连接直接丢掉时 nginx 返回的 444（无响应体、约 50 ms
	// 就回）。实测 2026-10-30 全量灌库跑到约 1 万次 /player 请求后，源侧开始对
	// /player 全量回 444（sitemap 与 /category 仍是 200），4 个 worker 在 17 分钟里
	// 打出了 98,190 次 444 —— 这是限流/风控，不是「这个作品不存在」，按
	// ErrRejected 处理让框架立刻中止本轮（docs/PLUGIN-CONTRACT.md 错误语义表
	// 「429、403、风控页」那一行），而不是继续以 90 req/s 打上游。
	statusDropped = 444
)

// Config 是构造期注入的不可变配置。
//
// 契约硬性要求 2：插件实例只允许保存构造期配置（名字、BaseURL、*http.Client、
// *slog.Logger），不保存游标、上次结果或缓存的直链。
//
// 唯一例外是匿名设备身份（MKACG-628）：它是 App 通道取址的前置，单条、失败也
// 缓存、续期 ≤ 1 次/分钟，只活在进程内存里（MKACG-626 契约第 3 条，
// MKACG-632 写进 docs/PLUGIN-CONTRACT.md）。
type Config struct {
	BaseURL           string       // 空 → DefaultBaseURL
	HTTPClient        *http.Client // nil → 30s 超时客户端
	Logger            *slog.Logger // nil → slog.Default()
	DeviceRegisterURL string       // 空 → defaultDeviceRegisterURL；测试注入假上游
	// AppVideoURL 是 App 通道取流端点。App 通道就两个 host：设备注册
	// （DeviceRegisterURL）与取流（本字段），两者都是构造期配置、默认值内置、
	// 测试可注入；settings 不新增键（MKACG-626 D2：上游换域名就发版）。
	AppVideoURL string // 空 → defaultAppVideoURL
}

// 编译期断言：红果插件实现契约的全部方法（少一个就是构建失败，不是运行时才发现）。
var _ plugin.Plugin = (*Plugin)(nil)

// Plugin 是红果插件。零值不可用，用 New 构造。
type Plugin struct {
	baseURL     string
	appVideoURL string
	client      *http.Client
	logger      *slog.Logger
	// device 是本插件唯一的进程内可变状态（匿名设备身份），规则见 device.go。
	device *deviceCache
}

// New 构造插件：空 BaseURL 用 DefaultBaseURL，空 client 用 30s 超时客户端，
// 空 logger 用默认 logger。
func New(cfg Config) *Plugin {
	p := &Plugin{baseURL: cfg.BaseURL, appVideoURL: cfg.AppVideoURL,
		client: cfg.HTTPClient, logger: cfg.Logger}
	if p.baseURL == "" {
		p.baseURL = DefaultBaseURL
	}
	if p.appVideoURL == "" {
		p.appVideoURL = defaultAppVideoURL
	}
	if p.client == nil {
		p.client = &http.Client{Timeout: defaultTimeout}
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	// 设备身份的注册点：构造期只装缓存，首次取用才发注册请求。
	p.device = newDeviceCache(cfg.DeviceRegisterURL, p.client, p.logger)
	return p
}

// deviceIdentity 取本进程的匿名设备身份（注册一次、失败也缓存、续期 ≤ 1 次/分钟）。
//
// 这是 App 通道取址（MKACG-630）的唯一入口；本包之外拿不到它，它也不落库、
// 不落盘、不进 settings、不进日志。
func (p *Plugin) deviceIdentity(ctx context.Context) (deviceIdentity, error) {
	return p.device.get(ctx)
}

// renewDeviceIdentity 在 App 接口拒了 rejected 这条身份后换一条新的，重试一次
// （626 契约）。续期 ≤ 1 次/分钟；静默期内再被拒就回 plugin.ErrRejected。
func (p *Plugin) renewDeviceIdentity(ctx context.Context, rejected deviceIdentity) (deviceIdentity, error) {
	return p.device.renew(ctx, rejected)
}

// Name 实现 plugin.Plugin。
func (p *Plugin) Name() string { return Name }

// Work 实现 plugin.Plugin：一个 GET /player/{id} 同时给出剧目元数据与全部集。
//
// 实测 seriesDetail.vid_list 就是全部集标识（下标即集号），因此不逐集抓。
// 集标题与时长源侧批量拿不到，留零值而不是编造（契约 Episode 注释）。
func (p *Plugin) Work(ctx context.Context, ref plugin.WorkRef) (plugin.WorkDetail, error) {
	detail, _, err := p.work(ctx, ref)
	return detail, err
}

// WorkWithEpisodeKeys 是框架侧（internal/sync）的可选能力接口：一次调用同时返回
// 详情与每集在源侧的集标识（红果的 vid），框架据此写 episode.vid。
//
// 契约的 Episode 只有集号 / 标题 / 时长，源侧集标识没有位置；为了让 2.0 迁移前的
// 入库字段（episode.vid）不丢，框架另行约定这个可选方法。它不在 plugin.Plugin
// 里，所以别的插件不实现也不影响契约。
func (p *Plugin) WorkWithEpisodeKeys(ctx context.Context, ref plugin.WorkRef) (plugin.WorkDetail, []string, error) {
	return p.work(ctx, ref)
}

// work 是 Work / WorkWithEpisodeKeys 的共同实现。
func (p *Plugin) work(ctx context.Context, ref plugin.WorkRef) (plugin.WorkDetail, []string, error) {
	detail, vids, _, err := p.workPage(ctx, ref)
	return detail, vids, err
}

// workPage 是 work 的完整形态：多返回一个「网页通道只给预览」（protectedPlayback）
// 的声明。只有 Resolve 关心它——Work 的返回值里没有这个字段，插件契约不变。
func (p *Plugin) workPage(ctx context.Context, ref plugin.WorkRef) (plugin.WorkDetail, []string, bool, error) {
	if ref.ID == "" {
		return plugin.WorkDetail{}, nil, false, fmt.Errorf("%w: WorkRef.ID 为空", plugin.ErrSchema)
	}
	body, err := p.get(ctx, "/player/"+ref.ID)
	if err != nil {
		return plugin.WorkDetail{}, nil, false, err
	}
	ser, err := parsePlayerPage(string(body), ref.ID)
	if err != nil {
		return plugin.WorkDetail{}, nil, false, err
	}
	episodes := make([]plugin.Episode, len(ser.vids))
	for i := range ser.vids {
		episodes[i] = plugin.Episode{No: i + 1}
	}
	detail := plugin.WorkDetail{
		Work: plugin.Work{
			Ref:         ref,
			Title:       ser.name,
			Year:        yearFromUnix(ser.firstVisibleTime),
			Plot:        ser.intro,
			CoverURL:    ser.cover,
			Genres:      ser.tags,
			EpisodeDecl: len(ser.vids),
		},
		Episodes: episodes,
	}
	return detail, ser.vids, ser.protected, nil
}

// Resolve 实现 plugin.Plugin：解析某一集的可播放直链。
//
// 取址顺序（MKACG-626 契约）：web(非预览) → app。
//   - 网页通道能给出真直链就用它（2 个请求：详情页取集表 + 集页取直链）；
//   - 页面声明 protectedPlayback==true 时，网页只有 10 秒 SEO 预览，一律不请求
//     集页、不把那个 URL 当片源（626 D1：不回落预览），直接走 App 通道；
//   - 网页通道回 ErrNoStream（集页 404 / VideoObject 无 contentUrl）同样落到 App；
//     其余错误照原样上抛——上游故障不该被 App 通道掩盖。
//
// epNo 超出集数 → ErrNotFound（这一集不存在，与通道无关）。
func (p *Plugin) Resolve(ctx context.Context, ref plugin.WorkRef, epNo int) (plugin.Stream, error) {
	_, vids, protected, err := p.workPage(ctx, ref)
	if err != nil {
		return plugin.Stream{}, err
	}
	if epNo < 1 || epNo > len(vids) {
		return plugin.Stream{}, fmt.Errorf("%w: %s 没有第 %d 集（共 %d 集）",
			plugin.ErrNotFound, ref.ID, epNo, len(vids))
	}
	vid := vids[epNo-1]
	if !protected {
		st, err := p.resolveWeb(ctx, ref.ID, vid)
		if err == nil {
			return st, nil
		}
		if !errors.Is(err, plugin.ErrNoStream) {
			return plugin.Stream{}, err
		}
	}
	return p.resolveApp(ctx, vid)
}

// resolveWeb 是网页通道取址：集页 JSON-LD 的明文 MP4 直链。
//
// 集页 404 或 VideoObject 无 contentUrl → ErrNoStream（契约错误语义表的两行：
// 这一集不可播不是作品不存在）。Key 显式留 nil：网页通道给的是明文。
func (p *Plugin) resolveWeb(ctx context.Context, sid, vid string) (plugin.Stream, error) {
	body, err := p.get(ctx, "/player/"+sid+"/"+vid)
	if err != nil {
		if errors.Is(err, plugin.ErrNotFound) {
			return plugin.Stream{}, fmt.Errorf("%w: 集页 %s/%s 返回 404", plugin.ErrNoStream, sid, vid)
		}
		return plugin.Stream{}, err
	}
	url, err := parseDirectURL(string(body), sid, vid)
	if err != nil {
		return plugin.Stream{}, err
	}
	return plugin.Stream{URL: url, Key: nil}, nil
}

// Health 实现 plugin.Plugin：取一次源侧目录索引，证明「能连上、能拿到已知对象」。
//
// 只用 200 + 非空判据；内容解析与分页属 Enumerate（S5）。它不参与刷新逻辑，
// 失败也不影响服务启动（契约 Health 注释）。
func (p *Plugin) Health(ctx context.Context) error {
	_, err := p.get(ctx, sitemapIndexPath)
	return err
}

// get 发一次 GET（相对 BaseURL 的路径）并读回响应体：页面级上限 maxBodyBytes。
//
// 错误映射与日志见 fetch。目录发现的 sitemap 走 getSitemap（上限更大），
// 因为它一份就是 5 MiB 的 XML，不是「一页」。
func (p *Plugin) get(ctx context.Context, path string) ([]byte, error) {
	return p.fetch(ctx, path, p.absolute(path), maxBodyBytes)
}

// getSitemap 读一份 sitemap（索引或子 sitemap）：上限 maxSitemapBytes。
//
// loc 用上游索引给的地址（可以是绝对 URL，跨 origin 也照读——loc 是源侧对文件
// 位置的声明）；相对地址按 BaseURL 补齐，测试用假站点时不必编造域名。
func (p *Plugin) getSitemap(ctx context.Context, loc string) ([]byte, error) {
	raw := p.absolute(loc)
	return p.fetch(ctx, pathOf(raw), raw, maxSitemapBytes)
}

// absolute 把上游给的地址规整成可请求的绝对 URL：已是绝对地址就原样返回，
// 否则拼在 BaseURL 之后。
func (p *Plugin) absolute(loc string) string {
	if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		return loc
	}
	return strings.TrimRight(p.baseURL, "/") + "/" + strings.TrimLeft(loc, "/")
}

// pathOf 取出 URL 的路径部分（无 query），只用于日志与错误文本：
// 错误里不得出现完整直链，而 sitemap 的 loc 是带主机名的绝对地址。
func pathOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		return u.Path
	}
	return raw
}

// setOutboundHeaders 给一次出站请求挂上浏览器身份（UA / Accept /
// Accept-Language / Referer）。
//
// 页面（/player、/category）、sitemap 与图片都走同一个 fetch，因此一套头覆盖全部
// 出站；Referer 取本站站点根，换成绝对 sitemap loc 也仍然是本站。
func (p *Plugin) setOutboundHeaders(req *http.Request) {
	req.Header.Set("User-Agent", outboundUA)
	req.Header.Set("Accept", outboundAccept)
	req.Header.Set("Accept-Language", outboundAcceptLanguage)
	req.Header.Set("Referer", referer(p.baseURL))
}

// fetch 发一次 GET 并读回响应体，把结果映射成契约哨兵：
//
//	429 / 403 / 444  → ErrRejected（可重试，需退避）
//	404              → ErrNotFound
//	其余非 2xx、传输错误、读体失败、空响应 → ErrUpstream
//	响应体超 limit                        → ErrSchema
//
// ctx 被取消 / 超时（用户点了取消）直接返回 ctx 的错误，不冒充源故障。
// slog 一条出站记录（源 / 对象 / 状态 / 字节数），不记直链内容。
func (p *Plugin) fetch(ctx context.Context, logPath, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: 构造请求 %s: %v", plugin.ErrUpstream, logPath, err)
	}
	// 出站身份：源侧要一个「像浏览器的请求」（常量与理由见包内 outbound* 注释）。
	p.setOutboundHeaders(req)
	resp, err := p.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		p.logger.WarnContext(ctx, "上游请求失败", "plugin", Name, "path", logPath, "upstream_status", 0, "bytes", 0)
		return nil, fmt.Errorf("%w: GET %s: %v", plugin.ErrUpstream, logPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	level := slog.LevelInfo
	if readErr != nil || resp.StatusCode != http.StatusOK {
		level = slog.LevelWarn
	}
	p.logger.Log(ctx, level, "上游请求",
		"plugin", Name, "path", logPath, "upstream_status", resp.StatusCode, "bytes", len(body))

	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: 读取 %s 响应体: %v", plugin.ErrUpstream, logPath, readErr)
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == statusDropped:
		return nil, fmt.Errorf("%w: GET %s 返回 %d", plugin.ErrRejected, logPath, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: GET %s 返回 404", plugin.ErrNotFound, logPath)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: GET %s 返回 %d", plugin.ErrUpstream, logPath, resp.StatusCode)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: GET %s 返回空响应体", plugin.ErrUpstream, logPath)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: GET %s 响应体超过 %d 字节", plugin.ErrSchema, logPath, limit)
	}
	return body, nil
}

// yearFromUnix 把上游的 unix 秒（字符串）换成 UTC 年份，尽力而为：缺失或非法
// 返回 0，不报错（该字段缺失不该让整次调用失败）。
func yearFromUnix(raw string) int {
	if raw == "" {
		return 0
	}
	var sec int64
	if _, err := fmt.Sscanf(raw, "%d", &sec); err != nil || sec <= 0 {
		return 0
	}
	return time.Unix(sec, 0).UTC().Year()
}

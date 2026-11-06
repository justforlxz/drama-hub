package hongguo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/plugin/hongguo/sign"
)

// App 通道取址（MKACG-630，设计见 MKACG-626）。
//
// 网页通道对受保护剧只给 10 秒 SEO 预览，完整片源在 App 接口里：
//
//	匿名设备身份 → POST multi_video_model → 选流 → spade_a 派生 16 字节内容密钥
//	→ key_seed 解出真实媒体地址（AES-128-CBC，不是每集都有）→ 域名白名单校验
//
// 这里只做「上游字节 → plugin.Stream」。解密不在插件里做：密钥随
// plugin.Stream.Key 交给播放面，由 ffmpeg 的 -decryption_key 原生解 CENC-AES-CTR
// （626 裁决）；插件不落整集、不解析 MP4、不加第三方依赖。
//
// 敏感面（626 契约）：spade_a、key_seed、内容密钥、解出的 URL 查询串一律不进日志
// 与错误文本。日志里只出现「已解析/失败 + 错误类别 + 通道 + 集标识」，域名只在校验
// 通过之后出现。
const (
	// defaultAppVideoURL 是 App 取流端点：host + 路径都是 App 侧协议事实。
	// 与设备注册端点一样作构造期配置（626 D2：不进 settings，上游换域名就发版），
	// 这样测试能把假上游整条装进来。
	defaultAppVideoURL = "https://api5-normal-sinfonlineb.fqnovel.com/novel/player/multi_video_model/v1/"

	// defaultAppVideoAltURL 是主域名被拒后的备用取流端点（参考实现同款备用域名）。
	defaultAppVideoAltURL = "https://api5-normal-hl.fqnovel.com/novel/player/multi_video_model/v1/"

	// defaultAppSingleVideoURL 是单集取流端点（sinfonlinea 节点），
	// 在 multi_video_model 主备节点均被拒时作为二级 App 通道自动兜底。
	defaultAppSingleVideoURL = "https://api5-normal-sinfonlinea.fqnovel.com/novel/player/video_model/v1/"

	// appRetryDelay 是换身份重试前的等待（参考实现的 300ms；只用于一次重试）。
	appRetryDelay = 300 * time.Millisecond

	// appUA 是取流请求的 User-Agent：App 的 Android 形态，与官网页的浏览器身份
	// （outboundUA）不是一套，两者别互相顶掉（626 §6 交叠提示）。
	appUA = "com.phoenix.read/71532 (Linux; U; Android 9; SM-N9860; Build/PQ3A.190705.10241111;tt-ok/3.12.13.20)"

	// appVersionName 是取流请求声明的 App 版本：查询串与签名（medusa 的设备指纹）
	// 都读它，两处必须是同一个值。
	appVersionName = "7.1.3.32"

	// appVideoIDKind 是 mixed_video_id_map 的键：这次请求的是「剧集 vid」这一类
	// 标识（App 侧协议事实，换一个上游就认不出这批 id）。
	appVideoIDKind = "1004"

	// maxAppResponseBytes 是取流响应的读取上限：正常响应几十 KB，2 MiB 足够且能
	// 挡住上游异常放大。超限按 ErrSchema（与页面闸同理：放大不是可重试故障）。
	maxAppResponseBytes = 2 << 20

	// appKeyLen 是内容密钥长度（AES-128），spade_a 里编码的是它的 32 个十六进制字符。
	appKeyLen    = 16
	appKeyHexLen = appKeyLen * 2

	// appURLHeader* 是 key_seed 密文的固定头：raw[0]=0xa8、raw[2]=0x01、raw[3]=0x00
	// （raw[1] 不参与判断）。头不符就是结构不符，不是「换个地址再试」。
	appURLHeader0 = 0xa8
	appURLHeader2 = 0x01
	appURLHeader3 = 0x00

	// appSpadeInitOdd / appSpadeInitEven 是 spade_a 派生里的两个初始字节（协议事实）。
	appSpadeInitOdd  = 0x55
	appSpadeInitEven = 0xf6
)

// appMediaDomains 是 App 片源的域名白名单（626 §3）：媒体只能来自这三家的子域。
// 它是协议事实而不是可调参数——白名单之外的一律拒绝，宁可不播也不出站。
var appMediaDomains = []string{"qznovelvod.com", "fqnovelvod.com", "douyinvod.com"}

// appBatchLimit 是单次 multi_video_model 请求携带的 vid 上限（与默认合并块 10 集对齐）。
const appBatchLimit = 10

// resolveApp 走 App 通道取某集完整片源。
//
// 上游把取流身份当一次性的：同一条身份第二次请求会被网关 200 + 空体拒绝
// （MKACG-676 实测）。所以每次取流先注册一条新身份；被拒时按参考实现再换一条
// 身份、改走备用域名、Connection: close 重试一次，仍失败就回原错误。
func (p *Plugin) resolveApp(ctx context.Context, vid string) (plugin.Stream, error) {
	res, err := p.resolveAppBatch(ctx, []string{vid})
	if err != nil {
		return plugin.Stream{}, err
	}
	st, ok := res[vid]
	if !ok {
		return plugin.Stream{}, fmt.Errorf("%w: App 取流响应缺集 %s 的载荷", plugin.ErrNoStream, vid)
	}
	return st, nil
}

// resolveAppMany 批量走 App 通道一次取多集完整片源（按 appBatchLimit 分批）。
//
// 相比逐集调 resolveApp：10 集合并块只需 1 次设备注册 + 1 次 multi_video_model 请求，
// 出站请求数减少 90%，规避连续注册 10 台设备触发的上游频控。
func (p *Plugin) resolveAppMany(ctx context.Context, vids []string) (map[string]plugin.Stream, error) {
	out := make(map[string]plugin.Stream, len(vids))
	for start := 0; start < len(vids); start += appBatchLimit {
		end := start + appBatchLimit
		if end > len(vids) {
			end = len(vids)
		}
		chunk := vids[start:end]
		batchMap, err := p.resolveAppBatch(ctx, chunk)
		if err != nil {
			return nil, err
		}
		for k, v := range batchMap {
			out[k] = v
		}
	}
	return out, nil
}

func (p *Plugin) resolveAppBatch(ctx context.Context, vids []string) (map[string]plugin.Stream, error) {
	if len(vids) == 0 {
		return nil, nil
	}
	firstVid := vids[0]
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		id, err := p.playbackIdentity(ctx)
		if err != nil {
			p.logAppFailure(ctx, firstVid, err)
			return nil, err
		}
		res, err := p.appPlaybackBatch(ctx, id, vids, attempt)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if attempt == 1 || !errors.Is(err, plugin.ErrRejected) {
			break
		}
		// 空体只说明「这条身份不被接受」：换身份 + 备用域名再试一次。
		p.logger.WarnContext(ctx, "App 通道取址被拒，换身份重试",
			"plugin", Name, "channel", "app", "vid", firstVid, "batch", len(vids), "reason", errorClass(err))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(appRetryDelay):
		}
	}
	if len(vids) == 1 && errors.Is(lastErr, plugin.ErrRejected) && p.appVideoURL == defaultAppVideoURL {
		if st, serr := p.resolveAppSingleV1(ctx, firstVid); serr == nil {
			return map[string]plugin.Stream{firstVid: st}, nil
		}
	}
	p.logAppFailure(ctx, firstVid, lastErr)
	return nil, lastErr
}

// resolveAppSingleV1 走 /novel/player/video_model/v1/ 单集端点兜底。
func (p *Plugin) resolveAppSingleV1(ctx context.Context, vid string) (plugin.Stream, error) {
	id, err := p.playbackIdentity(ctx)
	if err != nil {
		return plugin.Stream{}, err
	}
	body, err := json.Marshal(map[string]any{
		"video_id": vid,
		"biz_param": map[string]any{
			"video_id_type":             0,
			"need_all_video_definition": true,
			"video_platform":            3,
		},
	})
	if err != nil {
		return plugin.Stream{}, fmt.Errorf("%w: 构造单集取流请求体: %v", plugin.ErrSchema, err)
	}
	signed, err := p.sign.Playback(
		strings.TrimRight(defaultAppSingleVideoURL, "/"),
		appSignParams(),
		sign.Identity{
			DeviceID:    id.DeviceID,
			IID:         id.IID,
			CDID:        id.CDID,
			VersionName: appVersionName,
		},
		body, appUA,
	)
	if err != nil {
		return plugin.Stream{}, fmt.Errorf("%w: %v", plugin.ErrSchema, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, signed.URL, bytes.NewReader(body))
	if err != nil {
		return plugin.Stream{}, fmt.Errorf("%w: 构造单集取流请求", plugin.ErrUpstream)
	}
	for name, value := range signed.Headers {
		req.Header.Set(name, value)
	}
	req.Close = true
	req.Header.Set("Connection", "close")
	logPath := req.URL.Path

	resp, err := p.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return plugin.Stream{}, ctxErr
		}
		return plugin.Stream{}, fmt.Errorf("%w: POST %s 网络失败", plugin.ErrUpstream, logPath)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAppResponseBytes+1))
	if readErr != nil || resp.StatusCode != http.StatusOK || len(payload) == 0 || int64(len(payload)) > maxAppResponseBytes {
		return plugin.Stream{}, fmt.Errorf("%w: POST %s 返回 status=%d bytes=%d", plugin.ErrRejected, logPath, resp.StatusCode, len(payload))
	}
	var env struct {
		Code      json.RawMessage `json:"Code"`
		CodeLower json.RawMessage `json:"code"`
		Data      struct {
			VideoModel json.RawMessage `json:"video_model"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return plugin.Stream{}, fmt.Errorf("%w: 单集取流响应不是 JSON", plugin.ErrSchema)
	}
	code := rawText(env.Code)
	if code == "" {
		code = rawText(env.CodeLower)
	}
	if code != "" && code != "0" {
		return plugin.Stream{}, fmt.Errorf("%w: 单集取流响应 code=%s", plugin.ErrRejected, code)
	}
	model, err := parseAppModel(env.Data.VideoModel)
	if err != nil {
		return plugin.Stream{}, err
	}
	st, err := appStreamFromModel(model)
	if err != nil {
		return plugin.Stream{}, err
	}
	host := ""
	if u, uerr := url.Parse(st.URL); uerr == nil {
		host = u.Hostname()
	}
	p.logger.InfoContext(ctx, "App 单集备用通道取址成功",
		"plugin", Name, "channel", "app_v1", "vid", vid, "host", host, "decrypt", st.Key != nil)
	return st, nil
}

// playbackIdentity 取一条本次取流专用身份。注册失败时退回缓存身份
// （缓存同时承担「注册不可用时的 60 秒闸」，避免每集都打一次注册）。
func (p *Plugin) playbackIdentity(ctx context.Context) (deviceIdentity, error) {
	id, err := p.device.fresh(ctx)
	if err == nil {
		return id, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return deviceIdentity{}, ctxErr
	}
	p.logger.WarnContext(ctx, "取流身份注册失败，改用缓存身份",
		"plugin", Name, "reason", errorClass(err))
	return p.deviceIdentity(ctx)
}

// logAppFailure 记一条失败日志：只有通道、集标识与错误类别，没有密钥/地址/查询串。
func (p *Plugin) logAppFailure(ctx context.Context, vid string, err error) {
	p.logger.WarnContext(ctx, "App 通道取址失败",
		"plugin", Name, "channel", "app", "vid", vid, "reason", errorClass(err))
}

// errorClass 把哨兵折成日志里的稳定标签（错误文本可能带上游细节，不进日志）。
func errorClass(err error) string {
	for _, c := range []struct {
		sentinel error
		name     string
	}{
		{plugin.ErrRejected, "ErrRejected"},
		{plugin.ErrNoStream, "ErrNoStream"},
		{plugin.ErrSchema, "ErrSchema"},
		{plugin.ErrNotFound, "ErrNotFound"},
		{plugin.ErrLoginRequired, "ErrLoginRequired"},
		{plugin.ErrUnsupported, "ErrUnsupported"},
		{plugin.ErrUpstream, "ErrUpstream"},
	} {
		if errors.Is(err, c.sentinel) {
			return c.name
		}
	}
	return "unknown"
}

// appPlaybackBatch 发一次批量取流请求（支持 1..N 个 vid）并把结果翻成 map[vid]plugin.Stream。
func (p *Plugin) appPlaybackBatch(ctx context.Context, id deviceIdentity, vids []string, attempt int) (map[string]plugin.Stream, error) {
	body, err := json.Marshal(appRequestBody{
		BizParam: appBizParam{
			DeviceLevel:            3,
			NeedAllVideoDefinition: true,
			VideoPlatform:          1024,
		},
		MixedVideoIDMap: map[string][]string{appVideoIDKind: vids},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: 构造取流请求体: %v", plugin.ErrSchema, err)
	}
	// 签名与发送共用同一份 body 字节：签名覆盖 query + body，签名后重新 marshal
	// 会让上游验签失败（MKACG-676 的硬约束），所以这里只 marshal 这一次。
	baseURL := p.appVideoURL
	if attempt > 0 {
		baseURL = p.appVideoAltURL
	}
	signed, err := p.sign.Playback(
		strings.TrimRight(baseURL, "/"),
		appSignParams(),
		sign.Identity{
			DeviceID:    id.DeviceID,
			IID:         id.IID,
			CDID:        id.CDID,
			VersionName: appVersionName,
		},
		body, appUA,
	)
	if err != nil {
		// 签名生成本身失败（128 次 ticket 都没落进分支 0）：显式报结构错误，
		// 不得静默回退到无签名请求（那只会拿回 200+空体，把根因藏起来）。
		return nil, fmt.Errorf("%w: %v", plugin.ErrSchema, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, signed.URL, bytes.NewReader(body))
	if err != nil {
		// 不带上游错误的原文：*url.Error 会把带设备身份的完整 URL 打进错误文本，
		// 而错误文本会进日志与 502 响应体（626：签名参数 MUST NOT 出现）。
		return nil, fmt.Errorf("%w: 构造取流请求", plugin.ErrUpstream)
	}
	for name, value := range signed.Headers {
		req.Header.Set(name, value)
	}
	if attempt > 0 {
		// 备用路由按参考实现的写法强制换连接，避免复用被判重的那条。
		req.Close = true
		req.Header.Set("Connection", "close")
	}
	// 日志只记路径：查询串里是设备身份与签名参数，不是排障需要的。
	logPath := req.URL.Path

	resp, err := p.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		p.logger.WarnContext(ctx, "App 取流请求失败",
			"plugin", Name, "channel", "app", "path", logPath, "upstream_status", 0, "bytes", 0)
		// 同上：传输错误的原文含完整 URL（查询串里是设备身份），只报类别。
		return nil, fmt.Errorf("%w: POST %s 网络失败", plugin.ErrUpstream, logPath)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAppResponseBytes+1))
	// 200 + 空体与 200 + 非 JSON 是最需要排障的两格：线上可能把日志级别调到 warn，
	// 这两格必须在过滤之后仍留下现场，不能只记在 Info（MKACG-673）。
	level := slog.LevelInfo
	switch {
	case readErr != nil,
		resp.StatusCode != http.StatusOK,
		len(payload) == 0,
		int64(len(payload)) > maxAppResponseBytes,
		!json.Valid(payload):
		level = slog.LevelWarn
	}
	// content_type 是区分「真没返回」与「返回了别的东西」的唯一线索，不含内容本身。
	contentType := resp.Header.Get("Content-Type")
	// logid 是上游给的排障句柄（不含请求内容），失败时最需要它。
	logID := resp.Header.Get("X-Tt-Logid")
	if logID == "" {
		logID = resp.Header.Get("X-Tt-LogID")
	}
	p.logger.Log(ctx, level, "App 取流",
		"plugin", Name, "channel", "app", "path", logPath,
		"upstream_status", resp.StatusCode, "bytes", len(payload),
		"batch", len(vids), "content_type", contentType, "logid", logID)

	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: 读取 %s 响应体: %v", plugin.ErrUpstream, logPath, readErr)
	}
	if int64(len(payload)) > maxAppResponseBytes {
		return nil, fmt.Errorf("%w: POST %s 响应体超过 %d 字节",
			plugin.ErrSchema, logPath, maxAppResponseBytes)
	}
	// 判定顺序（MKACG-635）：状态码先判，响应体后判。空体不是「身份被挡」的证据——
	// 404 + 空体正是「这集没有片源」的形态，先判空体会把它误归 ErrRejected，于是多换
	// 一轮身份、多打一次上游。
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == statusDropped:
		return nil, fmt.Errorf("%w: POST %s 返回 %d", plugin.ErrRejected, logPath, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: POST %s 返回 404", plugin.ErrNoStream, logPath)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: POST %s 返回 %d", plugin.ErrUpstream, logPath, resp.StatusCode)
	}
	// 200 + 空体（MKACG-673）：这个 agw 拒绝请求时回 200 但不写响应体，不是契约结构
	// 变了。归 ErrRejected 才能走 resolveApp 的换身份重试；按结构不符归类会把本来
	// 可自愈的故障变成不可恢复，错误文案也误导。
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: POST %s 返回 200 但响应体为空", plugin.ErrRejected, logPath)
	}

	reply := appReply{
		status: resp.StatusCode, contentType: contentType, bytes: len(payload),
	}
	out := make(map[string]plugin.Stream, len(vids))
	for _, vid := range vids {
		st, err := appStream(payload, vid, reply)
		if err != nil {
			return nil, err
		}
		host := ""
		if u, uerr := url.Parse(st.URL); uerr == nil {
			host = u.Hostname()
		}
		// 域名在校验通过之后才落日志；密钥只记「有没有」。
		p.logger.InfoContext(ctx, "App 通道取址成功",
			"plugin", Name, "channel", "app", "vid", vid, "host", host, "decrypt", st.Key != nil)
		out[vid] = st
	}
	return out, nil
}

// appSignParams 把设备参数摊成有序切片（签名对 query 字节敏感，顺序必须稳定，
// 所以按 key 排序一次而不是用会被并发改写的 map）。
func appSignParams() []sign.KV {
	params := appDeviceParams()
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]sign.KV, 0, len(keys))
	for _, k := range keys {
		out = append(out, sign.KV{Key: k, Value: params.Get(k)})
	}
	return out
}

// appDeviceParams 是取流请求要声明的设备形态（App 7.1.3.32 / Android 手机那套）。
//
// 与注册信封里的 deviceProfile 是同一件事的两种写法：注册用 JSON 信封声明设备，
// 取流用 query 声明同一台设备。少一个字段或版本号对不上，上游的安全校验就可能
// 直接拒（626 风险 2）。所以这些是协议事实，不是可调参数，也不进 settings。
func appDeviceParams() url.Values {
	return url.Values{
		"ac":                      {"wifi"},
		"aid":                     {"8662"},
		"app_name":                {"novelread"},
		"channel":                 {"update_64"},
		"compliance_status":       {"0"},
		"device_brand":            {"Redmi"},
		"device_platform":         {"android"},
		"device_type":             {"25053RT47C"},
		"dpi":                     {"520"},
		"dragon_device_type":      {"phone"},
		"host_abi":                {"arm64-v8a"},
		"is_android_pad_screen":   {"0"},
		"language":                {"zh"},
		"manifest_version_code":   {"71332"},
		"need_personal_recommend": {"1"},
		"os":                      {"android"},
		"os_api":                  {"36"},
		"os_version":              {"16"},
		"player_so_load":          {"1"},
		"pv_player":               {"71332"},
		"resolution":              {"1280*2772"},
		"ssmix":                   {"a"},
		"update_version_code":     {"71332"},
		"version_code":            {"71332"},
		"version_name":            {appVersionName},
	}
}

// appReply 是一次取流响应的元信息，只用来给「不是 JSON」补事实。
// 不含响应体内容，也不含查询串（里面有设备身份）。
type appReply struct {
	status      int
	contentType string
	bytes       int
}

// appStream 把取流响应的原始字节翻成 plugin.Stream。
func appStream(payload []byte, vid string, reply appReply) (plugin.Stream, error) {
	var env appEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		// 非空但不是 JSON：确实不是契约结构。带上是哪次响应，排障才有依据。
		return plugin.Stream{}, fmt.Errorf("%w: App 取流响应不是 JSON（status=%d content_type=%q bytes=%d）",
			plugin.ErrSchema, reply.status, reply.contentType, reply.bytes)
	}
	code := rawText(env.Code)
	if code == "" {
		code = rawText(env.CodeLower)
	}
	if code != "" && code != "0" {
		// 业务拒绝（Code != 0）与风控同一条动作：退避到下一周期，不回落预览。
		return plugin.Stream{}, fmt.Errorf("%w: App 取流响应 code=%s", plugin.ErrRejected, code)
	}
	rawEntry, ok := env.Data[vid]
	if !ok {
		return plugin.Stream{}, fmt.Errorf("%w: App 取流响应缺集 %s 的载荷", plugin.ErrNoStream, vid)
	}
	var entry appEntry
	if err := json.Unmarshal(rawEntry, &entry); err != nil {
		return plugin.Stream{}, fmt.Errorf("%w: App 取流响应结构不符: %v", plugin.ErrSchema, err)
	}
	model, err := parseAppModel(entry.VideoModel)
	if err != nil {
		return plugin.Stream{}, err
	}
	return appStreamFromModel(model)
}

// parseAppModel 解 video_model：内联对象，或它的 JSON 文本。缺失 → ErrNoStream
// （这一集上游没给片源，是正常状态，不是结构错误）。
func parseAppModel(raw json.RawMessage) (appModel, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return appModel{}, fmt.Errorf("%w: App 未返回本集片源模型", plugin.ErrNoStream)
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return appModel{}, fmt.Errorf("%w: video_model 文本解不开: %v", plugin.ErrSchema, err)
		}
		trimmed = []byte(text)
	}
	var model appModel
	if err := json.Unmarshal(trimmed, &model); err != nil {
		return appModel{}, fmt.Errorf("%w: video_model 结构不符: %v", plugin.ErrSchema, err)
	}
	return model, nil
}

// appStreamFromModel 从片源模型里选出可播的一行：解地址、派生密钥、过白名单。
//
// 选流口径（626）：h264 优先，没有 h264 时取 hevc（Emby 对 -c copy 出的
// MPEG-TS HEVC 兼容性未验证，但比「报错不播」更接近目标）；同编码内取短边最大、
// 再取码率最高的那一行（626：Resolve 取源侧最优，不做清晰度选择）。
func appStreamFromModel(model appModel) (plugin.Stream, error) {
	var seed []byte
	if model.KeySeed != "" {
		s, err := decodeBase64Loose(model.KeySeed)
		if err != nil {
			return plugin.Stream{}, fmt.Errorf("%w: key_seed 不是合法 base64", plugin.ErrSchema)
		}
		seed = s
	}
	rows := appRankedVideos(model.VideoList)
	if len(rows) == 0 {
		return plugin.Stream{}, fmt.Errorf("%w: App 未给 h264/hevc 片源", plugin.ErrNoStream)
	}
	var lastErr error
	for _, row := range rows {
		key, err := appContentKey(row)
		if err != nil {
			lastErr = err
			continue
		}
		for _, candidate := range row.candidates() {
			media := cleanAppURL(candidate)
			if seed != nil {
				media, err = decryptAppURL(media, seed)
				if err != nil {
					lastErr = err
					continue
				}
			}
			checked, err := checkMediaURL(media)
			if err != nil {
				lastErr = err
				continue
			}
			return plugin.Stream{URL: checked, Key: key}, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: App 片源地址不可用", plugin.ErrSchema)
	}
	return plugin.Stream{}, lastErr
}

// appRankedVideos 过滤出可播编码的行并按选流口径排序。
func appRankedVideos(videos appVideoList) []appVideo {
	type ranked struct {
		video   appVideo
		rank    int
		pixels  int
		bitrate int
	}
	rows := make([]ranked, 0, len(videos))
	for _, v := range videos {
		rank := appCodecRank(v.codec())
		if rank < 0 || len(v.candidates()) == 0 {
			continue
		}
		pixels, bitrate := v.size()
		rows = append(rows, ranked{video: v, rank: rank, pixels: pixels, bitrate: bitrate})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.pixels != b.pixels {
			return a.pixels > b.pixels
		}
		return a.bitrate > b.bitrate
	})
	out := make([]appVideo, len(rows))
	for i := range rows {
		out[i] = rows[i].video
	}
	return out
}

// appContentKey 取本行的内容密钥：有 spade_a 就派生，声明明文才是 nil。
//
// 「没有 spade_a 又不声明明文」按 ErrSchema：把密文当明文交给 ffmpeg 会产出
// 不可解码的分片并被缓存（626 AC-8 的实测失败模式）。
func appContentKey(v appVideo) ([]byte, error) {
	if v.EncryptInfo.SpadeA == "" {
		if appEncryptFlag(v.EncryptInfo.Encrypt) {
			return nil, fmt.Errorf("%w: 片源声明加密但缺 spade_a", plugin.ErrSchema)
		}
		return nil, nil
	}
	return deriveAppContentKey(v.EncryptInfo.SpadeA)
}

// appEncryptFlag 判 encrypt 声明：只有显式 false / 0 / "false" / "0" 才是明文。
func appEncryptFlag(raw json.RawMessage) bool {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(string(raw)), `"`)) {
	case "false", "0":
		return false
	}
	return true
}

// checkMediaURL 校验媒体地址：https、无 userinfo、443、白名单域名（含子域）。
//
// 错误文本只说不符的类别，不带地址也不带域名——没通过白名单的域名不该进日志。
func checkMediaURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%w: App 片源地址无法解析", plugin.ErrSchema)
	}
	switch {
	case u.Scheme != "https":
		return "", fmt.Errorf("%w: App 片源地址不是 https", plugin.ErrSchema)
	case u.User != nil:
		return "", fmt.Errorf("%w: App 片源地址带 userinfo", plugin.ErrSchema)
	case u.Port() != "" && u.Port() != "443":
		return "", fmt.Errorf("%w: App 片源地址端口不是 443", plugin.ErrSchema)
	}
	host := strings.ToLower(u.Hostname())
	for _, domain := range appMediaDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return u.String(), nil
		}
	}
	return "", fmt.Errorf("%w: App 片源地址域名不在白名单", plugin.ErrSchema)
}

// cleanAppURL 还原上游对地址做的转义（JSON 双重转义与 HTML 实体）。
//
// 刻意不做 query 归一化：地址带签名，重排或重编码参数会签名失效（与网页通道的
// normalizeURL 不同，那条是明文直链、可以整 query）。
func cleanAppURL(raw string) string {
	s := strings.TrimSpace(raw)
	for _, repl := range [][2]string{
		{`\/`, "/"},
		{`\u002F`, "/"},
		{`\u0026`, "&"},
		{"&amp;", "&"},
	} {
		s = strings.ReplaceAll(s, repl[0], repl[1])
	}
	return s
}

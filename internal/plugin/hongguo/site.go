package hongguo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// 站点播放接口（MKACG-676）。
//
// App 通道已被字节网关静默拒绝（200 + 空体，缺 X-Argus 一类签名），站点自己
// 另有一个可用端点，本文件实现它。
const (
	// sitePlayInfoPath 是站点播放接口。POST 才能用：www.hongguoduanju.com 对
	// POST 返回 301，所以 BaseURL 必须是不带 www 的裸域。
	sitePlayInfoPath = "/api/video/protected/play-info"
	// siteDefinition 是请求的清晰度。实测只认 720p（不传时服务端也默认 720p）；
	// 其余取值的行为未验证，因此写死而不是做成 settings 键。
	siteDefinition = "720p"
	// maxSiteResponseBytes 是播放接口响应体上限：实测成功响应 630 字节、拒绝响应
	// 42 字节，1 MiB 足够容纳任何合法答复。
	maxSiteResponseBytes = 1 << 20
	// siteErrProtectedUnavailable 是站点对「未登录 / 无该集权限」的答复文：出现在
	// 403 的响应体里（本剧 77 集里 67 集如此）。它是登录态失效的唯一判定依据，
	// 必须映射到 plugin.ErrLoginRequired，不能混进「上游失败」。
	siteErrProtectedUnavailable = "protected_playback_unavailable"
)

// sitePlayInfoRequest 是播放接口的请求体。definition 只认 720p。
type sitePlayInfoRequest struct {
	SeriesID   string `json:"series_id"`
	ItemID     string `json:"item_id"`
	Definition string `json:"definition"`
}

// sitePlayInfoResponse 是播放接口的响应体。
//
// 两条分支二选一：
//   - legacy_play_info：明文 CDN 直链（支持 Range，ffmpeg 可直接吃）；
//   - play_info：DASH-CENC 加密流，只有 kid 与 player_access_key，
//     没有 16 字节 AES key——本实现拿不到可用的 key（见 parseSitePlayInfo）。
type sitePlayInfoResponse struct {
	LegacyPlayInfo *siteLegacyPlayInfo `json:"legacy_play_info"`
	PlayInfo       *sitePlayInfoDash   `json:"play_info"`
	BaseResp       siteBaseResp        `json:"base_resp"`
	// Error 是失败时的答复文（如 protected_playback_unavailable）。
	Error string `json:"error"`
}

type siteLegacyPlayInfo struct {
	MainURL  string  `json:"main_url"`
	Width    string  `json:"width"`
	Height   string  `json:"height"`
	Duration float64 `json:"duration"`
}

type sitePlayInfoDash struct {
	Kid             string `json:"kid"`
	PlayerAccessKey string `json:"player_access_key"`
}

type siteBaseResp struct {
	StatusCode    int    `json:"StatusCode"`
	StatusMessage string `json:"StatusMessage"`
}

// resolveSite 是站点播放接口通道：一次 POST 换一条明文直链。
//
// 成功只认 legacy_play_info.main_url；play_info（DASH-CENC）在拿不到 16 字节
// key 的情况下回 ErrUnsupported，绝不静默成 502。
func (p *Plugin) resolveSite(ctx context.Context, sid, vid string, withCookie bool) (plugin.Stream, error) {
	body, err := p.postJSON(ctx, sitePlayInfoPath, sitePlayInfoRequest{
		SeriesID: sid, ItemID: vid, Definition: siteDefinition,
	}, withCookie)
	if err != nil {
		return plugin.Stream{}, err
	}
	return parseSitePlayInfo(body)
}

// parseSitePlayInfo 把播放接口的响应体折成契约值（纯函数，便于单测）。
//
// 判定顺序：答复文 → 业务码 → 明文直链 → 加密流。答复文先判，因为站点对
// 「未登录」既可能回 403 也可能回 200 + error（MKACG-635 的同一条原则：
// 先看已经确定的语义，再看其余）。
func parseSitePlayInfo(body []byte) (plugin.Stream, error) {
	var out sitePlayInfoResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return plugin.Stream{}, fmt.Errorf("%w: 站点播放接口返回的不是 JSON: %v", plugin.ErrSchema, err)
	}
	if out.Error != "" {
		if out.Error == siteErrProtectedUnavailable {
			return plugin.Stream{}, fmt.Errorf("%w: 站点答复 %s（未配置登录态，或该集不在当前身份权限内）",
				plugin.ErrLoginRequired, siteErrProtectedUnavailable)
		}
		return plugin.Stream{}, fmt.Errorf("%w: 站点播放接口返回 %s", plugin.ErrUpstream, out.Error)
	}
	if out.BaseResp.StatusCode != 0 {
		return plugin.Stream{}, fmt.Errorf("%w: 站点播放接口 code=%d %s",
			plugin.ErrUpstream, out.BaseResp.StatusCode, out.BaseResp.StatusMessage)
	}
	if li := out.LegacyPlayInfo; li != nil && li.MainURL != "" {
		// 签名直链不能重排 query，只做转义还原（与 App 通道同一个清洗函数）。
		raw := cleanAppURL(li.MainURL)
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return plugin.Stream{}, fmt.Errorf("%w: 站点片源地址不是 https 绝对地址", plugin.ErrSchema)
		}
		return plugin.Stream{
			URL:         u.String(),
			Key:         nil,
			DurationSec: int(li.Duration + 0.5),
		}, nil
	}
	if out.PlayInfo != nil {
		// DASH-CENC：响应里只有 kid（密钥标识）与 player_access_key（取密钥的
		// 凭据），没有 16 字节 AES-128 key，key 要靠一套 DRM 授权流程换——本实现
		// 不做（MKACG-676 边界：不逆向签名、不新增解密流程）。明确报出而不是 502。
		return plugin.Stream{}, fmt.Errorf("%w: 该集仅提供加密流（DASH-CENC），本服务拿不到解密 key", plugin.ErrUnsupported)
	}
	return plugin.Stream{}, fmt.Errorf("%w: 站点播放接口既无明文片源也无加密流描述", plugin.ErrSchema)
}

// siteErrorOf 尽力取出响应体里的答复文，只用于 403 的判定；解析失败返回空。
func siteErrorOf(body []byte) string {
	var v struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.Error
}

// postJSON 发一次 POST（JSON 请求体）并读回响应体。
//
// 与 fetch 的差别只有三处：方法、请求体、以及 403 + protected_playback_unavailable
// 归 ErrLoginRequired（fetch 对 403 一律 ErrRejected）。日志同样一条出站记录，
// 不记 Cookie 内容、不记直链。
func (p *Plugin) postJSON(ctx context.Context, logPath string, payload any, withCookie bool) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: 编码 %s 请求体: %v", plugin.ErrSchema, logPath, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.absolute(logPath), bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: 构造请求 %s: %v", plugin.ErrUpstream, logPath, err)
	}
	p.setOutboundHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	// 登录态每次现读：settings 改完下一次取流就是新凭据（热改即生效）。
	cookie := ""
	if withCookie {
		cookie = p.cookie(ctx)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		p.logger.WarnContext(ctx, "站点播放接口请求失败",
			"plugin", Name, "channel", "site", "path", logPath, "upstream_status", 0, "bytes", 0)
		return nil, fmt.Errorf("%w: POST %s: %v", plugin.ErrUpstream, logPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSiteResponseBytes+1))
	level := slog.LevelInfo
	if readErr != nil || resp.StatusCode != http.StatusOK {
		level = slog.LevelWarn
	}
	// login 只记「带没带登录态」，不记内容。
	p.logger.Log(ctx, level, "站点播放接口",
		"plugin", Name, "channel", "site", "path", logPath,
		"upstream_status", resp.StatusCode, "bytes", len(body), "login", cookie != "")

	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: 读取 %s 响应体: %v", plugin.ErrUpstream, logPath, readErr)
	}
	if int64(len(body)) > maxSiteResponseBytes {
		return nil, fmt.Errorf("%w: POST %s 响应体超过 %d 字节", plugin.ErrSchema, logPath, maxSiteResponseBytes)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusForbidden:
		// 403 在这里是「这个身份不能看这一集」，不是限流：答复文对得上就归登录态。
		if siteErrorOf(body) == siteErrProtectedUnavailable {
			return nil, fmt.Errorf("%w: 站点答复 %s（未配置登录态或已失效，或该集无权限）",
				plugin.ErrLoginRequired, siteErrProtectedUnavailable)
		}
		return nil, fmt.Errorf("%w: POST %s 返回 403", plugin.ErrRejected, logPath)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == statusDropped:
		return nil, fmt.Errorf("%w: POST %s 返回 %d", plugin.ErrRejected, logPath, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: POST %s 返回 404", plugin.ErrNotFound, logPath)
	default:
		return nil, fmt.Errorf("%w: POST %s 返回 %d", plugin.ErrUpstream, logPath, resp.StatusCode)
	}
	if len(body) == 0 {
		// 这是个 JSON 接口：200 + 空体不是「拒绝」，是我们看不懂，归 ErrSchema。
		return nil, fmt.Errorf("%w: POST %s 返回空响应体", plugin.ErrSchema, logPath)
	}
	return body, nil
}

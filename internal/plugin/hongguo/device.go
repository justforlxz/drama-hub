package hongguo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// 匿名设备身份（MKACG-628，设计见 MKACG-626 §6 S15.2a）。
//
// App 通道取址要一整套设备参数，其中 device_id / install_id 由匿名注册端点发。
// 注册不需要账号、不需要 cookie：POST 一个加密信封，回两个数字标识。
//
// 本文件只管「拿到身份」：注册信封、单条缓存、失败也缓存、续期 ≤ 1 次/分钟。
// 身份是本插件唯一允许的进程内可变状态，**不落库、不落盘、不进 settings、不进
// 日志**（626 契约第 3 条）。取址、派生与白名单归 MKACG-630。

const (
	// defaultDeviceRegisterURL 是匿名设备注册端点。查询串是 App 侧的协议事实
	// （tt_data / cronet / ttnet 版本），与站点无关；测试注入 httptest 地址。
	defaultDeviceRegisterURL = "https://log5-applog-lf.fqnovel.com/service/2/device_register/" +
		"?tt_data=a&cronet_version=8d40f833_2026-03-03&ttnet_version=4.2.243.31-douyin&use_store_region_cookie=1"
	// deviceAid 是红果在头条系里的产品号（信封 header 与 x-ss-dp 头都用它）。
	deviceAid = 8662
	// deviceUA 是 App 通道的 User-Agent：Android 形态，与官网页的浏览器身份
	// （outboundUA）不是一套，两者别互相顶掉（626 §6 交叠提示）。
	deviceUA = "com.phoenix.read/72232 (Linux; U; Android 16; zh_CN; 25053RT47C; Build/BP2A.250605.031.A3)"
	// deviceRetryInterval 是注册失败后的静默期：期间一律回放同一条失败，不再发
	// 请求（626 契约的「续期 ≤ 1 次/分钟」）。
	deviceRetryInterval = time.Minute
	// maxDeviceResponseBytes 是注册响应的读取上限：正常响应只有几百字节，64 KiB
	// 足够且挡住上游异常放大。
	maxDeviceResponseBytes = 64 << 10
)

// deviceIdentity 是一次成功注册拿到的身份。它只在进程内存里活着。
type deviceIdentity struct {
	DeviceID string // 注册响应 device_id（数字文本）
	IID      string // 注册响应 install_id（数字文本）
	CDID     string // 本进程生成的设备标识，同时写进信封 header
}

// deviceCache 是插件唯一的进程内可变状态：一条身份，成功与失败都只存这一条。
//
// 语义（MKACG-628 验收）：同一进程内反复取用只注册一次；并发首次取用也只注册
// 一次（single-flight，不是两个开关）；失败同样落缓存，且 60 秒内不再重试；
// 换身份（续期）同样受这条 60 秒闸约束，即「续期 ≤ 1 次/分钟」。
type deviceCache struct {
	registerURL string
	client      *http.Client
	logger      *slog.Logger
	now         func() time.Time // 测试注入；生产是 time.Now
	rand        io.Reader        // 测试注入确定性随机源；生产是 crypto/rand

	mu         sync.Mutex
	cached     *deviceAttempt // 单条：成功或失败都在这
	inflight   chan struct{}  // 非 nil = 有一次注册在途；关闭即落定
	renewAfter time.Time      // 下一次允许换身份的时刻（续期 ≤ 1 次/分钟）
}

// deviceAttempt 是一次注册的结果。成功时 retryAt 为零值（永不重试）。
type deviceAttempt struct {
	id      deviceIdentity
	err     error
	retryAt time.Time
}

// newDeviceCache 构造缓存。registerURL 为空时用默认端点。
func newDeviceCache(registerURL string, client *http.Client, logger *slog.Logger) *deviceCache {
	if registerURL == "" {
		registerURL = defaultDeviceRegisterURL
	}
	return &deviceCache{
		registerURL: registerURL,
		client:      client,
		logger:      logger,
		now:         time.Now,
		rand:        rand.Reader,
	}
}

// get 取身份：命中缓存直接回；在途就等它落定；都没有就自己发起唯一一次注册。
func (d *deviceCache) get(ctx context.Context) (deviceIdentity, error) {
	for {
		d.mu.Lock()
		cur := d.cached
		wait := d.inflight
		switch {
		case cur != nil && cur.err == nil:
			d.mu.Unlock()
			return cur.id, nil
		case cur != nil && d.now().Before(cur.retryAt):
			// 失败态也在缓存里：静默期内不重试，调用方拿到同一条可判定错误。
			d.mu.Unlock()
			return deviceIdentity{}, cur.err
		case wait == nil:
			d.inflight = make(chan struct{})
			d.mu.Unlock()
			return d.attempt(ctx)
		}
		d.mu.Unlock()
		select {
		case <-wait:
			// 注册已落定（成功或失败都进缓存），回到循环读结果。
		case <-ctx.Done():
			return deviceIdentity{}, ctx.Err()
		}
	}
}

// renew 在「上游刚刚拒了 rejected 这条身份」时换一条新的（626 契约：更新一次
// 匿名设备并重试）。三条约束：
//
//   - 别人已经换过（当前身份不是被拒的那条）→ 直接回当前身份，不重复注册；
//   - 距上次续期不足 60 秒 → 不注册，回 plugin.ErrRejected（续期 ≤ 1 次/分钟）；
//   - 在途注册优先等它落定，与 get 共用同一个 single-flight。
func (d *deviceCache) renew(ctx context.Context, rejected deviceIdentity) (deviceIdentity, error) {
	for {
		d.mu.Lock()
		cur := d.cached
		wait := d.inflight
		if cur != nil && cur.err == nil && cur.id != rejected {
			d.mu.Unlock()
			return cur.id, nil
		}
		if d.now().Before(d.renewAfter) {
			d.mu.Unlock()
			return deviceIdentity{}, fmt.Errorf("%w: 设备续期已达每分钟一次上限", plugin.ErrRejected)
		}
		if wait == nil {
			d.inflight = make(chan struct{})
			d.renewAfter = d.now().Add(deviceRetryInterval)
			d.mu.Unlock()
			return d.attempt(ctx)
		}
		d.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return deviceIdentity{}, ctx.Err()
		}
	}
}

// attempt 是「本次调用负责发起注册」的那条路径：结果写回缓存再唤醒等待者。
func (d *deviceCache) attempt(ctx context.Context) (deviceIdentity, error) {
	id, err := d.register(ctx)

	d.mu.Lock()
	done := d.inflight
	d.inflight = nil
	if ctxErr := ctx.Err(); ctxErr != nil {
		// 调用方自己的取消不代表上游状态：缓存 60 秒等于把一次取消变成一分钟
		// 的「注册不可用」，所以这条失败不进缓存。
		d.mu.Unlock()
		close(done)
		return deviceIdentity{}, ctxErr
	}
	att := &deviceAttempt{id: id, err: err}
	if err != nil {
		att = &deviceAttempt{err: err, retryAt: d.now().Add(deviceRetryInterval)}
	}
	d.cached = att
	d.mu.Unlock()
	close(done)
	return att.id, att.err
}

// register 发一次注册请求。失败一律 plugin.ErrRejected（626 错误表：设备注册失败、
// 空响应、业务拒绝、风控 403/风控页都归这一条），取消则回 ctx 自己的错误。
func (d *deviceCache) register(ctx context.Context) (deviceIdentity, error) {
	now := d.now()
	cdid, err := deviceUUID(d.rand)
	if err != nil {
		return deviceIdentity{}, fmt.Errorf("%w: 生成 cdid: %v", plugin.ErrRejected, err)
	}
	body, err := buildDeviceRegisterBody(d.rand, cdid, now.UnixMilli())
	if err != nil {
		return deviceIdentity{}, fmt.Errorf("%w: 构造注册信封: %v", plugin.ErrRejected, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.registerURL, bytes.NewReader(body))
	if err != nil {
		return deviceIdentity{}, fmt.Errorf("%w: 构造注册请求: %v", plugin.ErrRejected, err)
	}
	setDeviceRegisterHeaders(req, body, now)
	logPath := req.URL.Path

	resp, err := d.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return deviceIdentity{}, ctxErr
		}
		// 日志只有源 / 路径 / 状态 / 字节数：身份与响应体都不进日志。
		d.logger.WarnContext(ctx, "设备注册失败",
			"plugin", Name, "path", logPath, "upstream_status", 0, "bytes", 0)
		return deviceIdentity{}, fmt.Errorf("%w: POST %s: %v", plugin.ErrRejected, logPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxDeviceResponseBytes+1))
	level := slog.LevelInfo
	if readErr != nil || resp.StatusCode != http.StatusOK {
		level = slog.LevelWarn
	}
	d.logger.Log(ctx, level, "设备注册",
		"plugin", Name, "path", logPath, "upstream_status", resp.StatusCode, "bytes", len(raw))

	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return deviceIdentity{}, ctxErr
		}
		return deviceIdentity{}, fmt.Errorf("%w: 读取注册响应: %v", plugin.ErrRejected, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return deviceIdentity{}, fmt.Errorf("%w: POST %s 返回 %d", plugin.ErrRejected, logPath, resp.StatusCode)
	}
	if len(raw) == 0 || len(raw) > maxDeviceResponseBytes {
		return deviceIdentity{}, fmt.Errorf("%w: POST %s 响应体长度为 %d", plugin.ErrRejected, logPath, len(raw))
	}
	id, err := parseDeviceRegisterResponse(raw)
	if err != nil {
		return deviceIdentity{}, err
	}
	id.CDID = cdid
	return id, nil
}

// setDeviceRegisterHeaders 挂上报文与身份头：信封是二进制体，Content-Type 带
// tt-data=a，x-ss-stub 是整包的 MD5（协议事实，不是安全加固）。
func setDeviceRegisterHeaders(req *http.Request, body []byte, now time.Time) {
	req.Header.Set("User-Agent", deviceUA)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/octet-stream;tt-data=a")
	req.Header.Set("log-encode-type", "gzip")
	req.Header.Set("x-ss-dp", strconv.Itoa(deviceAid))
	req.Header.Set("x-ss-req-ticket", strconv.FormatInt(now.UnixMilli(), 10))
	req.Header.Set("x-vc-bdturing-sdk-version", "4.0.3.cn")
	req.Header.Set("lc", "101")
	req.Header.Set("sdk-version", "2")
	req.Header.Set("passport-sdk-version", "5051452")
	req.Header.Set("x-neptune", "-8|50:51:59:30:40:47:49:39")
	req.Header.Set("x-ss-stub", deviceStub(body))
}

// deviceRegisterEnvelope 是信封解开后的那层 JSON：magic_tag 固定，header 是设备
// 参数，_gen_time 是毫秒时间戳。
type deviceRegisterEnvelope struct {
	MagicTag string        `json:"magic_tag"`
	Header   deviceProfile `json:"header"`
	GenTime  int64         `json:"_gen_time"`
}

// deviceProfile 是信封 header 段。
//
// 字段与取值是「App 在什么设备上、什么版本」的协议事实：上游据此识别设备，
// 少一个字段或换一个版本号都可能被安全校验挡（626 风险 2）。它们不是可调参数，
// 也不进 settings——上游改版即全体失效，只能人工跟进。
type deviceProfile struct {
	DisplayName         string       `json:"display_name"`
	UpdateVersionCode   int          `json:"update_version_code"`
	ManifestVersionCode int          `json:"manifest_version_code"`
	AppVersionMinor     string       `json:"app_version_minor"`
	Aid                 int          `json:"aid"`
	Channel             string       `json:"channel"`
	Package             string       `json:"package"`
	AppVersion          string       `json:"app_version"`
	VersionCode         int          `json:"version_code"`
	SdkVersion          string       `json:"sdk_version"`
	SdkTargetVersion    int          `json:"sdk_target_version"`
	GitHash             string       `json:"git_hash"`
	OS                  string       `json:"os"`
	OSVersion           string       `json:"os_version"`
	OSAPI               int          `json:"os_api"`
	DeviceModel         string       `json:"device_model"`
	DeviceBrand         string       `json:"device_brand"`
	DeviceManufacturer  string       `json:"device_manufacturer"`
	CPUABI              string       `json:"cpu_abi"`
	ReleaseBuild        string       `json:"release_build"`
	DensityDPI          int          `json:"density_dpi"`
	DisplayDensity      string       `json:"display_density"`
	Resolution          string       `json:"resolution"`
	Language            string       `json:"language"`
	Timezone            int          `json:"timezone"`
	Access              string       `json:"access"`
	NotRequestSender    int          `json:"not_request_sender"`
	Carrier             string       `json:"carrier"`
	MCCMNC              string       `json:"mcc_mnc"`
	ROM                 string       `json:"rom"`
	ROMVersion          string       `json:"rom_version"`
	Cdid                string       `json:"cdid"`
	SigHash             string       `json:"sig_hash"`
	Openudid            string       `json:"openudid"`
	Clientudid          string       `json:"clientudid"`
	IPv6List            []string     `json:"ipv6_list"`
	Region              string       `json:"region"`
	TZName              string       `json:"tz_name"`
	TZOffset            int          `json:"tz_offset"`
	SimRegion           string       `json:"sim_region"`
	SimSerialNumber     []string     `json:"sim_serial_number"`
	OaidMaySupport      bool         `json:"oaid_may_support"`
	ReqID               string       `json:"req_id"`
	DevicePlatform      string       `json:"device_platform"`
	Custom              deviceCustom `json:"custom"`
	PreInstalledChannel string       `json:"pre_installed_channel"`
	ApkFirstInstallTime int64        `json:"apk_first_install_time"`
	IsSystemApp         int          `json:"is_system_app"`
	SdkFlavor           string       `json:"sdk_flavor"`
	GuestMode           int          `json:"guest_mode"`
}

// deviceCustom 是 header 里的 custom 段（区域与设备形态声明）。
type deviceCustom struct {
	AccountRegionSrc string `json:"account_region_src"`
	AccountRegion    string `json:"account_region"`
	HostBit          int    `json:"host_bit"`
	DragonDeviceType string `json:"dragon_device_type"`
}

// buildDeviceRegisterBody 造注册请求体：设备参数 → JSON → 信封。
//
// 除 cdid 与时间戳由调用方给（cdid 要跟着身份一起返回），其余标识都从 random
// 取，因此同一顺序的同一份 random 与同一份明文必然产出同一份请求体。
func buildDeviceRegisterBody(random io.Reader, cdid string, nowMillis int64) ([]byte, error) {
	openudid, err := deviceHexID(random, 8)
	if err != nil {
		return nil, err
	}
	clientudid, err := deviceUUID(random)
	if err != nil {
		return nil, err
	}
	reqID, err := deviceUUID(random)
	if err != nil {
		return nil, err
	}
	profile := deviceProfile{
		DisplayName:         "红果免费短剧",
		UpdateVersionCode:   73932,
		ManifestVersionCode: 73932,
		AppVersionMinor:     "7.3.9.32",
		Aid:                 deviceAid,
		Channel:             "update_64",
		Package:             "com.phoenix.read",
		AppVersion:          "7.3.9.32",
		VersionCode:         73932,
		SdkVersion:          "3.7.0-rc.34-fanqie-xiaoshuo",
		SdkTargetVersion:    29,
		GitHash:             "3a88f4e",
		OS:                  "Android",
		OSVersion:           "16",
		OSAPI:               36,
		DeviceModel:         "25053RT47C",
		DeviceBrand:         "Redmi",
		DeviceManufacturer:  "Redmi",
		CPUABI:              "arm64-v8a",
		ReleaseBuild:        "b5e187f",
		DensityDPI:          520,
		DisplayDensity:      "mdpi",
		Resolution:          "1280x2772",
		Language:            "zh",
		Timezone:            8,
		Access:              "wifi",
		NotRequestSender:    0,
		Carrier:             "CHINA MOBILE",
		MCCMNC:              "46000",
		ROM:                 "eng.root.20231116.160340",
		ROMVersion:          "W528JS release-keys",
		Cdid:                cdid,
		SigHash:             "56a962410c494bbaf0b58dba20cae56f",
		Openudid:            openudid,
		Clientudid:          clientudid,
		IPv6List:            []string{},
		Region:              "CN",
		TZName:              "Asia/Shanghai",
		TZOffset:            28800,
		SimRegion:           "cn",
		SimSerialNumber:     []string{},
		OaidMaySupport:      false,
		ReqID:               reqID,
		DevicePlatform:      "android",
		Custom: deviceCustom{
			AccountRegionSrc: "fallback",
			AccountRegion:    "cn",
			HostBit:          64,
			DragonDeviceType: "phone",
		},
		PreInstalledChannel: "",
		ApkFirstInstallTime: nowMillis,
		IsSystemApp:         0,
		SdkFlavor:           "china",
		GuestMode:           0,
	}
	plaintext, err := json.Marshal(deviceRegisterEnvelope{
		MagicTag: "ss_app_log",
		Header:   profile,
		GenTime:  nowMillis,
	})
	if err != nil {
		return nil, fmt.Errorf("编码注册参数: %w", err)
	}
	return deviceEnvelope(plaintext, random)
}

// deviceRegisterResponse 是注册响应：code 与两个标识。标识既可能是数字，也可能是
// 字符串（上游两种形态都出现过），也存在 *_str 优先的写法。
type deviceRegisterResponse struct {
	Code         json.Number     `json:"code"`
	DeviceID     json.RawMessage `json:"device_id"`
	DeviceIDStr  json.RawMessage `json:"device_id_str"`
	InstallID    json.RawMessage `json:"install_id"`
	InstallIDStr json.RawMessage `json:"install_id_str"`
}

// parseDeviceRegisterResponse 解注册响应。code 非 0、缺标识、结构不符都归
// plugin.ErrRejected：调用方据此走「无直链」分支，拿到的一定是可判定的错误。
func parseDeviceRegisterResponse(body []byte) (deviceIdentity, error) {
	var resp deviceRegisterResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return deviceIdentity{}, fmt.Errorf("%w: 注册响应不是 JSON: %v", plugin.ErrRejected, err)
	}
	if code := resp.Code.String(); code != "" && code != "0" {
		return deviceIdentity{}, fmt.Errorf("%w: 注册响应 code=%s", plugin.ErrRejected, code)
	}
	id := deviceIdentity{
		DeviceID: deviceIDText(resp.DeviceIDStr, resp.DeviceID),
		IID:      deviceIDText(resp.InstallIDStr, resp.InstallID),
	}
	if id.DeviceID == "" || id.IID == "" {
		return deviceIdentity{}, fmt.Errorf("%w: 注册响应缺 device_id / install_id", plugin.ErrRejected)
	}
	return id, nil
}

// deviceIDText 取标识文本：*_str 优先（非空时），否则退回数字形态；两种形态
// 都只有「全数字、最多 30 位、不是全零」才算有效，其余按缺失处理。
func deviceIDText(preferred, fallback json.RawMessage) string {
	for _, raw := range []json.RawMessage{preferred, fallback} {
		if id := canonicalDeviceID(rawText(raw)); id != "" {
			return id
		}
	}
	return ""
}

// rawText 把 JSON 标量取成文本：字符串去引号，数字保留字面量，其它返回空。
func rawText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] == 'n' { // null
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	if raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9') {
		return string(raw)
	}
	return ""
}

// canonicalDeviceID 校验标识形态：1–30 位十进制且至少一个非零数字。
func canonicalDeviceID(s string) string {
	if len(s) == 0 || len(s) > 30 {
		return ""
	}
	nonzero := false
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return ""
		}
		if s[i] != '0' {
			nonzero = true
		}
	}
	if !nonzero {
		return ""
	}
	return s
}

// deviceUUID 从 random 取 16 字节拼成 UUIDv4 文本（cdid / clientudid / req_id 用）。
func deviceUUID(random io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", fmt.Errorf("读取设备随机数: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// deviceHexID 从 random 取 n 字节拼成小写十六进制（openudid 用）。
func deviceHexID(random io.Reader, n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", fmt.Errorf("读取设备随机数: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

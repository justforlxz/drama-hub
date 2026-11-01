package main

// MKACG-631 T5：端到端。假上游 + 真 ffmpeg 跑通完整片源播放。
//
// 链路：假官网/假 App 端点 → 真 hongguo 插件 Resolve（spade_a → 16 字节 key）→
// stream.Resolver → segment 按需建索引（ffprobe `-decryption_key`）→ 分片现转封装
// （ffmpeg `-decryption_key`）→ playlist。断言咬住三件事：
//
//  1. 完整片源与网页预览用时**时长**区分（24 秒 vs 10 秒），不是只看 200；
//  2. 取到的分片能被真 ffprobe 解析、能无错解码，时长之和等于夹具声明；
//  3. App 被拒时 playlist 502 + 响应体给原因 + 日志带通道与错误类别（不回落预览）。
//
// 夹具全部现场生成（`-encryption_scheme cenc-aes-ctr`，见 e2eEncryptMP4）：密文
// 产物依赖 ffmpeg 版本，提交进 testdata 后人无法重放，所以只提交「怎么生成」。
//
// 唯一不在真实链路上的一步是 localOrigin：插件只接受白名单媒体域名
// （`*.fqnovelvod.com`，https + 443，docs/PLUGIN-CONTRACT.md），而测试容器
// `--network=none` 且白名单域名不可能指到本地。适配器只把 origin 换成本地假 CDN，
// 路径/签名原样保留 —— 被换掉的只是 DNS，不是任何判断。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/bits"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/plugin/hongguo"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
)

const (
	// e2eSecret 是出流签名密钥（与其他装配测试同一个值）。
	e2eSecret = "0123456789abcdef0123456789abcdef"

	// e2eKeyHex 是完整片源的内容密钥（16 字节 AES-128，CENC-AES-CTR）。它同时
	// 由 spade_a 编码进「上游响应」，所以插件派生出的 key 与加密用的 key 是同一个。
	e2eKeyHex = "00112233445566778899aabbccddeeff"

	// e2eFullSeconds / e2ePreviewSeconds 是两套夹具的时长：完整集必须明显长于
	// 网页预览，时长断言才有区分力（625 F 的「≥60 秒」在真机上测，这里只证明
	// 拿到的是完整片源而不是 10 秒预览）。
	e2eFullSeconds    = 24
	e2ePreviewSeconds = 10

	// e2eAppMediaURL 是假 App 端点声明的媒体地址：必须在插件的白名单域名内
	// （协议事实），实际字节由本地假 CDN 提供（见文件头 localOrigin 说明）。
	e2eAppMediaURL = "https://v3-web.fqnovelvod.com/media/full.mp4"

	// e2eRegisterPath 是匿名设备注册端点（App 通道取址的前置）。
	e2eRegisterPath = "/service/2/device_register/"

	// e2eSIDProtected / e2eSIDOpen 是两套假剧：受保护剧（网页只给预览，
	// protectedPlayback=true）与非保护剧（集页按集号是否存在决定 404）。
	e2eSIDProtected = "7503745441910017049"
	// e2eSIDOpen 是同一部剧的另一份「剧目标识」：两套夹具必须落在两个 provider_id 上
	// （UpsertDrama 按 (provider, provider_id) 去重），所以假上游把夹具里的
	// series_id 换成了这个值再回。
	e2eSIDOpen = "7503745441910088888"
)

// e2eVidProtected 是受保护剧 vid_list 的前几项（取自 page-series.html 的真实列表）。
// 集号 = 下标 + 1，所以 ep1 用 e2eVidProtected[0]、ep4 用 [3]。
var e2eVidProtected = []string{
	"7503780574541122622",
	"7503780577347128382",
	"7503780574608231486",
	"7503780574738254910",
	"7503780575459675198",
}

// e2eUpstream 是假上游：一个 httptest 服务同时冒充官网（系列页 / 集页）、匿名
// 设备注册端点、App 取流端点与媒体 CDN。
type e2eUpstream struct {
	mu        sync.Mutex
	series    map[string]string // sid → 系列页 HTML
	episodes  map[string]string // "sid/vid" → 集页 HTML（缺 → 404）
	media     map[string][]byte // 路径 → 字节
	appStatus int               // App 取流端点的固定应答码（200 = 正常）
	spadeA    string            // 上游响应里的 spade_a（编码了 e2eKeyHex）
	hits      map[string]int    // 路径 → 请求次数
}

func newE2EUpstream(spadeA string) *e2eUpstream {
	return &e2eUpstream{
		series:    map[string]string{},
		episodes:  map[string]string{},
		media:     map[string][]byte{},
		appStatus: http.StatusOK,
		spadeA:    spadeA,
		hits:      map[string]int{},
	}
}

func (u *e2eUpstream) setSeries(sid, html string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.series[sid] = html
}

func (u *e2eUpstream) setEpisode(sid, vid, html string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.episodes[sid+"/"+vid] = html
}

func (u *e2eUpstream) setMedia(path string, body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.media[path] = body
}

func (u *e2eUpstream) setAppStatus(status int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.appStatus = status
}

// hits 返回某个路径被请求的次数（读的是计数，不是「最后一次请求」）。
func (u *e2eUpstream) hit(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

func (u *e2eUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.hits[r.URL.Path]++
	u.mu.Unlock()

	switch {
	case strings.HasPrefix(r.URL.Path, "/media/"):
		u.serveMedia(w, r)
	case r.URL.Path == e2eRegisterPath:
		// 真实上游每次注册发一条新身份；端到端只要求「注册成功」。
		_, _ = io.WriteString(w, `{"code":0,"device_id":1001,"install_id":2001}`)
	case r.URL.Path == "/video":
		u.serveApp(w, r)
	case strings.HasPrefix(r.URL.Path, "/player/"):
		u.servePlayer(w, r)
	default:
		http.NotFound(w, r)
	}
}

// serveMedia 冒充媒体 CDN：Range 由 http.ServeContent 处理（ffmpeg 的
// `-ss` 与索引探测都靠它按需拉区间）。
func (u *e2eUpstream) serveMedia(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	body, ok := u.media[r.URL.Path]
	u.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, filepath.Base(r.URL.Path), time.Time{}, bytes.NewReader(body))
}

func (u *e2eUpstream) servePlayer(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/player/")
	sid, vid, isEpisode := strings.Cut(rest, "/")

	u.mu.Lock()
	html, ok := u.series[sid]
	if isEpisode {
		html, ok = u.episodes[sid+"/"+vid]
	}
	u.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, html)
}

// serveApp 冒充 App 取流端点：把请求里的剧集 vid 回成一行「h264 + spade_a」片源。
func (u *e2eUpstream) serveApp(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MixedVideoIDMap map[string][]string `json:"mixed_video_id_map"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	vids := body.MixedVideoIDMap["1004"]
	if len(vids) != 1 {
		http.Error(w, "missing video id", http.StatusBadRequest)
		return
	}

	u.mu.Lock()
	status, spadeA := u.appStatus, u.spadeA
	u.mu.Unlock()
	if status != http.StatusOK {
		// 带一个非空响应体：上游的拒绝是「有内容的状态码」，插件据此归 ErrRejected
		// 并保留状态码（空体也会归 ErrRejected，但错误文本就没有 403 可读了）。
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"code":-1,"message":"rejected"}`)
		return
	}
	payload, err := json.Marshal(map[string]any{
		"Code": 0,
		"data": map[string]any{vids[0]: map[string]any{"video_model": map[string]any{
			"video_list": map[string]any{
				"360p": map[string]any{
					"main_url":   e2eAppMediaURL,
					"codec_type": "h264",
					"video_meta": map[string]any{
						"codec_type": "h264", "vwidth": 160, "vheight": 120, "bitrate": 200000,
					},
					"encrypt_info": map[string]any{"spade_a": spadeA, "encrypt": true},
				},
			},
		}}},
	})
	if err != nil {
		http.Error(w, "marshal response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

// localOrigin 把插件解析出的媒体地址的 origin 换成本地假 CDN（理由见文件头）。
// 只改 scheme/host：路径与签名原样保留，所以「哪一集、什么片源」仍由插件决定。
type localOrigin struct {
	*hongguo.Plugin
	cdn *url.URL
}

func (p localOrigin) Resolve(ctx context.Context, ref plugin.WorkRef, epNo int) (plugin.Stream, error) {
	st, err := p.Plugin.Resolve(ctx, ref, epNo)
	if err != nil {
		return plugin.Stream{}, err
	}
	u, err := url.Parse(st.URL)
	if err != nil {
		return plugin.Stream{}, err
	}
	u.Scheme, u.Host = p.cdn.Scheme, p.cdn.Host
	st.URL = u.String()
	return st, nil
}

// buildSpadeA 把 16 字节内容密钥编成上游的 spade_a 形态：deriveAppContentKey 的
// 逆运算（同一条协议规则，测试侧反向写，不拷参考实现的字节）。
func buildSpadeA(t *testing.T, key []byte) string {
	t.Helper()
	const (
		keyHexLen = 32
		initOdd   = 0x55
		initEven  = 0xf6
	)
	if len(key) != 16 {
		t.Fatalf("夹具密钥长度 = %d, 期望 16", len(key))
	}
	// 有效段：work[0] 前缀（派生时跳过）、work[1..32] 密钥十六进制、末位冗余。
	work := make([]byte, keyHexLen+2)
	copy(work[1:], hex.EncodeToString(key))
	work[0] = 0x41
	work[len(work)-1] = 0x7a

	odd, even := byte(initOdd), byte(initEven)
	encoded := make([]byte, len(work))
	for i := range work {
		previous := even
		if i&1 == 1 {
			previous = odd
		}
		old := previous ^ byte((int(work[i])+21+bits.OnesCount(uint(i)))&0xff)
		encoded[i] = old
		if i&1 == 1 {
			odd = old
		} else {
			even = old
		}
	}
	// 首 3 字节异或为 0：有效段长度取到上限（与派生侧一致）。
	raw := append([]byte{encoded[0] ^ encoded[1]}, encoded...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// e2ePlainMP4 现场造一份明文夹具（10 fps、每 10 帧一个关键帧：切片点可预测）。
func e2ePlainMP4(t *testing.T, seconds int) ([]byte, float64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("plain-%d.mp4", seconds))
	out, err := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=160x120:rate=10:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-g", "10", "-keyint_min", "10", "-sc_threshold", "0",
		"-movflags", "+faststart", path,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("生成明文夹具失败: %v（%s）", err, out)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读明文夹具: %v", err)
	}
	return body, e2eProbeDuration(t, path, "")
}

// e2eEncryptMP4 现场造 seconds 秒的明文再原地加密（命令与 MKACG-626 实测一致），
// 返回密文与它声明的时长（带 key 探测，即「夹具声明」）。
func e2eEncryptMP4(t *testing.T, seconds int) ([]byte, float64) {
	t.Helper()
	plain, _ := e2ePlainMP4(t, seconds)
	dir := t.TempDir()
	in := filepath.Join(dir, "plain.mp4")
	if err := os.WriteFile(in, plain, 0o644); err != nil {
		t.Fatalf("写明文夹具: %v", err)
	}
	out := filepath.Join(dir, "enc.mp4")
	kid := strings.Repeat("1", 32)
	cmd := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-i", in, "-c", "copy",
		"-encryption_scheme", "cenc-aes-ctr",
		"-encryption_key", e2eKeyHex,
		"-encryption_kid", kid,
		"-movflags", "+faststart", out,
	)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成 CENC 密文夹具失败: %v（%s）", err, msg)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("读密文夹具: %v", err)
	}
	return body, e2eProbeDuration(t, out, e2eKeyHex)
}

// e2eProbeDuration 用真 ffprobe 取容器时长；keyHex 非空时带 `-decryption_key`
// （顺序与播放面一致：必须落在源地址之前）。
func e2eProbeDuration(t *testing.T, path, keyHex string) float64 {
	t.Helper()
	args := []string{"-v", "error"}
	if keyHex != "" {
		args = append(args, "-decryption_key", keyHex)
	}
	args = append(args, "-show_entries", "format=duration", "-of", "csv=p=0", path)
	raw, err := exec.Command("ffprobe", args...).Output()
	if err != nil {
		t.Fatalf("ffprobe 取时长 %s: %v", path, err)
	}
	dur, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	if err != nil {
		t.Fatalf("解析 ffprobe 时长 %q: %v", raw, err)
	}
	return dur
}

// e2eDecodesCleanly 断言分片能完整解码：`ffmpeg -v error -i seg.ts -f null -`
// 退出 0 且没有任何错误输出。解密没生效时 ffmpeg 仍是 exit 0，但会报
// `non-existing PPS …` 之类的解码错误（MKACG-626 实测），所以必须看输出。
func e2eDecodesCleanly(t *testing.T, path string) {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("解码分片 %s 失败: %v（%s）", path, err, out)
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		t.Fatalf("解码分片 %s 有错误输出（解密/切片有问题）: %s", path, msg)
	}
}

// e2ePlaylist 取 playlist 并逐片下载校验，返回 playlist 文本与实测总时长。
func e2ePlaylist(t *testing.T, h http.Handler, uid string) (string, float64) {
	t.Helper()
	id := stream.ID(e2eSecret, uid)
	path := "/video/" + id + ".m3u8"
	rec := serve(t, h, http.MethodGet, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d, 期望 200（body=%s）", path, rec.Code, rec.Body.String())
	}
	playlist := rec.Body.String()

	durs, uris := parseE2EPlaylist(t, playlist)
	total := 0.0
	for i, uri := range uris {
		seg := serve(t, h, http.MethodGet, uri)
		if seg.Code != http.StatusOK {
			t.Fatalf("GET %s 状态码 = %d, 期望 200（body=%s）", uri, seg.Code, seg.Body.String())
		}
		if ct := seg.Header().Get("Content-Type"); ct != "video/mp2t" {
			t.Errorf("分片 %d 的 Content-Type = %q, 期望 video/mp2t", i, ct)
		}
		file := filepath.Join(t.TempDir(), fmt.Sprintf("seg-%d.ts", i))
		if err := os.WriteFile(file, seg.Body.Bytes(), 0o644); err != nil {
			t.Fatalf("写分片 %d: %v", i, err)
		}
		got := e2eProbeDuration(t, file, "")
		if math.Abs(got-durs[i]) > 0.5 {
			t.Errorf("分片 %d 实测时长 %.3fs 与 EXTINF %.3fs 不符", i, got, durs[i])
		}
		e2eDecodesCleanly(t, file)
		total += got
	}
	return playlist, total
}

// parseE2EPlaylist 取出 EXTINF 时长与分片 URI（两者顺序一一对应）。
func parseE2EPlaylist(t *testing.T, body string) ([]float64, []string) {
	t.Helper()
	var durs []float64
	var uris []string
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, "#EXTINF:"); ok {
			num, _, _ := strings.Cut(rest, ",")
			d, err := strconv.ParseFloat(num, 64)
			if err != nil {
				t.Fatalf("EXTINF 不是数字: %q", line)
			}
			durs = append(durs, d)
			continue
		}
		if strings.HasPrefix(line, "/video/") {
			uris = append(uris, line)
		}
	}
	if len(durs) == 0 {
		t.Fatalf("playlist 没有分片：\n%s", body)
	}
	if len(durs) != len(uris) {
		t.Fatalf("EXTINF 数 %d 与分片 URI 数 %d 不一致：\n%s", len(durs), len(uris), body)
	}
	return durs, uris
}

// readE2EFixture 读插件侧的上游页面夹具（假上游直接回它）。
func readE2EFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "internal", "plugin", "hongguo", "testdata", "html", name)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读夹具 %s: %v", path, err)
	}
	return string(body)
}

// TestPlaybackFullEpisode 是 MKACG-631 的验收用例：假上游 + 真 ffmpeg，覆盖
// ep1（受保护剧，网页只给 10 秒预览）与 ep4（网页无片源）都走 App 成功，以及
// App 被拒时 playlist 502 且原因可读。
func TestPlaybackFullEpisode(t *testing.T) {
	ctx := context.Background()

	// ---- 夹具：完整集（CENC 密文，24 秒）与网页预览（明文，10 秒）----
	fullCipher, fullDur := e2eEncryptMP4(t, e2eFullSeconds)
	preview, previewDur := e2ePlainMP4(t, e2ePreviewSeconds)
	if fullDur < float64(e2ePreviewSeconds)+5 {
		t.Fatalf("完整夹具时长 %.2fs 不足以与 %.2fs 预览区分", fullDur, previewDur)
	}
	spadeA := buildSpadeA(t, mustE2EDecodeHex(t, e2eKeyHex))

	up := newE2EUpstream(spadeA)
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)

	up.setMedia("/media/full.mp4", fullCipher)
	// 受保护剧：集页是真实存在的「诱饵」——内容就是 10 秒预览。插件按契约不该请求它
	// （protectedPlayback==true），断言见下面的 hit 计数；App 不可用时它也是「回落预览」
	// 那条错路的具体形态（200 + 10 秒 playlist，见最后一条子测试）。
	previewPath := func(vid string) string { return "/media/" + vid + "/preview.mp4" }
	decoyEpisode := func(vid string) string {
		t.Helper()
		up.setMedia(previewPath(vid), preview)
		page := strings.ReplaceAll(readE2EFixture(t, "page-episode.html"), e2eVidProtected[0], vid)
		page = strings.Replace(page,
			// 夹具里那条 10 秒预览地址（站点根来自插件常量，测试不写死上游域名）。
			hongguo.DefaultBaseURL+"/video/public/"+vid+"/360p.mp4",
			srv.URL+previewPath(vid), 1)
		if !strings.Contains(page, srv.URL+previewPath(vid)) {
			t.Fatalf("诱饵集页没有替换成预览地址（vid=%s，夹具形态变了？）", vid)
		}
		return page
	}
	up.setSeries(e2eSIDProtected, readE2EFixture(t, "page-series-e2e.html"))
	up.setEpisode(e2eSIDProtected, e2eVidProtected[0], decoyEpisode(e2eVidProtected[0]))
	// ep2 也要有诱饵：App 被拒那条子测试断言「502 而不是 200 + 10 秒 playlist」，
	// 只有该集的预览页真实存在，这条断言才有区分力。
	up.setEpisode(e2eSIDProtected, e2eVidProtected[1], decoyEpisode(e2eVidProtected[1]))

	// 非保护剧：复用完整 76 集列表（只把 series_id 换成 e2eSIDOpen，页面解析会校验
	// 它等于请求的 sid），第 4 集的集页**不存在**（404 = 网页无片源）。
	openSeries := strings.ReplaceAll(readE2EFixture(t, "page-series.html"), e2eSIDProtected, e2eSIDOpen)
	up.setSeries(e2eSIDOpen, openSeries)

	// ---- 库：两套剧 + settings ----
	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	for k, v := range map[store.SettingKey]string{
		store.KeyStreamSecret: e2eSecret,
		store.KeySegTargetSec: "10",
		store.KeyCacheDir:     filepath.Join(t.TempDir(), "cache"),
	} {
		if err := settings.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	// addEpisode 造一部剧的某一集并返回 episode.id（播放单元 uid 的来源）。
	addEpisode := func(title, providerID string, epNo int) (dramaID, epID int64) {
		t.Helper()
		dramaID, err := st.UpsertDrama(ctx, store.Drama{
			Provider: hongguo.Name, ProviderID: providerID, Title: title, Year: 2026, Genre: "爱情",
		})
		if err != nil {
			t.Fatalf("UpsertDrama %s: %v", title, err)
		}
		epID, err = st.UpsertEpisode(ctx, store.Episode{
			DramaID: dramaID, EpNo: epNo, Source: hongguo.Name, Vid: "v" + strconv.Itoa(epNo),
		})
		if err != nil {
			t.Fatalf("UpsertEpisode %s ep%d: %v", title, epNo, err)
		}
		return dramaID, epID
	}
	protectedDrama, protectedEp1 := addEpisode("受保护剧", e2eSIDProtected, 1)
	_, protectedEp2 := addEpisode("受保护剧", e2eSIDProtected, 2)
	openDrama, openEp4 := addEpisode("非保护剧", e2eSIDOpen, 4)
	// 播放单元只认派生表（硬约束 6）：等价于一次同步完成后的重建。
	for _, id := range []int64{protectedDrama, openDrama} {
		if _, err := st.RebuildEmbyItems(ctx, id); err != nil {
			t.Fatalf("RebuildEmbyItems %d: %v", id, err)
		}
	}

	// ---- 真插件 + 本地 origin 适配器 + 装配根 ----
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	web := hongguo.New(hongguo.Config{
		BaseURL:           srv.URL,
		HTTPClient:        srv.Client(),
		Logger:            logger,
		DeviceRegisterURL: srv.URL + e2eRegisterPath,
		AppVideoURL:       srv.URL + "/video",
	})
	cdn, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("解析假 CDN 地址: %v", err)
	}
	reg := plugin.NewRegistry()
	if err := reg.Register(localOrigin{Plugin: web, cdn: cdn}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h := newAssembly(logger, st, settings, testSyncer(), fakeSchedule{}, testSources(), reg)

	// ep1（受保护剧）：网页只有预览，插件必须拿 App 的完整片源。
	t.Run("ep1 受保护剧走 App：拿到完整片源而不是 10 秒预览", func(t *testing.T) {
		playlist, total := e2ePlaylist(t, h, "e"+strconv.FormatInt(protectedEp1, 10))
		t.Logf("ep1 playlist（夹具声明 %.3fs，网页预览 %.3fs）：\n%s", fullDur, previewDur, playlist)
		t.Logf("ep1 分片实测总时长 = %.3fs", total)
		if math.Abs(total-fullDur) > 1.0 {
			t.Errorf("ep1 分片总时长 %.3fs, 期望夹具声明 %.3fs", total, fullDur)
		}
		if math.Abs(total-previewDur) < 4 {
			t.Errorf("ep1 分片总时长 %.3fs 与网页预览（%.3fs）接近——预览被当成了完整片源", total, previewDur)
		}
		// 受保护剧不请求集页：这条断言就是「10 秒预览没有被取用」的直接证据。
		if n := up.hit("/player/" + e2eSIDProtected + "/" + e2eVidProtected[0]); n != 0 {
			t.Errorf("受保护剧请求了集页 %d 次（网页只有 10 秒预览，不该取）", n)
		}
		if n := up.hit("/video"); n == 0 {
			t.Error("没有发出 App 取流请求（ep1 应走 App 通道）")
		}
	})

	// ep4（非保护剧）：集页 404 → 网页无片源 → 落 App，仍然拿到完整片源。
	t.Run("ep4 网页无片源走 App：集页 404 后仍拿到完整片源", func(t *testing.T) {
		_, total := e2ePlaylist(t, h, "e"+strconv.FormatInt(openEp4, 10))
		// 网页通道先试集页、拿到 404（ErrNoStream）才落 App。直链按契约不缓存，
		// 所以每次解析都会试一次：playlist 一次 + 每个分片一次。
		if n := up.hit("/player/" + e2eSIDOpen + "/" + e2eVidProtected[3]); n < 1 {
			t.Error("第 4 集页一次都没请求：网页无片源 → 落 App 的分支没被走到")
		}
		if math.Abs(total-fullDur) > 1.0 {
			t.Errorf("ep4 分片总时长 %.3fs, 期望夹具声明 %.3fs", total, fullDur)
		}
		if math.Abs(total-previewDur) < 4 {
			t.Errorf("ep4 分片总时长 %.3fs 与网页预览（%.3fs）接近——预览被当成了完整片源", total, previewDur)
		}
	})

	// 播放面的收尾断言：索引确实按 App 通道建立、时长回填到 episode.duration_sec。
	t.Run("索引与时长回填按 App 通道落库", func(t *testing.T) {
		segs, err := st.ListSegments(ctx, protectedEp1)
		if err != nil {
			t.Fatalf("ListSegments: %v", err)
		}
		if len(segs) == 0 {
			t.Fatal("ep1 没有建立分片索引")
		}
		if !strings.HasPrefix(segs[0].CodecSig, "app/") {
			t.Errorf("ep1 codec_sig = %q, 期望带 app/ 通道段（防跨通道串味）", segs[0].CodecSig)
		}
		ep, err := st.EpisodeByID(ctx, protectedEp1)
		if err != nil {
			t.Fatalf("EpisodeByID: %v", err)
		}
		if want := int(math.Round(fullDur)); ep.DurationSec != want {
			t.Errorf("ep1 duration_sec = %d, 期望回填 %d（ffprobe 实测）", ep.DurationSec, want)
		}
	})

	// App 被拒：502 + 可读原因 + 日志带通道与类别；不回落预览、不写索引。
	t.Run("App 被拒时 playlist 502 且原因可读", func(t *testing.T) {
		up.setAppStatus(http.StatusForbidden)
		before := logs.Len()

		id := stream.ID(e2eSecret, "e"+strconv.FormatInt(protectedEp2, 10))
		rec := serve(t, h, http.MethodGet, "/video/"+id+".m3u8")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("App 被拒时状态码 = %d, 期望 502（body=%s）", rec.Code, rec.Body.String())
		}
		// 626 D1 / MKACG-637：这一集的集页是真的能给出 10 秒预览。状态码 502 之外，
		// 再钉住「没去取它」「响应体不是 playlist」——回落到预览就只能是 200 + 10 秒。
		if n := up.hit("/player/" + e2eSIDProtected + "/" + e2eVidProtected[1]); n != 0 {
			t.Errorf("App 不可用时请求了集页 %d 次（10 秒预览被当成回落目标）", n)
		}
		if strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "#EXTM3U") {
			t.Error("App 不可用时回落到网页预览 playlist（200 + 10 秒）")
		}
		var body struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("502 响应不是合法 JSON: %v（body=%s）", err, rec.Body.String())
		}
		if body.Code != "upstream" {
			t.Errorf("错误码 code = %q, 期望 upstream", body.Code)
		}
		if !strings.Contains(body.Message, "403") {
			t.Errorf("502 响应体没给出可读原因（应含上游状态 403）: %q", body.Message)
		}

		// 日志：插件一条 WarnContext，含 channel=app 与错误类别。
		entry := logs.String()[before:]
		if !strings.Contains(entry, `"channel":"app"`) || !strings.Contains(entry, `"reason":"ErrRejected"`) {
			t.Errorf("失败日志缺 channel=app / reason=ErrRejected：%s", entry)
		}
		if strings.Contains(entry, spadeA) || strings.Contains(entry, e2eKeyHex) {
			t.Error("日志泄露了 spade_a 或内容密钥")
		}

		// 失败不污染库：这一集没有索引行。
		segs, err := st.ListSegments(ctx, protectedEp2)
		if err != nil {
			t.Fatalf("ListSegments: %v", err)
		}
		if len(segs) != 0 {
			t.Errorf("App 被拒后写了 %d 行分片索引，期望 0", len(segs))
		}
	})
}

// mustE2EDecodeHex 解十六进制夹具（写死在文件头的密钥）。
func mustE2EDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("夹具密钥不是十六进制: %v", err)
	}
	return b
}

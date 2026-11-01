package hongguo

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/bits"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// App 通道测试的夹具全部自建（626 D3）：spade_a 与 key_seed 密文由测试侧按
// app.go 里写的规则反向/正向构造，断言的是往返一致与拒绝路径，不拷参考实现
// 的字节、常量表或期望值。
//
// 白名单里的媒体域名是协议事实，只作为**字符串**出现在夹具里：插件从不出站取
// 媒体（那是播放面 ffmpeg 的事），测试也从不请求这些地址。

// appTestKey 是夹具用的内容密钥（16 字节，AES-128）。
var appTestKey = []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
	0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}

// appTestSeed 是夹具用的 key_seed 原始字节。
var appTestSeed = []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02, 0x03, 0x04}

// appTestMediaURL 是白名单内的一条媒体地址（带签名查询串，验证不被重排）。
const appTestMediaURL = "https://v3-web.fqnovelvod.com/video/public/7503780574541122622/360p.mp4?sig=ab%2Fcd&t=1"

// fakeApp 是假 App 上游：设备注册端点 + 取流端点，记录请求次数与最后一条请求。
type fakeApp struct {
	mu             sync.Mutex
	registerCalls  int
	videoCalls     int
	registerStatus int
	videoReply     func(call int) (int, string)
	lastQuery      url.Values
	lastBody       string
	lastHeaders    http.Header
}

// newFakeApp 起一个假 App 上游：注册端点每次回一条新的身份（真实上游也是
// 每次注册发一条新身份，续期测试靠它区分「换过没换过」）。
func newFakeApp(t *testing.T) (*fakeApp, *httptest.Server) {
	t.Helper()
	f := &fakeApp{registerStatus: http.StatusOK,
		videoReply: func(int) (int, string) { return http.StatusOK, "" }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		switch r.URL.Path {
		case "/service/2/device_register/":
			f.registerCalls++
			status := f.registerStatus
			call := f.registerCalls
			f.mu.Unlock()
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, deviceResponseBody(call))
			return
		case "/video":
			f.videoCalls++
			call := f.videoCalls
			reply := f.videoReply
			f.lastQuery = r.URL.Query()
			f.lastBody = string(body)
			f.lastHeaders = r.Header.Clone()
			f.mu.Unlock()
			status, resp := reply(call)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, resp)
			return
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// deviceResponseBody 是第 call 次注册的响应：每条身份的 device_id / install_id 不同。
func deviceResponseBody(call int) string {
	deviceID, iid := strconv.Itoa(1000+call), strconv.Itoa(2000+call)
	return `{"code":0,"device_id":` + deviceID + `,"install_id":` + iid +
		`,"device_id_str":"` + deviceID + `","install_id_str":"` + iid + `"}`
}

// setRegisterStatus 设置注册端点的应答状态（续期失败场景用）。
func (f *fakeApp) setRegisterStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerStatus = status
}

// replyVideo 设置取流端点的应答。
func (f *fakeApp) replyVideo(fn func(call int) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.videoReply = fn
}

// count 返回（注册次数, 取流次数）。
func (f *fakeApp) count() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registerCalls, f.videoCalls
}

// request 返回最后一次取流请求的 query / body / headers。
func (f *fakeApp) request() (url.Values, string, http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastQuery, f.lastBody, f.lastHeaders
}

// newAppTestPlugin 构造一个「网页通道指向 webSrv、App 通道指向 appSrv」的插件。
func newAppTestPlugin(t *testing.T, webSrv, appSrv *httptest.Server, logger *slog.Logger) *Plugin {
	t.Helper()
	client := webSrv.Client()
	return New(Config{
		BaseURL:           webSrv.URL,
		HTTPClient:        client,
		Logger:            logger,
		DeviceRegisterURL: appSrv.URL + "/service/2/device_register/",
		AppVideoURL:       appSrv.URL + "/video",
	})
}

// appRow 造片源列表里的一行。
func appRow(codec, mediaURL, spadeA string, width, height, bitrate int) map[string]any {
	return map[string]any{
		"main_url":   mediaURL,
		"codec_type": codec,
		"video_meta": map[string]any{
			"codec_type": codec, "vwidth": width, "vheight": height, "bitrate": bitrate,
		},
		"encrypt_info": map[string]any{"spade_a": spadeA, "encrypt": true},
	}
}

// appResponse 造取流响应：data[vid].video_model 是内联对象（上游的两种形态之一）。
func appResponse(t *testing.T, vid string, model map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"Code": 0,
		"data": map[string]any{vid: map[string]any{"video_model": model}},
	})
	if err != nil {
		t.Fatalf("构造取流响应: %v", err)
	}
	return string(body)
}

// buildSpadeA 把 16 字节密钥编成上游的 spade_a 形态（app.go 派生规则的逆运算）。
//
// 有效段布局：work[0] 是前缀字节（不参与密钥）、work[1..32] 是密钥的十六进制
// 文本、work[33] 是冗余字节；首 3 字节的异或决定有效段长度，这里取 0 让长度
// 取到上限（len-1）。
func buildSpadeA(t *testing.T, key []byte) string {
	t.Helper()
	if len(key) != appKeyLen {
		t.Fatalf("夹具密钥长度 = %d, 期望 %d", len(key), appKeyLen)
	}
	return buildSpadeText(t, hex.EncodeToString(key))
}

// buildSpadeText 用一个 32 字符的文本段编 spade_a（用于喂「不是十六进制」的负例）。
func buildSpadeText(t *testing.T, keyText string) string {
	t.Helper()
	if len(keyText) != appKeyHexLen {
		t.Fatalf("夹具文本长度 = %d, 期望 %d", len(keyText), appKeyHexLen)
	}
	work := make([]byte, appKeyHexLen+2)
	copy(work[1:], keyText)
	work[0] = 0x41 // 前缀字节，派生时被跳过
	work[len(work)-1] = 0x7a

	odd, even := byte(appSpadeInitOdd), byte(appSpadeInitEven)
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
	raw := append([]byte{0}, encoded...)
	// 首 3 字节异或为 0：有效段长度取到 len-1（与夹具布局一致）。
	raw[0] = encoded[0] ^ encoded[1]
	return base64.RawURLEncoding.EncodeToString(raw)
}

// buildEncryptedURL 用 key_seed 把媒体地址加密成上游形态（测试侧独立实现 KDF 与 CBC）。
func buildEncryptedURL(t *testing.T, mediaURL string, seed []byte) string {
	t.Helper()
	inner := sha512.Sum512(seed)
	h := sha512.New()
	h.Write(inner[:])
	h.Write(deviceKDFSalt)
	derived := h.Sum(nil)
	block, err := aes.NewCipher(derived[:16])
	if err != nil {
		t.Fatalf("构造夹具分组密码: %v", err)
	}
	plain := []byte(mediaURL)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(plain, bytes.Repeat([]byte{byte(pad)}, pad)...)
	ciphertext := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, derived[16:32]).CryptBlocks(ciphertext, plain)
	raw := append([]byte{appURLHeader0, 0x00, appURLHeader2, appURLHeader3}, ciphertext...)
	return base64.StdEncoding.EncodeToString(raw)
}

// appModelFixture 造一个「key_seed 加密地址 + spade_a 加密内容」的完整片源模型。
func appModelFixture(t *testing.T, videos map[string]any) map[string]any {
	t.Helper()
	return map[string]any{
		"key_seed":   base64.StdEncoding.EncodeToString(appTestSeed),
		"video_list": videos,
	}
}

// appModelPlainFixture 造一个没有 key_seed 的片源模型：地址是明文，只有内容加密。
func appModelPlainFixture(videos map[string]any) map[string]any {
	return map[string]any{"video_list": videos}
}

// TestAppResolve 覆盖 AC：App 返回 spade_a 时 Stream.Key 是 16 字节、URL 通过白名单；
// 同时验证取流请求的形态（POST、设备身份、剧集 vid）与「敏感面不进日志」。
func TestAppResolve(t *testing.T) {
	spade := buildSpadeA(t, appTestKey)
	encrypted := buildEncryptedURL(t, appTestMediaURL, appTestSeed)
	logs := &logSink{}
	web, webSrv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series-protected.html"))
	app, appSrv := newFakeApp(t)
	app.replyVideo(func(int) (int, string) {
		return http.StatusOK, appResponse(t, fixtureVID, appModelFixture(t, map[string]any{
			"360p": appRow("h264", encrypted, spade, 640, 360, 640_000),
		}))
	})
	p := newAppTestPlugin(t, webSrv, appSrv, slog.New(slog.NewTextHandler(logs, nil)))

	st, err := p.Resolve(context.Background(), testRef(), 1)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if st.URL != appTestMediaURL {
		t.Fatalf("直链 = %q, 期望 key_seed 解出的 %q", st.URL, appTestMediaURL)
	}
	if !bytes.Equal(st.Key, appTestKey) {
		t.Fatalf("Key = %x, 期望 spade_a 派生的 %x", st.Key, appTestKey)
	}
	if !st.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt = %v, 上游没给过期时间就该是零值", st.ExpiresAt)
	}

	// 受保护剧：网页只取一次详情页（不请求集页 → 不可能拿到 10 秒预览），App 注册与取流各一次。
	if reqs := web.requests(); len(reqs) != 1 || reqs[0] != "/player/"+fixtureSID {
		t.Fatalf("网页请求 = %v, 期望只有详情页一次", reqs)
	}
	if registers, videos := app.count(); registers != 1 || videos != 1 {
		t.Fatalf("App 请求 = 注册 %d / 取流 %d, 期望各一次", registers, videos)
	}

	query, body, headers := app.request()
	for _, key := range []string{"device_id", "iid", "cdid", "_rticket"} {
		if query.Get(key) == "" {
			t.Errorf("取流 query 缺 %s（设备身份/票据是 App 侧要求）", key)
		}
	}
	if got := query.Get("aid"); got != "8662" {
		t.Errorf("取流 query 的 aid = %q, 期望红果的产品号", got)
	}
	// 请求体逐个键对照：biz_param 的 8 个键都是协议必需（含 5 个必须是零值的开关），
	// 少一个或改成别的值上游就可能不回片源，所以整块钉住而不是只挑一个键断言。
	var sent struct {
		BizParam        map[string]any      `json:"biz_param"`
		MixedVideoIDMap map[string][]string `json:"mixed_video_id_map"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("取流请求体不是 JSON：%v（body=%s）", err, body)
	}
	wantBizParam := map[string]any{
		"detail_page_version":       float64(0),
		"device_level":              float64(3),
		"disable_digg_stat":         false,
		"need_all_video_definition": true,
		"need_mp4_align":            false,
		"use_os_player":             false,
		"use_server_dns":            false,
		"video_platform":            float64(1024),
	}
	if !reflect.DeepEqual(sent.BizParam, wantBizParam) {
		t.Errorf("biz_param = %v, 期望 %v", sent.BizParam, wantBizParam)
	}
	if got := sent.MixedVideoIDMap[appVideoIDKind]; len(got) != 1 || got[0] != fixtureVID {
		t.Errorf("mixed_video_id_map[%s] = %v, 期望只有本集 %s", appVideoIDKind, got, fixtureVID)
	}
	if got := headers.Get("User-Agent"); got != appUA {
		t.Errorf("取流 UA = %q, 期望 App 形态的 %q", got, appUA)
	}

	// 敏感面：spade_a、key_seed、内容密钥、媒体地址的查询串都不许出现在日志里。
	assertNotLogged(t, logs.text(),
		map[string]string{
			"spade_a":  spade,
			"key_seed": base64.StdEncoding.EncodeToString(appTestSeed),
			"内容密钥":     hex.EncodeToString(appTestKey),
			"地址查询串":    "sig=ab",
			"媒体路径":     "/video/public/" + fixtureVID,
		})
	// 通过白名单的域名可以落日志（排障需要）。
	if !strings.Contains(logs.text(), "fqnovelvod.com") {
		t.Errorf("日志缺域名，排障无从判断：%s", logs.text())
	}
}

// TestAppResolveKeyBranches 覆盖 appContentKey 的两条声明分支：
// 显式声明明文 → Key 为 nil；声明加密却缺 spade_a → ErrSchema（不能把密文当明文交付）。
func TestAppResolveKeyBranches(t *testing.T) {
	media := "https://v3-web.fqnovelvod.com/video/plain/360p.mp4?sig=1"
	row := func(encryptInfo map[string]any) map[string]any {
		return map[string]any{
			"main_url":     media,
			"codec_type":   "h264",
			"video_meta":   map[string]any{"codec_type": "h264", "vwidth": 640, "vheight": 360},
			"encrypt_info": encryptInfo,
		}
	}
	cases := []struct {
		name        string
		rows        map[string]any
		wantErr     error
		wantKeyless bool
	}{
		{
			name:        "声明明文（encrypt=false, 无 spade_a）→ Key 为 nil",
			rows:        map[string]any{"360p": row(map[string]any{"encrypt": false})},
			wantKeyless: true,
		},
		{
			name:    "声明加密但缺 spade_a → ErrSchema",
			rows:    map[string]any{"360p": row(map[string]any{"encrypt": true})},
			wantErr: plugin.ErrSchema,
		},
		{
			name:    "加密声明缺失（无 encrypt、无 spade_a）→ ErrSchema",
			rows:    map[string]any{"360p": row(map[string]any{})},
			wantErr: plugin.ErrSchema,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, webSrv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series-protected.html"))
			app, appSrv := newFakeApp(t)
			app.replyVideo(func(int) (int, string) {
				return http.StatusOK, appResponse(t, fixtureVID, appModelPlainFixture(tc.rows))
			})
			p := newAppTestPlugin(t, webSrv, appSrv, slog.New(slog.NewTextHandler(io.Discard, nil)))
			st, err := p.Resolve(context.Background(), testRef(), 1)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, 期望 %v", err, tc.wantErr)
				}
				if st.URL != "" || st.Key != nil {
					t.Fatalf("失败时仍产出了 Stream：%+v", st)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if st.URL != media {
				t.Fatalf("直链 = %q, 期望 %q", st.URL, media)
			}
			if tc.wantKeyless && st.Key != nil {
				t.Fatalf("Key = %x, 明文片源必须是 nil", st.Key)
			}
		})
	}
}

// TestAppResolvePicksH264 覆盖 626 的选流口径：h264 优先；只有在没有 h264 时才取 hevc。
func TestAppResolvePicksH264(t *testing.T) {
	spade := buildSpadeA(t, appTestKey)
	h264URL := "https://v3-web.qznovelvod.com/video/h264/360p.mp4?sig=1"
	hevcURL := "https://v3-web.qznovelvod.com/video/hevc/1080p.mp4?sig=2"

	cases := []struct {
		name string
		rows map[string]any
		want string
	}{
		{
			name: "h264 低清胜过 hevc 高清",
			rows: map[string]any{
				"1080p": appRow("hevc", hevcURL, spade, 1080, 1920, 2_000_000),
				"360p":  appRow("h264", h264URL, spade, 360, 640, 640_000),
			},
			want: h264URL,
		},
		{
			name: "只有 hevc 时取 hevc（仍带 key，交 ffmpeg copy）",
			rows: map[string]any{
				"1080p": appRow("bytevc1", hevcURL, spade, 1080, 1920, 2_000_000),
			},
			want: hevcURL,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, webSrv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series-protected.html"))
			app, appSrv := newFakeApp(t)
			app.replyVideo(func(int) (int, string) {
				return http.StatusOK, appResponse(t, fixtureVID, appModelPlainFixture(tc.rows))
			})
			p := newAppTestPlugin(t, webSrv, appSrv, slog.New(slog.NewTextHandler(io.Discard, nil)))

			st, err := p.Resolve(context.Background(), testRef(), 1)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if st.URL != tc.want {
				t.Fatalf("直链 = %q, 期望 %q", st.URL, tc.want)
			}
			if len(st.Key) != appKeyLen {
				t.Fatalf("Key 长度 = %d, 期望 %d", len(st.Key), appKeyLen)
			}
		})
	}
}

// TestAppResolveRenewFailureLogged 覆盖续期也失败时的日志：两个错误类别都要记下来
// （只看 ErrRejected 分不清是「取流被拒」还是「换身份被拒」）。
func TestAppResolveRenewFailureLogged(t *testing.T) {
	logs := &logSink{}
	_, webSrv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series-protected.html"))
	app, appSrv := newFakeApp(t)
	app.replyVideo(func(call int) (int, string) {
		if call == 1 {
			// 取流被拒后把注册也打成被拒：续期拿不到新身份。
			app.setRegisterStatus(http.StatusForbidden)
			return http.StatusForbidden, ""
		}
		return http.StatusInternalServerError, ""
	})
	p := newAppTestPlugin(t, webSrv, appSrv, slog.New(slog.NewTextHandler(logs, nil)))

	if _, err := p.Resolve(context.Background(), testRef(), 1); !errors.Is(err, plugin.ErrRejected) {
		t.Fatalf("err = %v, 期望 ErrRejected", err)
	}
	if registers, videos := app.count(); registers != 2 || videos != 1 {
		t.Fatalf("App 请求 = 注册 %d / 取流 %d, 期望注册两次（首次 + 续期）且取流只因首次被拒", registers, videos)
	}
	text := logs.text()
	for _, want := range []string{"reason=ErrRejected", "renew_reason=ErrRejected"} {
		if !strings.Contains(text, want) {
			t.Errorf("日志缺 %q：%s", want, text)
		}
	}
}

// TestAppResolveRenewsDeviceOnce 覆盖 626：App 拒了这条身份 → 换一条重试一次。
func TestAppResolveRenewsDeviceOnce(t *testing.T) {
	spade := buildSpadeA(t, appTestKey)
	media := "https://v3-web.douyinvod.com/video/x.mp4?sig=1"
	_, webSrv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series-protected.html"))
	app, appSrv := newFakeApp(t)
	app.replyVideo(func(call int) (int, string) {
		if call == 1 {
			return http.StatusForbidden, ""
		}
		return http.StatusOK, appResponse(t, fixtureVID, appModelPlainFixture(map[string]any{
			"360p": appRow("h264", media, spade, 640, 360, 640_000),
		}))
	})
	p := newAppTestPlugin(t, webSrv, appSrv, slog.New(slog.NewTextHandler(io.Discard, nil)))

	st, err := p.Resolve(context.Background(), testRef(), 1)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if st.URL != media {
		t.Fatalf("直链 = %q, 期望 %q", st.URL, media)
	}
	registers, videos := app.count()
	if registers != 2 || videos != 2 {
		t.Fatalf("App 请求 = 注册 %d / 取流 %d, 期望各两次（首次被拒 → 换身份重试一次）", registers, videos)
	}
	if got := app.lastQueryDeviceID(); got != "1002" {
		t.Fatalf("重试用的 device_id = %q, 期望续期后的新身份 1002（不能拿旧身份重试）", got)
	}
}

// TestAppResolveStatusAndBodyClassification 覆盖 app.go 的「状态码 × 响应体」判定
// 顺序（MKACG-635）：状态码先判，空体后判。每一格都断言错误类别与上游请求次数——
// 归类错了的代价不是日志措辞，而是多走一轮换身份、多打一次取流。
func TestAppResolveStatusAndBodyClassification(t *testing.T) {
	// 非 200 时上游带的 JSON 体只有诊断价值：判定不该看它。
	const upstreamErrorBody = `{"code":-1,"message":"no"}`
	// 200 + 业务 code != 0 是业务拒绝（与风控同一条动作）。
	const bizRejectedBody = `{"code":-1,"message":"rejected"}`

	cases := []struct {
		name       string
		status     int
		body       string
		want       error
		wantReg    int
		wantVideos int
	}{
		// 635 的现象：404 + 空体曾被「空体 → 被拒」抢在状态码之前，误归 ErrRejected。
		{name: "404 + 空体 → 无片源，只注册一次", status: http.StatusNotFound, body: "",
			want: plugin.ErrNoStream, wantReg: 1, wantVideos: 1},
		{name: "404 + JSON 体 → 无片源（既有归类）", status: http.StatusNotFound, body: upstreamErrorBody,
			want: plugin.ErrNoStream, wantReg: 1, wantVideos: 1},
		// 风控三格：判被拒 → 换一次身份，仍被拒 → 各两次上游请求。
		{name: "403 + 空体 → 被拒", status: http.StatusForbidden, body: "",
			want: plugin.ErrRejected, wantReg: 2, wantVideos: 2},
		{name: "429 + JSON 体 → 被拒", status: http.StatusTooManyRequests, body: upstreamErrorBody,
			want: plugin.ErrRejected, wantReg: 2, wantVideos: 2},
		{name: "444 + 空体 → 被拒（上游丢连接）", status: statusDropped, body: "",
			want: plugin.ErrRejected, wantReg: 2, wantVideos: 2},
		// 5xx：换身份解决不了服务端故障，不重试。
		{name: "500 + 空体 → 上游故障", status: http.StatusInternalServerError, body: "",
			want: plugin.ErrUpstream, wantReg: 1, wantVideos: 1},
		{name: "503 + JSON 体 → 上游故障", status: http.StatusServiceUnavailable, body: upstreamErrorBody,
			want: plugin.ErrUpstream, wantReg: 1, wantVideos: 1},
		// 200 + 空体是结构不符，不是身份被挡：同样不重试。
		{name: "200 + 空体 → 结构不符", status: http.StatusOK, body: "",
			want: plugin.ErrSchema, wantReg: 1, wantVideos: 1},
		{name: "200 + 非 JSON 体 → 结构不符", status: http.StatusOK, body: "not json",
			want: plugin.ErrSchema, wantReg: 1, wantVideos: 1},
		{name: "200 + 非零 code → 被拒", status: http.StatusOK, body: bizRejectedBody,
			want: plugin.ErrRejected, wantReg: 2, wantVideos: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, webSrv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series-protected.html"))
			app, appSrv := newFakeApp(t)
			app.replyVideo(func(int) (int, string) { return tc.status, tc.body })
			p := newAppTestPlugin(t, webSrv, appSrv, slog.New(slog.NewTextHandler(io.Discard, nil)))

			st, err := p.Resolve(context.Background(), testRef(), 1)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, 期望 %v", err, tc.want)
			}
			if st.URL != "" || st.Key != nil {
				t.Fatalf("失败时仍返回了 Stream: %+v", st)
			}
			if reg, videos := app.count(); reg != tc.wantReg || videos != tc.wantVideos {
				t.Fatalf("App 请求 = 注册 %d / 取流 %d, 期望 %d / %d",
					reg, videos, tc.wantReg, tc.wantVideos)
			}
		})
	}
}

// lastQueryDeviceID 取最后一次取流请求的 device_id（测试辅助）。
func (f *fakeApp) lastQueryDeviceID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastQuery == nil {
		return ""
	}
	return f.lastQuery.Get("device_id")
}

// TestDeriveContentKey 覆盖 spade_a → 16 字节 key 的往返与拒绝路径。
func TestDeriveContentKey(t *testing.T) {
	spade := buildSpadeA(t, appTestKey)
	got, err := deriveAppContentKey(spade)
	if err != nil {
		t.Fatalf("派生: %v", err)
	}
	if !bytes.Equal(got, appTestKey) {
		t.Fatalf("派生 key = %x, 期望 %x", got, appTestKey)
	}

	rejects := []struct {
		name  string
		spade string
	}{
		{"空值", ""},
		{"不是 base64", "!!!not-base64!!!"},
		{"长度不足", base64.RawURLEncoding.EncodeToString([]byte{1, 2})},
		{"密钥段不是十六进制", buildSpadeText(t, "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := deriveAppContentKey(tc.spade); !errors.Is(err, plugin.ErrSchema) {
				t.Fatalf("err = %v, 期望 ErrSchema", err)
			}
		})
	}
}

// TestDecryptAppURL 覆盖 key_seed 解 URL 的往返与拒绝路径（头不符 / 无完整分组）。
func TestDecryptAppURL(t *testing.T) {
	encrypted := buildEncryptedURL(t, appTestMediaURL, appTestSeed)
	got, err := decryptAppURL(encrypted, appTestSeed)
	if err != nil {
		t.Fatalf("解密: %v", err)
	}
	if got != appTestMediaURL {
		t.Fatalf("解出 = %q, 期望 %q", got, appTestMediaURL)
	}
	// 换一把 seed 解出来一定不是同一个地址（CBC 下不会静默解成原文）。
	if wrong, err := decryptAppURL(encrypted, []byte{0, 1, 2, 3}); err == nil && wrong == appTestMediaURL {
		t.Fatal("换 seed 仍解出原地址，派生或解密用错了输入")
	}

	badHeader := append([]byte{0x00, 0x00, 0x01, 0x00}, bytes.Repeat([]byte{0x11}, 16)...)
	if _, err := decryptAppURL(base64.StdEncoding.EncodeToString(badHeader), appTestSeed); !errors.Is(err, plugin.ErrSchema) {
		t.Fatalf("头不符：err = %v, 期望 ErrSchema", err)
	}
	short := append([]byte{appURLHeader0, 0x00, appURLHeader2, appURLHeader3}, bytes.Repeat([]byte{0x11}, 5)...)
	if _, err := decryptAppURL(base64.StdEncoding.EncodeToString(short), appTestSeed); !errors.Is(err, plugin.ErrSchema) {
		t.Fatalf("无完整分组：err = %v, 期望 ErrSchema", err)
	}
}

// TestDomainAllowlist 覆盖 AC：只有 https + 无 userinfo + 443 + 白名单（含子域）放行。
func TestDomainAllowlist(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"白名单根域", "https://fqnovelvod.com/a.mp4", true},
		{"白名单子域带签名查询串", appTestMediaURL, true},
		{"另一个白名单子域", "https://v5-x.douyinvod.com/a/b.mp4", true},
		{"显式 443", "https://v3-web.qznovelvod.com:443/a.mp4", true},

		{"http", "http://v3-web.fqnovelvod.com/a.mp4", false},
		{"后缀伪装", "https://fqnovelvod.com.evil.example/a.mp4", false},
		{"不在白名单", "https://evil.example/a.mp4", false},
		{"名字相近但不同域", "https://notqznovelvod.com/a.mp4", false},
		{"带 userinfo", "https://u:p@v3-web.fqnovelvod.com/a.mp4", false},
		{"非 443 端口", "https://v3-web.fqnovelvod.com:8443/a.mp4", false},
		{"空值", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := checkMediaURL(tc.raw)
			if tc.want {
				if err != nil {
					t.Fatalf("期望放行，err = %v", err)
				}
				if got != tc.raw {
					t.Fatalf("放行后地址 = %q, 期望原样 %q", got, tc.raw)
				}
				return
			}
			if !errors.Is(err, plugin.ErrSchema) {
				t.Fatalf("err = %v, 期望 ErrSchema", err)
			}
			// 拒绝时不得把地址或域名写进错误文本（没通过白名单的域名不该进日志）。
			for _, leak := range []string{"evil", "fqnovelvod", "qznovelvod", "douyinvod"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("错误文本泄露了域名 %q：%v", leak, err)
				}
			}
		})
	}
}

// TestResolvePrecedence 覆盖 AC：取址顺序 web(非预览) → app，且预览永不被当片源。
func TestResolvePrecedence(t *testing.T) {
	spade := buildSpadeA(t, appTestKey)
	appMedia := "https://v3-web.fqnovelvod.com/video/full/360p.mp4?sig=1"
	webMedia := DefaultBaseURL + "/video/public/" + fixtureVID + "/360p.mp4"

	newPlugin := func(t *testing.T, routes map[string]route, video func(int) (int, string)) (*Plugin, *fakeSite, *fakeApp) {
		t.Helper()
		web, webSrv := newFakeSite(t, http.StatusOK, "")
		web.setRoutes(routes)
		app, appSrv := newFakeApp(t)
		app.replyVideo(video)
		return newAppTestPlugin(t, webSrv, appSrv, slog.New(slog.NewTextHandler(io.Discard, nil))), web, app
	}

	okVideo := func(int) (int, string) {
		return http.StatusOK, appResponse(t, fixtureVID, appModelPlainFixture(map[string]any{
			"360p": appRow("h264", appMedia, spade, 640, 360, 640_000),
		}))
	}

	t.Run("受保护剧走 App，不返回 10 秒预览", func(t *testing.T) {
		p, web, app := newPlugin(t, map[string]route{
			"/player/" + fixtureSID:                    {http.StatusOK, readFixture(t, "page-series-protected.html")},
			"/player/" + fixtureSID + "/" + fixtureVID: {http.StatusOK, readFixture(t, "page-episode.html")},
		}, okVideo)
		st, err := p.Resolve(context.Background(), testRef(), 1)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if st.URL == webMedia {
			t.Fatalf("把网页的 10 秒预览当成了片源：%q", st.URL)
		}
		if st.URL != appMedia || len(st.Key) != appKeyLen {
			t.Fatalf("直链 = %q / key 长度 %d, 期望 App 片源 + 16 字节 key", st.URL, len(st.Key))
		}
		if reqs := web.requests(); len(reqs) != 1 {
			t.Fatalf("网页请求 = %v, 受保护剧不该请求集页", reqs)
		}
		if _, videos := app.count(); videos != 1 {
			t.Fatalf("App 取流 = %d 次, 期望 1 次", videos)
		}
	})

	t.Run("网页有真直链就优先用，不发 App 请求", func(t *testing.T) {
		p, _, app := newPlugin(t, map[string]route{
			"/player/" + fixtureSID:                    {http.StatusOK, readFixture(t, "page-series.html")},
			"/player/" + fixtureSID + "/" + fixtureVID: {http.StatusOK, readFixture(t, "page-episode.html")},
		}, okVideo)
		st, err := p.Resolve(context.Background(), testRef(), 1)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if st.URL != webMedia || st.Key != nil {
			t.Fatalf("直链 = %q / key = %x, 期望网页明文直链", st.URL, st.Key)
		}
		if registers, videos := app.count(); registers != 0 || videos != 0 {
			t.Fatalf("App 请求 = 注册 %d / 取流 %d, 网页通道成功时不该发", registers, videos)
		}
	})

	t.Run("网页无直链（集页 404）时落到 App", func(t *testing.T) {
		p, _, app := newPlugin(t, map[string]route{
			"/player/" + fixtureSID:                    {http.StatusOK, readFixture(t, "page-series.html")},
			"/player/" + fixtureSID + "/" + fixtureVID: {http.StatusNotFound, ""},
		}, okVideo)
		st, err := p.Resolve(context.Background(), testRef(), 1)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if st.URL != appMedia || len(st.Key) != appKeyLen {
			t.Fatalf("直链 = %q / key 长度 %d, 期望 App 片源", st.URL, len(st.Key))
		}
		if _, videos := app.count(); videos != 1 {
			t.Fatalf("App 取流 = %d 次, 期望 1 次", videos)
		}
	})

	t.Run("受保护剧 App 被拒时按错误上抛，不回落预览", func(t *testing.T) {
		p, _, _ := newPlugin(t, map[string]route{
			"/player/" + fixtureSID:                    {http.StatusOK, readFixture(t, "page-series-protected.html")},
			"/player/" + fixtureSID + "/" + fixtureVID: {http.StatusOK, readFixture(t, "page-episode.html")},
		}, func(int) (int, string) { return http.StatusForbidden, "" })
		st, err := p.Resolve(context.Background(), testRef(), 1)
		if !errors.Is(err, plugin.ErrRejected) {
			t.Fatalf("err = %v, 期望 ErrRejected（风控被拒 → 502）", err)
		}
		if st.URL != "" {
			t.Fatalf("失败时仍返回了直链 %q（不许回落预览）", st.URL)
		}
	})
}

// TestAppResolveRejectsNonWhitelisted 覆盖 AC：媒体地址不在白名单 → ErrSchema 且没有 Stream。
func TestAppResolveRejectsNonWhitelisted(t *testing.T) {
	model, err := parseAppModel(json.RawMessage(mustJSON(t, appModelPlainFixture(map[string]any{
		"360p": appRow("h264", "https://evil.example/a.mp4", buildSpadeA(t, appTestKey), 640, 360, 640_000),
	}))))
	if err != nil {
		t.Fatalf("解析夹具模型: %v", err)
	}
	st, err := appStreamFromModel(model)
	if !errors.Is(err, plugin.ErrSchema) {
		t.Fatalf("err = %v, 期望 ErrSchema", err)
	}
	if st.URL != "" || st.Key != nil {
		t.Fatalf("非白名单地址仍产出了 Stream：%+v", st)
	}
}

// mustJSON 把夹具结构序列化成 JSON 文本。
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化夹具: %v", err)
	}
	return b
}

// TestAppResolveErrorHasNoSecrets 覆盖 626：错误文本不带请求 URL 与设备身份
// （错误文本会进日志，也会被播放面写进 502 响应体）。
func TestAppResolveErrorHasNoSecrets(t *testing.T) {
	_, webSrv := newFakeSite(t, http.StatusOK, readFixture(t, "page-series-protected.html"))
	_, appSrv := newFakeApp(t)
	p := newAppTestPlugin(t, webSrv, appSrv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// 取流端点指向不可达端口：走 transport 错误那条分支。
	p.appVideoURL = "http://127.0.0.1:1/video"

	_, err := p.Resolve(context.Background(), testRef(), 1)
	if !errors.Is(err, plugin.ErrUpstream) {
		t.Fatalf("err = %v, 期望 ErrUpstream", err)
	}
	for _, leak := range []string{"127.0.0.1", "device_id", "iid", "cdid", "1001", "2001"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误文本泄露了 %q：%v", leak, err)
		}
	}
}

// assertNotLogged 断言给定文本里不出现任何敏感值（密钥、seed、地址查询串）。
func assertNotLogged(t *testing.T, logs string, secrets map[string]string) {
	t.Helper()
	for name, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(logs, secret) {
			t.Errorf("日志里出现了%s：%q", name, secret)
		}
	}
}

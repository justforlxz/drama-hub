package hongguo

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// 本文件的夹具全部自建（626 D3）：按 device_envelope.go 里写的信封布局，用
// 标准库现场造明文与信封，再用测试侧独立写出的解码步骤反解断言。不从参考实现
// 拷任何字节、常量表或期望值。

const deviceTestOKBody = `{"code":0,"device_id":1001,"install_id":2002,` +
	`"device_id_str":"1001","install_id_str":"2002"}`

// fakeClock 是可推进的时钟：60 秒静默期靠推进它来验证，不 sleep（测试规范禁止
// 依赖真实时间）。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// logSink 收集插件日志文本，供「身份不进日志」断言。
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// newDevicePlugin 构造一个只打设备注册端点的插件：页面通道地址指向不可达端口，
// 任何误走页面通道的调用都会立刻失败而不是静默通过。
func newDevicePlugin(t *testing.T, registerURL string, clock *fakeClock) (*Plugin, *logSink) {
	t.Helper()
	logs := &logSink{}
	p := New(Config{
		BaseURL:           "http://127.0.0.1:1",
		Logger:            slog.New(slog.NewTextHandler(logs, nil)),
		DeviceRegisterURL: registerURL,
	})
	p.device.now = clock.Now
	// 固定随机源：同一进程里两次构造的请求体可复现，便于断言。
	p.device.rand = rand.New(rand.NewSource(1))
	return p, logs
}

// TestDeviceIdentitySingleSlot：同一进程内两次取身份只发一次注册请求，且拿到的
// 是同一条身份；身份不出现在日志里。
func TestDeviceIdentitySingleSlot(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, deviceTestOKBody)
	clock := newFakeClock()
	p, logs := newDevicePlugin(t, srv.URL, clock)

	first, err := p.deviceIdentity(context.Background())
	if err != nil {
		t.Fatalf("第一次取身份: %v", err)
	}
	second, err := p.deviceIdentity(context.Background())
	if err != nil {
		t.Fatalf("第二次取身份: %v", err)
	}

	if reqs := fake.requests(); len(reqs) != 1 {
		t.Fatalf("注册请求 %d 次, 期望 1 次（同一进程只注册一次）：%v", len(reqs), reqs)
	}
	if first != second {
		t.Fatalf("两次取到不同身份：%+v / %+v", first, second)
	}
	if first.DeviceID != "1001" || first.IID != "2002" {
		t.Fatalf("身份 = %+v, 期望 device_id=1001 install_id=2002", first)
	}
	if first.CDID == "" {
		t.Fatalf("cdid 为空：它既是身份的一部分，也要写进信封 header")
	}

	// 请求头是协议的一部分：信封以二进制体发出，x-ss-stub 是整包 MD5（大写十六进制）。
	headers := fake.headerList()
	if len(headers) != 1 {
		t.Fatalf("请求头记录 %d 条, 期望 1 条", len(headers))
	}
	if ct := headers[0].Get("Content-Type"); ct != "application/octet-stream;tt-data=a" {
		t.Fatalf("Content-Type = %q, 期望 application/octet-stream;tt-data=a", ct)
	}
	if stub := headers[0].Get("x-ss-stub"); !isUpperHex(stub, 32) {
		t.Fatalf("x-ss-stub = %q, 期望 32 位大写十六进制（整包 MD5）", stub)
	}

	assertIdentityNotLogged(t, logs.text(), first)
}

// TestDeviceIdentityConcurrentFirstUse：并发首次取用也只能发一次注册（single-flight）。
func TestDeviceIdentityConcurrentFirstUse(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, deviceTestOKBody)
	clock := newFakeClock()
	p, logs := newDevicePlugin(t, srv.URL, clock)

	const callers = 20
	ids := make([]deviceIdentity, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = p.deviceIdentity(context.Background())
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("并发第 %d 个调用失败: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("并发第 %d 个拿到不同身份：%+v vs %+v", i, ids[i], ids[0])
		}
	}
	if reqs := fake.requests(); len(reqs) != 1 {
		t.Fatalf("并发首次取用发了 %d 次注册, 期望 1 次：%v", len(reqs), reqs)
	}
	assertIdentityNotLogged(t, logs.text(), ids[0])
}

// TestDeviceIdentityFailureCached：失败也缓存，60 秒内不重试；静默期过后才允许
// 下一次尝试；下一次成功后回到成功态。
func TestDeviceIdentityFailureCached(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusInternalServerError, "")
	clock := newFakeClock()
	p, logs := newDevicePlugin(t, srv.URL, clock)

	_, err := p.deviceIdentity(context.Background())
	if !errors.Is(err, plugin.ErrRejected) {
		t.Fatalf("注册失败的 err = %v, 期望 plugin.ErrRejected（调用方据此判定无直链）", err)
	}
	if reqs := fake.requests(); len(reqs) != 1 {
		t.Fatalf("首次失败后注册请求 = %d 次, 期望 1 次", len(reqs))
	}

	// 静默期内（59 秒）不再打扰上游，且回放的仍是同一条错误。
	clock.Advance(59 * time.Second)
	if _, err := p.deviceIdentity(context.Background()); !errors.Is(err, plugin.ErrRejected) {
		t.Fatalf("静默期内的 err = %v, 期望仍是 plugin.ErrRejected", err)
	}
	if reqs := fake.requests(); len(reqs) != 1 {
		t.Fatalf("静默期内又发了注册请求（共 %d 次），期望仍是 1 次", len(reqs))
	}

	// 过了静默期才允许重试。
	clock.Advance(2 * time.Second)
	if _, err := p.deviceIdentity(context.Background()); !errors.Is(err, plugin.ErrRejected) {
		t.Fatalf("静默期后的 err = %v, 期望 plugin.ErrRejected", err)
	}
	if reqs := fake.requests(); len(reqs) != 2 {
		t.Fatalf("静默期后注册请求 = %d 次, 期望 2 次（每 60 秒最多一次）", len(reqs))
	}

	// 上游恢复：下一次尝试成功，此后回放成功身份，不再注册。
	fake.setReply(http.StatusOK, deviceTestOKBody)
	clock.Advance(61 * time.Second)
	id, err := p.deviceIdentity(context.Background())
	if err != nil {
		t.Fatalf("上游恢复后取身份: %v", err)
	}
	if id.DeviceID != "1001" || id.IID != "2002" {
		t.Fatalf("身份 = %+v, 期望 device_id=1001 install_id=2002", id)
	}
	if _, err := p.deviceIdentity(context.Background()); err != nil {
		t.Fatalf("成功后再取身份: %v", err)
	}
	if reqs := fake.requests(); len(reqs) != 3 {
		t.Fatalf("注册请求总数 = %d, 期望 3（两次失败 + 一次成功，成功后不再注册）", len(reqs))
	}
	assertIdentityNotLogged(t, logs.text(), id)
}

// TestDeviceIdentityRenewalBounded：身份被上游拒后可以换一条，但续期 ≤ 1 次/分钟；
// 已被换掉的旧身份再报拒，直接回当前身份，不重复注册。
func TestDeviceIdentityRenewalBounded(t *testing.T) {
	fake, srv := newFakeSite(t, http.StatusOK, `{"code":0,"device_id":1001,"install_id":2002}`)
	clock := newFakeClock()
	p, _ := newDevicePlugin(t, srv.URL, clock)
	ctx := context.Background()

	first, err := p.deviceIdentity(ctx)
	if err != nil {
		t.Fatalf("首次取身份: %v", err)
	}

	// 上游拒了 first：换一条新的（一次续期 = 一次注册）。
	fake.setReply(http.StatusOK, `{"code":0,"device_id":3003,"install_id":2002}`)
	second, err := p.renewDeviceIdentity(ctx, first)
	if err != nil {
		t.Fatalf("续期: %v", err)
	}
	if second.DeviceID != "3003" || second == first {
		t.Fatalf("续期后的身份 = %+v, 期望新的一条（不是 %+v）", second, first)
	}
	if reqs := fake.requests(); len(reqs) != 2 {
		t.Fatalf("注册请求 = %d 次, 期望 2 次（首次 + 一次续期）", len(reqs))
	}

	// 同一分钟内又被拒：不注册，回可判定的错误。
	if _, err := p.renewDeviceIdentity(ctx, second); !errors.Is(err, plugin.ErrRejected) {
		t.Fatalf("静默期内续期的 err = %v, 期望 plugin.ErrRejected", err)
	}
	if reqs := fake.requests(); len(reqs) != 2 {
		t.Fatalf("静默期内又发了注册请求（共 %d 次），期望仍是 2 次", len(reqs))
	}

	// 过了静默期才允许再换一条。
	fake.setReply(http.StatusOK, `{"code":0,"device_id":4004,"install_id":2002}`)
	clock.Advance(61 * time.Second)
	third, err := p.renewDeviceIdentity(ctx, second)
	if err != nil {
		t.Fatalf("静默期后续期: %v", err)
	}
	if third.DeviceID != "4004" {
		t.Fatalf("静默期后续期的身份 = %+v, 期望 4004", third)
	}

	// 被拒的是已经换掉的旧身份：直接回当前身份，不注册。
	stale, err := p.renewDeviceIdentity(ctx, first)
	if err != nil {
		t.Fatalf("旧身份续期: %v", err)
	}
	if stale != third {
		t.Fatalf("旧身份续期拿到 %+v, 期望当前身份 %+v", stale, third)
	}
	if reqs := fake.requests(); len(reqs) != 3 {
		t.Fatalf("注册请求 = %d 次, 期望 3 次", len(reqs))
	}
}

// TestDeviceIdentityRejectsBadResponse：结构不符、业务拒绝、标识缺失都回
// plugin.ErrRejected，不返回空身份让上层误判为成功。
func TestDeviceIdentityRejectsBadResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"业务拒绝", `{"code":10010,"device_id":1001,"install_id":2002}`},
		{"缺 install_id", `{"code":0,"device_id":1001}`},
		{"标识全零", `{"code":0,"device_id":0,"install_id":0}`},
		{"不是 JSON", `<html>风控页</html>`},
		{"空响应", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newFakeSite(t, http.StatusOK, tc.body)
			p, _ := newDevicePlugin(t, srv.URL, newFakeClock())
			if _, err := p.deviceIdentity(context.Background()); !errors.Is(err, plugin.ErrRejected) {
				t.Fatalf("err = %v, 期望 plugin.ErrRejected", err)
			}
		})
	}
}

// TestDeviceEnvelopeLayout：信封的字节布局与协议一致——magic(6) | seed(32) |
// AES-128-CBC(sha512(gz) | gz)，key/iv 由 sha512(sha512(seed)||salt) 派生。
// 这里用测试侧独立写出的解码步骤反解，不复用被测函数的中间结果。
func TestDeviceEnvelopeLayout(t *testing.T) {
	plaintext := []byte(`{"magic_tag":"ss_app_log","header":{"aid":8662},"_gen_time":1700000000000}`)
	envelope, err := deviceEnvelope(plaintext, bytes.NewReader(seedBytes(0xA5)))
	if err != nil {
		t.Fatalf("构造信封: %v", err)
	}

	if got := decryptDeviceEnvelope(t, envelope); !bytes.Equal(got, plaintext) {
		t.Fatalf("解出的明文 = %s, 期望 %s", got, plaintext)
	}
	// stub 是整包的 MD5，跟着包体走：换包体就换 stub。
	if stub := deviceStub(envelope); !isUpperHex(stub, 32) {
		t.Fatalf("deviceStub = %q, 期望 32 位大写十六进制", stub)
	}
}

// TestDeviceEnvelopeDeterministic：同一明文 + 同一种子 → 同一信封（夹具自建的
// 自断言基础：信封里没有时间戳、没有隐式随机）。
func TestDeviceEnvelopeDeterministic(t *testing.T) {
	plaintext := []byte("同一个明文")
	first, err := deviceEnvelope(plaintext, bytes.NewReader(seedBytes(0x11)))
	if err != nil {
		t.Fatalf("第一次构造信封: %v", err)
	}
	second, err := deviceEnvelope(plaintext, bytes.NewReader(seedBytes(0x11)))
	if err != nil {
		t.Fatalf("第二次构造信封: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("同一明文 + 同一种子得到不同信封：%x vs %x", first, second)
	}

	other, err := deviceEnvelope([]byte("另一个明文"), bytes.NewReader(seedBytes(0x11)))
	if err != nil {
		t.Fatalf("构造信封: %v", err)
	}
	if bytes.Equal(first, other) {
		t.Fatalf("不同明文得到同一个信封，说明明文没有进信封")
	}
}

// TestDeviceRegisterBody：注册请求体是信封包的 JSON 设备参数，不是裸 JSON。
func TestDeviceRegisterBody(t *testing.T) {
	const nowMillis = 1_700_000_000_000
	body, err := buildDeviceRegisterBody(bytes.NewReader(seedBytes(0x3C)), "cdid-under-test", nowMillis)
	if err != nil {
		t.Fatalf("构造注册请求体: %v", err)
	}
	var got deviceRegisterEnvelope
	if err := json.Unmarshal(decryptDeviceEnvelope(t, body), &got); err != nil {
		t.Fatalf("解出的明文不是注册信封 JSON: %v", err)
	}
	if got.MagicTag != "ss_app_log" {
		t.Fatalf("magic_tag = %q, 期望 ss_app_log", got.MagicTag)
	}
	if got.GenTime != nowMillis {
		t.Fatalf("_gen_time = %d, 期望 %d", got.GenTime, nowMillis)
	}
	h := got.Header
	if h.Aid != deviceAid || h.DevicePlatform != "android" || h.Package != "com.phoenix.read" {
		t.Fatalf("header 设备参数不符：aid=%d platform=%q package=%q", h.Aid, h.DevicePlatform, h.Package)
	}
	if h.Cdid != "cdid-under-test" {
		t.Fatalf("header.cdid = %q, 期望与返回的身份同一个 cdid", h.Cdid)
	}
	if h.Openudid == "" || h.Clientudid == "" || h.ReqID == "" {
		t.Fatalf("header 少了随机标识：openudid=%q clientudid=%q req_id=%q", h.Openudid, h.Clientudid, h.ReqID)
	}
	if h.IPv6List == nil || h.SimSerialNumber == nil {
		t.Fatalf("ipv6_list / sim_serial_number 必须是空数组而不是 null（上游按数组解）")
	}
	if h.ApkFirstInstallTime != nowMillis {
		t.Fatalf("apk_first_install_time = %d, 期望 %d", h.ApkFirstInstallTime, nowMillis)
	}

	again, err := buildDeviceRegisterBody(bytes.NewReader(seedBytes(0x3C)), "cdid-under-test", nowMillis)
	if err != nil {
		t.Fatalf("第二次构造注册请求体: %v", err)
	}
	if !bytes.Equal(body, again) {
		t.Fatalf("同一输入得到不同请求体：不是确定的信封")
	}
}

// TestParseDeviceRegisterResponse：标识的三种形态（数字 / 字符串 / *_str 优先）
// 与无效形态各断言一条。
func TestParseDeviceRegisterResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want deviceIdentity
	}{
		{"数字", `{"code":0,"device_id":1001,"install_id":2002}`, deviceIdentity{DeviceID: "1001", IID: "2002"}},
		{"字符串", `{"device_id":"1001","install_id":"2002"}`, deviceIdentity{DeviceID: "1001", IID: "2002"}},
		{"_str 优先", `{"device_id":1,"device_id_str":"7","install_id":2,"install_id_str":"9"}`, deviceIdentity{DeviceID: "7", IID: "9"}},
		{"_str 为空退回数字", `{"device_id":1,"device_id_str":"","install_id":2}`, deviceIdentity{DeviceID: "1", IID: "2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDeviceRegisterResponse([]byte(tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tc.want {
				t.Fatalf("身份 = %+v, 期望 %+v", got, tc.want)
			}
		})
	}
}

// --- 测试辅助 ---

// seedBytes 返回 n 个相同的字节，用作确定的随机源。
func seedBytes(b byte) []byte {
	return bytes.Repeat([]byte{b}, deviceSeedLen*4)
}

// isUpperHex 判断 s 是 n 位大写十六进制。
func isUpperHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// assertIdentityNotLogged：身份（device_id / install_id / cdid）不得出现在日志里
// （AC：身份不出现在任何日志）。
func assertIdentityNotLogged(t *testing.T, logs string, id deviceIdentity) {
	t.Helper()
	if logs == "" {
		t.Fatalf("没有采集到任何日志：至少应有一条设备注册记录")
	}
	for name, value := range map[string]string{"device_id": id.DeviceID, "install_id": id.IID, "cdid": id.CDID} {
		if value == "" {
			continue
		}
		if strings.Contains(logs, value) {
			t.Fatalf("日志里出现了 %s=%s：\n%s", name, value, logs)
		}
	}
}

// decryptDeviceEnvelope 是测试侧独立写出的解码器：按 device_envelope.go 顶部
// 写明的布局逐步反解（magic → seed → AES-128-CBC → PKCS#7 → sha512 前缀 → gzip）。
func decryptDeviceEnvelope(t *testing.T, envelope []byte) []byte {
	t.Helper()
	const headerLen = 6 + deviceSeedLen
	if len(envelope) <= headerLen+aes.BlockSize {
		t.Fatalf("信封太短：%d 字节", len(envelope))
	}
	if !bytes.Equal(envelope[:6], mustHex(deviceMagicHex)) {
		t.Fatalf("信封前缀 = %x, 期望协议 magic %s", envelope[:6], deviceMagicHex)
	}
	seed := envelope[6:headerLen]
	ciphertext := envelope[headerLen:]
	if len(ciphertext)%aes.BlockSize != 0 {
		t.Fatalf("密文长度 %d 不是 %d 的整数倍", len(ciphertext), aes.BlockSize)
	}

	inner := sha512.Sum512(seed)
	h := sha512.New()
	h.Write(inner[:])
	h.Write(mustHex(deviceKDFSaltHex))
	derived := h.Sum(nil)
	block, err := aes.NewCipher(derived[:16])
	if err != nil {
		t.Fatalf("构造测试侧 AES: %v", err)
	}
	padded := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, derived[16:32]).CryptBlocks(padded, ciphertext)

	pad := int(padded[len(padded)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(padded) {
		t.Fatalf("PKCS#7 补位非法：%d", pad)
	}
	plain := padded[:len(padded)-pad]
	if len(plain) <= deviceDigestLen {
		t.Fatalf("解出的明文太短：%d 字节", len(plain))
	}
	digest, compressed := plain[:deviceDigestLen], plain[deviceDigestLen:]
	if sum := sha512.Sum512(compressed); !bytes.Equal(sum[:], digest) {
		t.Fatalf("明文前缀不是压缩体的 sha512：信封布局不符")
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("压缩体不是 gzip: %v", err)
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("解压: %v", err)
	}
	return out
}

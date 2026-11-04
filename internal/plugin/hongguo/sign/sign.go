// SPDX-License-Identifier: GPL-3.0-only
// Signing algorithms adapted from woshishiq1/drpys
// commit 22261adfa31435e3b3ef5a730b8a2a75bcaf1715, spider/js/红果果[短].js,
// via N3urda/hongguoTV eea73e6a45569a2cbbb9440f7cd5290c459b0b70,
// ported to Go from jackotom/hongguo-drama src/native/app-source.js.

// Package sign 实现红果 App 取流接口的字节系请求签名（MKACG-676）。
//
// 为什么需要它：App 端点（api*.fqnovel.com/novel/player/multi_video_model/v1/）
// 被网关按请求签名校验，缺签名时回 200 + 空体（不是 4xx，也不是 401）。签名覆盖
// **query 字节 + body 字节**，因此调用方必须把「签名时用的 body」原样发出、把
// 「签名时用的 query」原样放进 URL——序列化后重新 marshal 会让签名失效。
//
// 约束：
//   - 只依赖标准库（插件包的地盘，不引第三方）；
//   - 随机量通过 Signer.Rand 注入，测试才能拿到固定向量；
//   - 本包不做日志、不落盘，签名材料由调用方按敏感面处理。
package sign

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	mrand "math/rand"
	"strings"
	"sync"
	"time"
)

// ErrBranchTimeout 表示 128 次 ticket 重试都没落进签名分支 0。
// 这是「上游改了签名策略」的信号，调用方必须显式失败，不得静默回退匿名路径。
var ErrBranchTimeout = errors.New("无法生成播放请求签名")

// branchAttempts 是 ticket 重试上限（参考实现的值；命中率约 1/3，128 次失败
// 只有在上游改变算法时才会发生）。
const branchAttempts = 128

// RandSource 是签名里所有随机量的来源。生产用 math/rand 的进程实例（crypto 播种），
// 测试注入固定序列即可拿到逐字节可复现的输出。
//
// 暴露的必须是 [0,1) 浮点而不是 32 位整数：参考实现里同一个随机数有时按
// floor(x*2^32) 用、有时按 floor(x*21)、floor(x*65536) 用，换算方式不同。
type RandSource interface {
	Float64() float64
}

// Identity 是参与签名的设备身份（匿名注册得到）。
type Identity struct {
	DeviceID    string
	IID         string
	CDID        string
	VersionName string
}

// KV 是有序查询参数：签名对顺序敏感，所以用切片而不是 map。
type KV struct {
	Key   string
	Value string
}

// Signer 持有进程级签名材料（参考实现里是模块级常量）。
type Signer struct {
	Rand    RandSource
	Nonce   string // 18 字节随机数的 base64url，进程内固定
	Started int64  // 进程启动毫秒时间戳，进程内固定
	Now     func() time.Time

	// branch 是分支判定，默认 branchOf。只有测试会替换它：重试与超限失败
	// 无法靠构造输入稳定命中（散列不可控），所以留这一个窄缝。
	branch func(url string, body []byte, khronos uint32) int

	// mu 串行化整个 Playback：Signer 是进程级单例，并发播放请求共用它，而
	// Rand 默认是一条 *math/rand.Rand —— 标准库明确不可多 goroutine 共用。
	// 锁在入口一次拿全，签名过程中的随机量（gorgon random、helios、medusa
	// 的消息随机段）因此都落在同一把锁内。
	mu sync.Mutex
}

func (s *Signer) branchOf(url string, body []byte, khronos uint32) int {
	if s.branch != nil {
		return s.branch(url, body, khronos)
	}
	return branchOf(url, body, khronos)
}

// NewSigner 用 crypto/rand 播种的随机源与进程级 nonce 构造签名器。
func NewSigner() *Signer {
	seed, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		// 熵源不可用时退化为时间播种：签名仍可用，只是随机性变差。
		seed = big.NewInt(time.Now().UnixNano())
	}
	r := mrand.New(mrand.NewSource(seed.Int64()))
	nonce := make([]byte, 18)
	if _, err := rand.Read(nonce); err != nil {
		copy(nonce, []byte(fmt.Sprintf("%018d", time.Now().UnixNano())))
	}
	return &Signer{
		Rand:    r,
		Nonce:   base64.RawURLEncoding.EncodeToString(nonce),
		Started: time.Now().UnixMilli(),
		Now:     time.Now,
	}
}

// rand32 等价于参考实现的 Math.floor(Math.random() * 0x100000000)。
func (s *Signer) rand32() uint32 { return uint32(s.Rand.Float64() * 4294967296) }

// randInt 等价于参考实现的 Math.floor(Math.random() * n)。
func (s *Signer) randInt(n int) int64 { return int64(s.Rand.Float64() * float64(n)) }

func (s *Signer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Request 是一次已签名请求的最终形态：URL 与请求头必须原样使用。
type Request struct {
	URL     string
	Headers map[string]string
}

// Playback 生成 App 取流请求：查询串（设备参数 + 身份 + ts + _rticket）与 body
// 字节参与签名，ticket 递增直到散列落进分支 0。
//
// body 必须是即将发出的那串字节：调用方先 marshal 一次，签名与发送共用同一份。
func (s *Signer) Playback(baseURL string, deviceParams []KV, ident Identity, body []byte, userAgent string) (Request, error) {
	// 一把锁覆盖整次签名：随机源与派生随机量只有这一处使用，锁粒度取整个调用
	// （互斥区间是微秒级，不构成瓶颈），并发安全由这一处保证。
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	khronos := uint32(now.Unix())
	ticketBase := now.UnixMilli()

	params := make([]KV, 0, len(deviceParams)+6)
	params = append(params, deviceParams...)
	params = append(params,
		KV{"device_id", ident.DeviceID},
		KV{"iid", ident.IID},
		KV{"cdid", ident.CDID},
		KV{"ts", fmt.Sprintf("%d", khronos)},
	)

	base := strings.TrimRight(baseURL, "?&")
	var urlStr, query string
	var ticket int64
	for n := 0; n < branchAttempts; n++ {
		ticket = ticketBase + int64(n)
		withTicket := append(append([]KV{}, params...), KV{"_rticket", fmt.Sprintf("%d", ticket)})
		query = encodeQuery(withTicket)
		urlStr = base + "?" + query
		if s.branchOf(urlStr, body, khronos) == 0 {
			break
		}
		if n == branchAttempts-1 {
			return Request{}, fmt.Errorf("%w（%d 次 ticket 均未落入分支 0）", ErrBranchTimeout, branchAttempts)
		}
	}

	random := uint16(s.randInt(65536))
	headers := map[string]string{
		"User-Agent":            userAgent,
		"Accept":                "application/json; charset=utf-8,application/x-protobuf",
		"Content-Type":          "application/json; charset=UTF-8",
		"x-ss-req-ticket":       fmt.Sprintf("%d", ticket),
		"sdk-version":           "2",
		"x-xs-from-web":         "0",
		"x-khronos":             fmt.Sprintf("%d", khronos),
		"x-gorgon":              xGorgon(query, body, khronos, random),
		"x-ladon":               base64.StdEncoding.EncodeToString(be32(khronos)),
		"x-argus":               base64.StdEncoding.EncodeToString(le32(khronos)),
		"x-helios":              s.helios(khronos),
		"x-medusa":              s.medusa(urlStr, body, khronos, ident),
		"x-tt-dt":               "",
		"x-tt-store-region":     "cn",
		"x-tt-store-region-src": "did",
	}
	return Request{URL: urlStr, Headers: headers}, nil
}

// encodeQuery 是 Python urllib.parse.quote / JS encodeURIComponent 的混合口径：
// 字母数字与 - _ . ~ * 原样，! ' ( ) 也走百分号（参考实现如此），空格编成 %20。
// 签名对编码逐字节敏感，别换成 url.Values.Encode()（它把空格写成 +，且会排序）。
func encodeQuery(params []KV) string {
	var b strings.Builder
	for i, p := range params {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(encodeComponent(p.Key))
		b.WriteByte('=')
		b.WriteString(encodeComponent(p.Value))
	}
	return b.String()
}

func encodeComponent(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '*':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	return b.String()
}

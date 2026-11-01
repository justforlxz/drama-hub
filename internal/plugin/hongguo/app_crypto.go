package hongguo

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/bits"
	"strings"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// App 通道的两段密码学（MKACG-630）：spade_a → 内容密钥、key_seed → 媒体地址。
//
// 两段都只用标准库，规则是协议事实（626 D3：只取算法事实、独立实现，不搬参考
// 实现的代码与夹具）。密钥与解出的地址只在内存里过一手：不进日志、不进错误文本。

// deriveAppContentKey 按 spade_a 的编码规则派生 16 字节内容密钥。
//
// 布局：base64 后的首 3 字节算出「有效段长度」，有效段内每字节按位序做一次可逆
// 变换，变换后的第 1..32 字节是密钥的十六进制文本。规则是协议事实（626 D3：
// 只取算法事实、独立实现，不搬参考实现的代码与夹具）。
func deriveAppContentKey(spadeA string) ([]byte, error) {
	raw, err := decodeBase64Loose(spadeA)
	if err != nil {
		return nil, fmt.Errorf("%w: spade_a 不是合法 base64", plugin.ErrSchema)
	}
	if len(raw) < 3 {
		return nil, fmt.Errorf("%w: spade_a 长度不足", plugin.ErrSchema)
	}
	span := len(raw) - int(raw[0]^raw[1]^raw[2]) + 47
	if span > len(raw)-1 {
		span = len(raw) - 1
	}
	if span < 1+appKeyHexLen {
		return nil, fmt.Errorf("%w: spade_a 未给出 %d 个密钥字节", plugin.ErrSchema, appKeyHexLen)
	}
	work := make([]byte, span)
	copy(work, raw[1:1+span])
	odd, even := byte(appSpadeInitOdd), byte(appSpadeInitEven)
	for i := 0; i < span; i++ {
		old := work[i]
		var previous byte
		if i&1 == 1 {
			previous, odd = odd, old
		} else {
			previous, even = even, old
		}
		work[i] = byte(-21 - bits.OnesCount(uint(i)) + int(previous^old))
	}
	key, err := hex.DecodeString(string(work[1 : 1+appKeyHexLen]))
	if err != nil {
		return nil, fmt.Errorf("%w: spade_a 里的密钥不是十六进制", plugin.ErrSchema)
	}
	return key, nil
}

// decryptAppURL 用 key_seed 把密文媒体地址解成明文地址（AES-128-CBC + PKCS#7 补位）。
//
// 派生：key = sha512(sha512(seed) || 白名单同款常量)[0:16]，iv = 同一摘要 [16:32]
// （与注册信封共用同一份 KDF 常量）。密文头固定 4 字节，之后是整分组的密文。
func decryptAppURL(encoded string, seed []byte) (string, error) {
	raw, err := decodeBase64Loose(encoded)
	if err != nil {
		return "", fmt.Errorf("%w: key_seed 密文不是合法 base64", plugin.ErrSchema)
	}
	if len(raw) < 5 || raw[0] != appURLHeader0 || raw[2] != appURLHeader2 || raw[3] != appURLHeader3 {
		return "", fmt.Errorf("%w: key_seed 密文头不符", plugin.ErrSchema)
	}
	body := raw[4:]
	body = body[:len(body)/aes.BlockSize*aes.BlockSize]
	if len(body) == 0 {
		return "", fmt.Errorf("%w: key_seed 密文不足一个分组", plugin.ErrSchema)
	}
	// 与注册信封共用同一份 KDF（device_envelope.go）：key 前 16 字节、iv 次 16 字节。
	key, iv := deviceEnvelopeKey(seed)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("%w: key_seed 派生密钥失败", plugin.ErrSchema)
	}
	plain := make([]byte, len(body))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, body)
	if pad := int(plain[len(plain)-1]); pad >= 1 && pad <= aes.BlockSize && pad <= len(plain) {
		plain = plain[:len(plain)-pad]
	}
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 {
		return "", fmt.Errorf("%w: key_seed 解出空地址", plugin.ErrSchema)
	}
	return string(plain), nil
}

// decodeBase64Loose 接受标准/URL 两种字母表、带或不带补位。
func decodeBase64Loose(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("空 base64")
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("非法 base64")
}

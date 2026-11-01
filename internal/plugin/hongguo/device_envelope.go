package hongguo

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
)

// 匿名设备注册的「信封」协议事实（MKACG-628，设计见 MKACG-626 §6 S15.2a）。
//
// 注册端点收的不是 JSON，而是一个自描述信封：
//
//	magic(6) || seed(32) || AES-128-CBC( sha512(gzip(明文)) || gzip(明文) )
//
// 其中 key = sha512( sha512(seed) || kdfSalt )[0:16]，iv = 同一摘要 [16:32]，
// 补位是 PKCS#7；请求头 x-ss-stub 是整包的 MD5（大写十六进制）。上游按同一
// 规则解开，所以 magic 与 kdfSalt 是互通必需的事实（改一个字节上游就解不开），
// 不是可调参数，也不进 settings。
//
// 许可证边界（626 D3）：参考实现是 GPL-3.0，这里只落「上游要什么」的协议事实，
// 参考仓的代码组织、设备常量表与夹具一概不搬运；夹具由 device_envelope_test.go
// 按上面的规则自建并自断言。
const (
	// deviceSeedLen 是信封里随机种子（KDF 输入）的长度。
	deviceSeedLen = 32
	// deviceDigestLen 是明文前缀的摘要长度：sha512。
	deviceDigestLen = sha512.Size
	// deviceMagicHex / deviceKDFSaltHex 是协议常量，见上面的信封布局。
	deviceMagicHex   = "746305100000"
	deviceKDFSaltHex = "4dd4c2e6b83162090e52b3c7a6733ba41cb2462b829ab58a196b39db57177524f49baf7f08e8d68d26a72e37c1a95a2f1f05a51892aef2949732b62a38aadd58"
)

var (
	deviceMagic   = mustHex(deviceMagicHex)
	deviceKDFSalt = mustHex(deviceKDFSaltHex)
)

// mustHex 解包包内常量：写错的常量是构建期的错误，不是运行时分支。
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("hongguo: 信封常量不是合法十六进制: " + err.Error())
	}
	return b
}

// deviceEnvelope 把注册明文封成上游要的信封，random 提供 32 字节种子。
//
// 同一份明文配同一份种子必然得到同一份字节（无时间戳、无隐式随机），这条性质
// 是 fixture 的断言基础，也是 register 只在构造明文时用时钟的原因。
func deviceEnvelope(plaintext []byte, random io.Reader) ([]byte, error) {
	seed := make([]byte, deviceSeedLen)
	if _, err := io.ReadFull(random, seed); err != nil {
		return nil, fmt.Errorf("读取信封种子: %w", err)
	}
	compressed, err := gzipBytes(plaintext)
	if err != nil {
		return nil, err
	}
	key, iv := deviceEnvelopeKey(seed)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("构造信封分组密码: %w", err)
	}
	digest := sha512.Sum512(compressed)
	plain := make([]byte, 0, deviceDigestLen+len(compressed))
	plain = append(plain, digest[:deviceDigestLen]...)
	plain = append(plain, compressed...)
	padded := pkcs7Pad(plain, block.BlockSize())
	body := make([]byte, 0, len(deviceMagic)+len(seed)+len(padded))
	body = append(body, deviceMagic...)
	body = append(body, seed...)
	body = append(body, make([]byte, len(padded))...)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(body[len(deviceMagic)+len(seed):], padded)
	return body, nil
}

// deviceEnvelopeKey 按协议派生 AES-128 的 key 与 IV：sha512(sha512(seed)||salt)，
// 前 16 字节做 key、紧接 16 字节做 IV。
func deviceEnvelopeKey(seed []byte) (key, iv []byte) {
	inner := sha512.Sum512(seed)
	h := sha512.New()
	h.Write(inner[:])
	h.Write(deviceKDFSalt)
	derived := h.Sum(nil)
	return derived[:16:16], derived[16:32:32]
}

// deviceStub 是整包的 MD5（大写十六进制），只作请求头的完整性校验，不是安全用途。
func deviceStub(body []byte) string {
	sum := md5.Sum(body)
	return fmt.Sprintf("%X", sum)
}

// gzipBytes 压缩明文。常量头的 ModTime 为零值，因此输出只由输入决定。
func gzipBytes(plaintext []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plaintext); err != nil {
		return nil, fmt.Errorf("压缩信封明文: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("压缩信封明文: %w", err)
	}
	return buf.Bytes(), nil
}

// pkcs7Pad 补位到 blockSize 的整数倍（AES-CBC 要求）。
func pkcs7Pad(b []byte, blockSize int) []byte {
	n := blockSize - len(b)%blockSize
	return append(b, bytes.Repeat([]byte{byte(n)}, n)...)
}

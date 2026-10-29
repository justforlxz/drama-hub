package stream

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// Sign 按 ADR-0003 计算 uid 的签名 token：
//
//	base64url_nopad( HMAC-SHA256(secret, "stream:"+uid) )
//
// 纯函数：无状态、无时钟、无 token 表。编码固定为 base64.RawURLEncoding
// （URL-safe、无 '='），产出 43 字符的路径段安全字符串；生产方（渲染 strm）
// 与校验方必须逐字节一致。
func Sign(secret []byte, uid string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("stream:" + uid))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify 判断 token 是否是 uid 的有效签名。
//
// 用 crypto/hmac.Equal 做常数时间比较，不按字节短路泄漏时序信息。任何不匹配
// （非 base64、长度不对、用别的 uid 或别的 secret 签的、篡改一位）都返回 false。
func Verify(secret []byte, uid, token string) bool {
	return hmac.Equal([]byte(Sign(secret, uid)), []byte(token))
}

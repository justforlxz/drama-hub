package stream

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// IDLen 是 id 里签名的十六进制字符数（12 个字符 = 6 字节）。
//
// 与 docs/PLAYBACK-CONTRACT.md 的示例 `e1234-9f3a1c02b7d4` 一致。
const IDLen = 12

// ID 按 docs/PLAYBACK-CONTRACT.md 生成对外播放 id：
//
//	id = uid + "-" + hex(HMAC-SHA256(stream_secret, uid))[:12]
//
// uid 取 emby_item.uid（单集 `e<episode.id>`、合并集 `m<merge_group.id>`）。
// 纯函数：只由 uid 与 secret 决定，因此同步、重建派生表、改目录、重启都不改变
// 它，缓存键不会漂（ADR-0003 的不变量）。换 stream_secret 后所有 id 失效。
//
// 生产方是 vfs 的 strm 渲染（经 cmd 注入），校验方是 `/video` 端点（S8），
// 两侧必须调用同一个函数，不能各写一份 HMAC 拼接。
func ID(secret, uid string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(uid))
	sum := hex.EncodeToString(mac.Sum(nil))
	return uid + "-" + sum[:IDLen]
}

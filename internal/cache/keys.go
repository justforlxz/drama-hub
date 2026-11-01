package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/justforlxz/drama-hub/internal/store"
)

// 缓存键的三种规则（真值见 docs/CACHE-MODEL.md「落盘布局与键」）。
//
// 键只用不含 secret 的稳定标识：分片与索引用 uid（`e<episode.id>` /
// `m<merge_group.id>`），海报用 `<provider>/<provider_id>`。所以换 stream_secret
// 只会让已发出的 URL 失效，不会让缓存失效（ADR-0007「后果」）。

// keySeparator 是分片键的分段符（\x1f 不会出现在 uid / 数字 / codec 签名里）。
const keySeparator = "\x1f"

// SegmentKey 是分片的 artifact 键，同时也是落盘文件名：
// sha256(uid \x1f seq \x1f codec_sig) 的十六进制（ADR-0007 第 3 条）。
//
// codec_sig 参与键：切片是 `-c copy`，不同编码参数的源若混进同一个文件就是坏数据。
//
// 传进来的 codec_sig 已经带通道段（`web/…` / `app/…`，由 internal/segment 按
// MKACG-626 裁决 3 拼）：同一集的网页源与 App 源编码参数可能完全相同、字节流不同，
// 通道段是键里唯一能区分它们的东西。本函数不认识通道，也不该认识 —— 判据只有一处
// （segment.channelSig），索引行与这里消费的是同一个字符串。
func SegmentKey(uid string, seq int, codecSig string) string {
	preimage := uid + keySeparator + strconv.Itoa(seq) + keySeparator + codecSig
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:])
}

// IndexKey 是关键帧索引的 artifact 键：uid（一集或一个合并组一份索引）。
func IndexKey(uid string) string { return uid }

// CoverKey 是海报的 artifact 键：`<provider>/<provider_id>`。
//
// S3（internal/cache/cover.go）已有实现，规则收在这里，避免两处各拼一遍。
func CoverKey(provider, providerID string) string { return provider + "/" + providerID }

// cacheHash 把 artifact 键映射成落盘用的十六进制哈希。
//
// 契约里 `<hash> = sha256(<键>)`：分片那条的 `<键>` 本身已经是 sha256(uid|seq|sig)，
// 直接用（再哈希一次就与文档的路径不符）；海报与索引的键是语义键，取 sha256(键)。
func cacheHash(kind, key string) string {
	if kind == store.ArtifactKindSegment {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

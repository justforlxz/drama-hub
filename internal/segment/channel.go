package segment

import (
	"strings"

	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
)

// 分片身份里的通道段（MKACG-626 裁决 3）。
//
// 同一集的网页源与 App 源字节流不同，但 codec_sig（codec / profile / level /
// pix_fmt / tag / time_base）可能完全相同：它不含分辨率与时长，10 秒的 360p 预览
// 与 3 分钟的完整集会算出同一个签名。签名不区分通道，`cache.SegmentKey =
// sha256(uid | seq | codec_sig)` 就会串味，`ensureSegments` 也会沿用另一个通道的
// 索引。
//
// 所以 codec_sig 一律写成 `<通道>/<探测出的签名>`：一处产出（ensureSegments 写
// segment.codec_sig），两处消费（索引行与 cache.SegmentKey）—— 分片键拿到签名就
// 拿到了判据，不必自己认识通道（CACHE-MODEL「键」）。
const (
	channelWeb = "web" // 网页通道：明文源
	channelApp = "app" // App 通道：CENC 密文源，ffmpeg 要 -decryption_key
)

// channelOf 是通道的唯一判据：带内容密钥的流是 App 通道，其余是网页通道。
//
// 判据只能是「有没有 key」：插件契约里没有通道名字段，新增一个就为了拼缓存键是
// 多余的概念；而字节流的分水岭恰好就是这个 —— 网页通道交付明文（受保护剧只给
// 10 秒预览，按 626 D1 直接 ErrNoStream，不当作直链交付），密文只可能来自 App
// 通道（docs/PLUGIN-CONTRACT.md「加密切片与 Key」）。
func channelOf(d stream.Direct) string {
	if len(d.Key) > 0 {
		return channelApp
	}
	return channelWeb
}

// channelSig 把通道段并进编码签名：`<通道>/<签名>`。
func channelSig(channel, codecSig string) string { return channel + "/" + codecSig }

// sigChannel 取签名里的通道段；加通道段之前写下的老索引返回空串（一律当失效）。
func sigChannel(codecSig string) string {
	ch, _, ok := strings.Cut(codecSig, "/")
	if !ok || (ch != channelWeb && ch != channelApp) {
		return ""
	}
	return ch
}

// indexStale 判定现存索引是否由别的通道建立（含没有通道段的老索引）：是则必须重建。
//
// 不重建的话「有行就直接返回」会把旧通道的索引永久沿用 —— 通道一换，playlist 的
// 片数、时长、分片键全错（626 裁决 3）。
func indexStale(segs []store.Segment, channel string) bool {
	for _, s := range segs {
		if sigChannel(s.CodecSig) != channel {
			return true
		}
	}
	return false
}

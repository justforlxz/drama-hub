// Package stream 负责播放面的两件事：对外 id 的 HMAC 签名与直链的解析与复用。
//
// 依赖方向：rank 20，可 import rank 10 的 internal 包（store、plugin）；
// rank 30 及以上的包（vfs / httpx / segment / webdav / web / cmd）可 import 本包。
// 规则见 docs/DEV-STANDARDS.md「代码组织」，由 scripts/check-deps.sh 强制。
//
// 边界（docs/PLAYBACK-CONTRACT.md、docs/adr/0016-episode-stream-cache.md）：
//
//   - id 的算法只有一个实现（`ID`），生产方是与 `ParseID` 的消费方共用它；
//   - 直链与解密 key 由 `Resolver` 向插件解析后连同 expires_at 落库
//     （episode_stream）：未过期就直接复用、零外呼，过期或被告知失效才重解析；
//     0004 删 episode.play_url 防的是「把很久以前存的 URL 当永久有效」，与本条
//     并存（ADR-0016）。URL 与 key 都不进日志与错误文本；
//   - 本包不碰 HTTP：状态码映射在边界包（internal/segment 的 `/video`）。
package stream

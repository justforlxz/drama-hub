// Package stream 负责播放面的两件事：对外 id 的 HMAC 签名与请求时的直链解析。
//
// 依赖方向：rank 20，可 import rank 10 的 internal 包（store、plugin）；
// rank 30 及以上的包（vfs / httpx / segment / webdav / web / cmd）可 import 本包。
// 规则见 docs/DEV-STANDARDS.md「代码组织」，由 scripts/check-deps.sh 强制。
//
// 边界（docs/PLAYBACK-CONTRACT.md）：
//
//   - id 的算法只有一个实现（`ID`），生产方是与 `ParseID` 的消费方共用它；
//   - 直链只在请求时由 `Resolver` 向插件解析，不落库、不缓存（0004 已删
//     episode.play_url / play_url_at）；
//   - 本包不碰 HTTP：状态码映射在边界包（internal/segment 的 `/video`）。
package stream

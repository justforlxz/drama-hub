// Package segment 负责播放面：每集一片的播放时间轴、播放列表生成、整集转封装
// 与分片缓存编排。
//
// 依赖方向：rank 40，可 import rank 30/20/10 的 internal 包（httpx、cache、stream、
// store）；与同级的 webdav 互不 import；rank 50/60 可 import 本包。规则见
// docs/DEV-STANDARDS.md「代码组织」，由 scripts/check-deps.sh 强制。
//
// 五件事（契约真值：docs/PLAYBACK-CONTRACT.md）：
//
//  1. 端点：`GET /video/{id}.m3u8`（`ServePlaylist`）与 `GET /video/{id}/{seq}.m4s`
//     （`ServeSegment`），单集与合并集同一路径形态。
//  2. 时长：`episode.duration_sec` 是唯一真值。第一次播放某个 uid 时 `probeDuration`
//     现 `ffprobe` 一次取源时长并回填（`store.BackfillEpisodeDuration`，只填 0），
//     下一次读即生效（ADR-0014：对外剧集不落库），`.nfo` 的 `<runtime>` 与 playlist
//     的 EXTINF 因此变成真实值。整体 30 s 硬上限，超时 503。
//  3. 时间轴映射：`Timeline` 把有序的成员集拼成连续 seq 空间。每集一片，所以
//     `seq` 就是成员下标（合并集播放的 O(1) 定位）。
//  4. 播放列表生成：`BuildPlaylist` 把时间轴渲成 VOD m3u8，集边界插
//     `#EXT-X-DISCONTINUITY`；playlist 是派生视图，不落盘、不缓存。
//  5. 分片：`remux` 把**整集**一次 `-c copy` 转成自包含的 fMP4（片头自带
//     `ftyp+moov`）；分片经 `internal/cache` 落盘（键 `sha256("e<episode.id>"\x1fchannel)`，
//     single-flight 合并并发生成）。每集只生成一次，与它出现在单集还是合并集无关。
//
// 边界：直链不在本包解析（`ResolveFunc` 由装配根注入 `stream.Resolver`），也不落库
// ——0004 已删 episode.play_url；分块（含合并集的现算）属 internal/store。
// 出站身份同理：ffmpeg / ffprobe 的 `-headers` 由装配根从插件常量注入（`NewCutter`
// 的 headers），本包不认识任何源，也不提供默认 UA（MKACG-624）。身份按通道两份
// （`Headers`），用哪一份由本次解析出的直链决定 —— 判据就是本包既有的 `channelOf`
// （MKACG-638：App 通道的媒体 CDN 拒绝站内 Referer，一份身份喂两个通道必有一边 403）。
package segment

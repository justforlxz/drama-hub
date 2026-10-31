// Package segment 负责播放面：关键帧索引、分片时间轴、播放列表生成、按切点现转封装
// 与分片缓存编排。
//
// 依赖方向：rank 40，可 import rank 30/20/10 的 internal 包（httpx、cache、stream、
// store）；与同级的 webdav 互不 import；rank 50/60 可 import 本包。规则见
// docs/DEV-STANDARDS.md「代码组织」，由 scripts/check-deps.sh 强制。
//
// 五件事（契约真值：docs/PLAYBACK-CONTRACT.md）：
//
//  1. 端点：`GET /video/{id}.m3u8`（`ServePlaylist`）与 `GET /video/{id}/{seq}.ts`
//     （`ServeSegment`），单集与合并集同一路径形态。
//  2. 关键帧索引：`ensureSegments` 在第一次播放某个 uid 时同步 `ffprobe` 预扫，把切点
//     按目标时长对齐到关键帧后写入 `segment` 表（30 s 硬上限，超时 503）。索引按需
//     建立，禁止批量预建；同一次探测还把源时长回填 `episode.duration_sec`
//     （`store.BackfillEpisodeDuration`，只填 0）并重建该剧 `emby_item`，`.nfo` 的
//     `<runtime>` 因此变成真实值。
//  3. 时间轴映射：`Timeline` 把有序的各集分片拼成连续 seq 空间，回答
//     `seq → (集号, 集内秒, 绝对起点)`（合并集播放的 O(1) 定位）。
//  4. 播放列表生成：`BuildPlaylist` 把时间轴渲成 VOD m3u8，集边界插
//     `#EXT-X-DISCONTINUITY`；playlist 是 `segment` 表的纯函数视图，不落盘、不缓存。
//  5. 分片：`cut` 按切点从直链现转封装出 MPEG-TS；分片经 `internal/cache` 落盘
//     （键 `sha256(uid\x1fseq\x1fcodec_sig)`，single-flight 合并并发生成）。
//
// 边界：直链不在本包解析（`ResolveFunc` 由装配根注入 `stream.Resolver`），也不落库
// ——0004 已删 episode.play_url；合并编排（建组 / 重建派生表）属 internal/store。
// 出站身份同理：ffmpeg / ffprobe 的 `-headers` 由装配根从插件常量注入（`NewCutter`
// 的 headers），本包不认识任何源，也不提供默认 UA（MKACG-624）。
package segment

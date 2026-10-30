// Package segment 负责关键帧索引、分片时间轴、播放列表生成与按切点现转封装。
//
// 依赖方向：rank 40，可 import rank 30/20/10 的 internal 包（httpx、store）；
// 与同级的 webdav、stream 互不 import；rank 50/60 可 import 本包。
// 规则见 docs/DEV-STANDARDS.md「代码组织」，由 scripts/check-deps.sh 强制。
//
// 四件事：
//
//  1. 关键帧索引：`ffprobe -skip_frame nokey` 预扫某集的源，把切点按目标时长
//     对齐到关键帧后写入 `segment` 表（`Cutter.Index`）。切点落在关键帧上，
//     `-c copy` 才不会漂移。
//  2. 时间轴映射：`Timeline` 把有序的各集分片拼成连续 seq 空间，回答
//     `seq → (集号, 集内秒, 绝对起点)`（合并集播放的 O(1) 定位）。
//  3. 播放列表生成：`BuildPlaylist` 把时间轴渲成 VOD m3u8（集边界插
//     `#EXT-X-DISCONTINUITY`），`Cutter.ServePlaylist` 服务合并集端点
//     （`PlaylistRoute`）。
//  4. 现转封装：`cut` 按一个切点从源直链切出 MPEG-TS 字节；`Cutter` 本身实现
//     `/s/{token}/seg/{uid}/{seq}.ts` 端点（`Route` 常量 + `ServeHTTP`），单集
//     与合并集共用。
//
// 边界：不落任何视频字节（由缓存代理负责，ADR-0004）；合并编排（建组 / 重建
// 派生表）属 internal/store；不做集间 codec 签名的转码判定。
package segment

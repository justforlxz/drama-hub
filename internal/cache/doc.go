// Package cache 负责 artifact 缓存（海报 / 关键帧索引 / 分片）的落盘与索引。
//
// 依赖方向：rank 20，可 import rank 10 的 internal 包（store）；rank 30 及以上的
// 包可 import 本包。规则见 docs/DEV-STANDARDS.md「代码组织」，由
// scripts/check-deps.sh 强制。
//
// 边界（S3 交付范围）：只做「取到就落盘 + 写 artifact 行」。LRU / 过期淘汰 /
// single-flight / 关键帧索引与分片缓存的生成归 S7（MKACG-611），见
// docs/CACHE-MODEL.md。海报不在淘汰候选里（ADR-0004 只让视频分片参与淘汰）。
package cache

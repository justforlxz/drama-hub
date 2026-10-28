// Package cache 负责 artifact 缓存（封面 / 关键帧索引）与 pin 预取驱动。
//
// 依赖方向：rank 20，可 import rank 10 的 internal 包（store）；rank 30 及以上的
// 包可 import 本包。规则见 docs/DEV-STANDARDS.md「代码组织」，由
// scripts/check-deps.sh 强制。
//
// 边界：本阶段只提供包骨架与空壳类型；视频缓存淘汰由缓存代理负责，不在本包。
package cache

// Store 是本包的中枢类型，对应一个 artifact 缓存。
type Store struct{}

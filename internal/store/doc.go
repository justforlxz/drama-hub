// Package store 负责 SQLite 连接、迁移与派生表重建，是依赖层级的底座。
//
// 依赖方向：rank 10（最低），不 import 其他 internal 包；其余 internal 包
// 只允许单向依赖本包。规则见 docs/DEV-STANDARDS.md「代码组织」，由
// scripts/check-deps.sh 强制。
//
// 边界：本阶段只提供包骨架与空壳类型，不含任何业务逻辑。
package store

// DB 是本包的中枢类型，对应一个 SQLite 连接。
type DB struct{}

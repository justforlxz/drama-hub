// Package store 负责 PostgreSQL 连接、迁移，drama / episode 两张表的读写（Store），
// 以及对外剧集（单集 / 合并集）的现算（ADR-0014：不落库，没有派生表），是依赖层级的底座。
//
// 依赖方向：rank 10（最低），不 import 其他 internal 包；其余 internal 包
// 只允许单向依赖本包。规则见 docs/DEV-STANDARDS.md「代码组织」，由
// scripts/check-deps.sh 强制。
package store

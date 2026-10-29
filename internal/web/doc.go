// Package web 提供管理 API 的 HTTP 入口（页面模板后续阶段并入本包）。
//
// 依赖通过构造函数注入；本包不持有全局状态。
//
// 依赖方向：rank 50，可 import rank 45/40/30/20/10 的 internal 包；rank 60
// 的 cmd 可 import 本包。规则见 docs/DEV-STANDARDS.md「代码组织」，由
// scripts/check-deps.sh 强制。
//
// 边界：框架阶段整棵 `/api/` 子树返回 501（已规划未实现）占位，不含页面/API
// 的业务逻辑；`/healthz` 属装配面，由 cmd 注册，不在本包。
package web

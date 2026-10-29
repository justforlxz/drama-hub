// Package httpx 提供 HTTP 边界的横切约定：统一错误映射与请求访问日志。
//
// 依赖方向：rank 30，只依赖标准库，不 import 任何 internal 包；rank 40/50/60
// 的包（webdav、stream、segment、web、cmd）可 import 本包。与同为 rank 30 的
// provider、vfs 同级、互不 import —— 上游/虚拟层不做 HTTP 状态分类，那是边界包
// 的职责。规则见 docs/DEV-STANDARDS.md「代码组织」，由 scripts/check-deps.sh 强制。
//
// 边界：只做两件事——把传输层错误类别映射为状态码、写出统一 JSON 错误响应；
// 在装配根记录每请求一条访问日志。不含路由、鉴权、recover、request id 透传，
// 也不含日志采样 / 轮转 / 落盘。
package httpx

// Package stream 负责出流：HMAC 签名、HEAD/GET/Range 响应。
//
// 依赖方向：rank 40，可 import rank 30/20/10 的 internal 包（httpx、store）；
// 与同级的 webdav、segment 互不 import；rank 50/60 可 import 本包。
// 规则见 docs/DEV-STANDARDS.md「代码组织」，由 scripts/check-deps.sh 强制。
//
// 边界：本阶段只有单集字节中继 `GET|HEAD /s/{token}/e/{uid}`；分片（M6）与
// 合并集（M7）在 `/s/` 子树内自行注册，其余路径仍返回 501。
package stream

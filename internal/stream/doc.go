// Package stream 负责出流：HMAC 签名、HEAD/GET/Range 响应。
//
// 依赖方向：rank 40，可 import rank 30/20/10 的 internal 包（provider、vfs、
// cache、store）；与同级的 webdav、segment 互不 import；rank 50/60 可 import 本包。
// 规则见 docs/DEV-STANDARDS.md「代码组织」，由 scripts/check-deps.sh 强制。
//
// 边界：本阶段只提供包骨架与空壳类型，不含任何业务逻辑。
package stream

// Server 是本包的中枢类型，对应一个出流服务。
type Server struct{}

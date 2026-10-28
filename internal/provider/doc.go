// Package provider 封装短剧数据源适配（官网 SSR 主力 / App 接口回退）。
//
// 依赖方向：rank 30，可 import rank 20/10 的 internal 包（cache、store）；
// rank 40/50/60 的包可 import 本包；与同为 30 的 vfs 同级、互不 import。
// 规则见 docs/DEV-STANDARDS.md「代码组织」，由 scripts/check-deps.sh 强制。
//
// 边界：本阶段只提供包骨架与空壳类型，不含任何业务逻辑。
package provider

// Source 是本包的中枢类型，代表一个上游数据源。
type Source struct{}

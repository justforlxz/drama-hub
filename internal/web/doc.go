// Package web 提供管理 API 与三页管理界面（剧目 / 合并编排 / 同步与缓存看板）。
//
// 依赖通过构造函数注入（store、settings、数据源同步器、logger）；本包不持有全局
// 状态。模板与静态资源用 //go:embed 随二进制嵌入，没有前端构建步骤（ADR-0002）。
//
// 依赖方向：rank 50，可 import rank 45/40/30/20/10 的 internal 包；rank 60
// 的 cmd 可 import 本包。规则见 docs/DEV-STANDARDS.md「代码组织」，由
// scripts/check-deps.sh 强制。
//
// 边界：本包负责页面渲染、JSON 响应与传输层错误映射（httpx），业务规则（同步
// 解析、合并编排校验、派生表重建）留在 domain 包。`/healthz` 属装配面，由 cmd
// 注册，不在本包。
package web

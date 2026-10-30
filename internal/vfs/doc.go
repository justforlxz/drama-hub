// Package vfs 把数据库里的剧目与集渲染成一棵只读的虚拟文件树。
//
// Tree 是本包唯一入口：List 列出**一个目录**的直接子项，Render 按路径产出
// strm / nfo 的完整字节。树与文件名都是纯函数派生的，不落盘（AGENTS.md
// 硬约束 1）；渲染结果的长度只有 len(返回值) 一个真值，调用方拿它当
// Content-Length（硬约束 3），本包不提供独立的长度字段或 Size 方法。
//
// 依赖方向：rank 30，可 import rank 20/10 的 internal 包（cache、store）；
// rank 40/50/60 的包可 import 本包；与同为 30 的 plugin/hongguo、sync 同级、
// 互不 import。
// 规则见 docs/DEV-STANDARDS.md「代码组织」，由 scripts/check-deps.sh 强制。
//
// 目录形状（分类 → 分片 → 剧 → Season 01 → 文件）与命名、同名冲突规则的真值是
// docs/DIRECTORY-CONTRACT.md。
//
// 边界：只读 drama / episode / emby_item 三张表；封面下载、HTTP 处理、
// 缓存与分页都不在本包。树与文件内容的**数据源是 emby_item 派生表**（硬约束 6：
// 对外集号只由它决定）——单集与合并集在同一棵树里各占一条文件对；派生表的重建
// 属 internal/store，本包只读。出流 token 的算法（HMAC，ADR-0003）属 internal/stream
// （rank 40），本包通过 NewTree 注入的 TokenFunc 计算，不自行实现签名。
//
// 渲染契约见 docs/DEV-STANDARDS.md「虚拟文件树与渲染」。
package vfs

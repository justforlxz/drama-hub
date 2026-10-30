// Package webdav 把虚拟文件树适配为 x/net/webdav 的只读 FileSystem。
//
// NewHandler 是唯一入口：Basic 认证（settings.webdav_user / webdav_pass，每请求
// 现读）→ x/net/webdav 协议 → FS（x/net/webdav.FileSystem）→ Tree 的
// List / Render（生产实现是 *vfs.Tree，单测注入 fakeTree）。文件长度只有
// len(Render 的字节) 一个来源，PROPFIND 的
// getcontentlength 与 GET 的 Content-Length 都取它（AGENTS.md 硬约束 3），
// 本包不引入第二条长度来源，也不落盘任何东西（硬约束 1）。
//
// 依赖方向：rank 40，可 import rank 30/20/10 的 internal 包（vfs、store、httpx）；
// 与同级的 stream、segment 互不 import。规则见 docs/DEV-STANDARDS.md
// 「代码组织」，由 scripts/check-deps.sh 强制。
//
// 边界：只读（Mkdir / RemoveAll / Rename / 写打开一律失败），不含出流、分片、
// 合并集，不做封面（属 artifact 缓存）。树不跨请求缓存：一次请求内按「列过的目录」
// 做快照，跨请求每次现算。契约见 docs/DEV-STANDARDS.md「WebDAV」。
package webdav

package store

import "sync/atomic"

// treeRev 是进程内的「树结构版本号」，单调递增。
//
// 语义：任何可能改变 WebDAV 树结构或虚拟文件字节长度的写入，成功后都要让它 +1；
// vfs 与 webdav 的结构缓存以它为准，读到的版本与缓存条目不一致就整体失效
// （docs/adr/0015「失效模型」）。
//
// 放在进程内而不是库内：本服务是单二进制单实例（AGENTS.md 硬约束 4），版本号只需
// 在本进程内一致；重启后缓存本来就是空的，从 0 重新开始没有影响。多个测试库共用
// 一个进程只会让版本号多跳几次（多失效一次），不会读到旧数据。
//
// 递增必须发生在事务**提交之后**：提交前递增会让读者把「提交前读到的旧数据」
// 存进新版本号下，从而永久读旧值。
var treeRev atomic.Uint64

// TreeRevision 返回当前的树结构版本号。
func TreeRevision() uint64 { return treeRev.Load() }

// bumpTreeRevision 递增树结构版本号。只允许 store 包的写路径调用。
func bumpTreeRevision() { treeRev.Add(1) }

// TreeRevision 实现 vfs.Store：本进程的树结构版本号。
//
// 定义在 *Store 上而不是包级函数，是为了让 vfs 通过它已有的 Store 接口拿到，
// 不必额外持有一个「版本号来源」。
func (s *Store) TreeRevision() uint64 { return TreeRevision() }

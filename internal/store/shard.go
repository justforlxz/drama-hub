package store

import "hash/fnv"

// ShardCount 是分类目录下分片层的大小 N（docs/DIRECTORY-CONTRACT.md「D2」）。
//
// 它是代码常量，不是 schema、也不进 settings：分片只服务「一个目录的子项数摊薄」，
// 改 N 时一条 `UPDATE drama SET shard = …` 重算即可，不改表结构。
// 取值依据（S5 用真实数据校准，docs/DIRECTORY-CONTRACT.md「N 怎么定」）：最大分类
// 「剧情」≈183,000 部，除以 128 后均值 ≈1,430 子项/目录、单分片最多 ≈1,900，
// PROPFIND 的响应体落回百 KB 量级，仍在「≤3000」的判据内。
//
// 改这个常量时注意两件事（都不改 schema，但要一次数据迁移）：
//   - 变大 >256 之后分片目录名 `%02x` 会出三位十六进制（00–ff 仍是两位，N≤256 安全）；
//   - 变小（或变大）之后必须用新 N 重算存量行的 `drama.shard`（一条 UPDATE / 逐行重算），
//     否则旧行仍停在旧分片号下——按新 N 拼出的路径找不到它们，S8 回填时别踩。
const ShardCount = 128

// ShardOf 返回某个 provider_id 的目录分片号：fnv32a(providerID) % ShardCount。
//
// 纯函数：只由身份决定，不依赖时间、标题或配置，因此剧改名、重扫都不换分片。
// 分片号不承载任何业务含义，只用于 `/<类型>/<分片>/` 这一层的分页列举。
func ShardOf(providerID string) int {
	h := fnv.New32a()
	// Write 的返回值只表示写入了多少字节；hash 的写入不会失败，忽略是安全的。
	_, _ = h.Write([]byte(providerID))
	return int(h.Sum32() % ShardCount)
}

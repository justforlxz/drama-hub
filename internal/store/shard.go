package store

import "hash/fnv"

// ShardCount 是分类目录下分片层的大小 N（docs/DIRECTORY-CONTRACT.md「D2」）。
//
// 它是代码常量，不是 schema、也不进 settings：分片只服务「一个目录的子项数摊薄」，
// 改 N 时一条 `UPDATE drama SET shard = …` 重算即可，不改表结构。
// 默认 128 的取值依据：最大分类 ≈ 21.6 万部，除以 128 后约 1.7k 子项/目录，
// PROPFIND 的响应体落回百 KB 量级；实测值校准后由上层改这个常量。
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

// Package cache 负责 artifact 缓存的落盘与淘汰：海报（cover）、关键帧索引（index）、
// 视频分片（segment）。
//
// 依赖方向：rank 20，可 import rank 10 的 internal 包（store）；rank 30 及以上的
// 包可 import 本包。规则见 docs/DEV-STANDARDS.md「代码组织」，由
// scripts/check-deps.sh 强制。本包不 import httpx（rank 30）、segment（rank 40）：
// 生成内容的动作（ffmpeg 切片、ffprobe 建索引）由上层以 GenerateFunc 注入，错误
// 原样透出，由边界包映射状态码。
//
// 契约真值：docs/CACHE-MODEL.md（键规则、目录布局、默认值、测试要求）+ ADR-0007。
//
// 三件事：
//
//  1. 读写：`Cache.GetOrGenerate` 命中即读本地文件并 touch `artifact.last_used_at`，
//     未命中先按 single-flight 生成（同一键只生成一次），再「临时文件 + rename」原子
//     落盘、写 artifact 行。海报这条链路另有 `CoverCache`（S3 交付），共用同一套键与
//     布局规则，并在命中时 touch。
//  2. 原子性：临时文件写 `<cache_dir>/tmp`，rename 进内容目录；进程被 kill 时内容
//     目录里只会出现完整文件，tmp 里的半截文件由淘汰清扫。
//  3. 淘汰：`Cache.Evict` 先按 `cache_lru_days` 删过期（三种 kind 都算——过期是
//     「多久没命中」，与类型无关），再按 `cache_max_mb` 删最久未用的分片（只有分片
//     参与大小上限）。`Cache.RunEviction` 是装配根起的 5 分钟循环。
//
// 不缓存的东西由 `Spec.validate` 兜住：m3u8 文本与源整片没有对应的 kind，写不进来
// （理由见 docs/CACHE-MODEL.md「为什么 m3u8 文本不缓存」「为什么不缓存源 mp4 整文件」）。
package cache

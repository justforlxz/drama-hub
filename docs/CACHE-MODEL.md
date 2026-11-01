# 缓存模型

状态：定稿（2026-10-30 / MKACG-604，按 lxz 2026-10-30 评论新增）
前置：`docs/PLAYBACK-CONTRACT.md`（`/video/{id}.m3u8` 与分片端点）
决策记录：`docs/adr/0007-own-hls-and-local-cache.md`（本模块**推翻** ADR-0004 的划界）

## 为什么现在由本服务做缓存

ADR-0004 的理由是「视频分片是缓存代理在传输层做的，它拥有访问历史与本地占用，天然适合做 LRU」。
那个前提是：`.strm` 指向 rclone 代理的地址，播放字节从 rclone 的缓存里流出去。

**本次改动之后这个前提不存在了**：`.strm` 里是本服务自己的 `/video/{id}.m3u8`，
播放字节由本服务切片产出，rclone 不再在播放链路上（它只剩 WebDAV 只读挂载）。
链路里没有别人能缓存，所以缓存必须在链路里唯一的服务身上。
详见 `docs/adr/0007-own-hls-and-local-cache.md`。

## 缓存什么、不缓存什么

| 对象 | 缓存？ | 位置 | 键 | 说明 |
|---|---|---|---|---|
| 海报（cover） | **是** | `artifact(kind='cover')` + 文件 | `<provider>/<provider_id>` | 已有机制，本次只是把「刷新时下载」接上 |
| 分片（`.ts`） | **是** | `artifact(kind='segment')` + 文件 | `sha256(uid \| seq \| codec_sig)` | 新增，播放的主收益在这；`codec_sig` 带通道段（`web/…` / `app/…`，见「落盘布局与键」） |
| 关键帧索引 | **是** | `artifact(kind='index')` | `uid` | 已有机制，改为按需建立（`docs/PLAYBACK-CONTRACT.md`） |
| m3u8 文本 | **否** | — | — | 见下 |
| 源 mp4 整文件 | **否** | — | — | 见下 |

### 为什么 m3u8 文本不缓存

playlist 是 `segment` 表的**纯函数**（`segment.BuildPlaylist`）：一次查询 + 一次字符串拼接，
实测单次 < 1 ms。把它落盘等于给同一个东西存第二份真值，还要处理
「索引重建了但 playlist 文件没失效」这类不一致。

你要的「缓存 m3u8」的实际收益，全在它引用的分片上——所以缓存分片就够了，
`segment` 表本身就是 playlist 的持久化形态。

### 为什么不缓存源 mp4 整文件

单集源是 360p / 3–10 分钟 / 十几 MB 的整片。缓存整片意味着「这一集全部字节」
（含永远看不到的部分）与我们的分片缓存重复存储，且整片不可切片复用。
分片缓存天然是按需的：只看前 30 秒就只存前 30 秒。

## 落盘布局与键

```
<cache_dir>/
├── seg/<hash 前 2 位>/<hash>.ts        # 分片，内容寻址
├── cover/<hash 前 2 位>/<hash>.<ext>   # 海报
└── index/<hash 前 2 位>/<hash>.json    # 关键帧索引
```

- 文件名里的 `<hash>` 按 kind 分两种取法（`internal/cache/keys.go` 的 `cacheHash`），
  不是一句 `sha256(<键>)` 能覆盖的：
  - **分片直接用键本身**。分片的键已经是 `sha256(uid | seq | codec_sig)` 的十六进制，
    落盘名就是它，不再二次哈希（二次哈希会同时与上面布局图和实现不符）。
  - **海报与索引取 `sha256(<语义键>)` 的十六进制**：它们的键是语义标识（`provider/provider_id`、
    `uid`），需要先映射成定长十六进制才能做文件名与分桶。
- 前 2 位做一级分桶，避免单目录十万文件。
- 键：
  - 分片：`uid + "\x1f" + seq + "\x1f" + codec_sig`
  - 海报：`provider + "/" + provider_id`
  - 索引：`uid`
- `codec_sig` 不是裸的编码签名，而是 **`<通道>/<签名>`**（`web/…` / `app/…`，
  MKACG-626 契约 4）：同一集的网页源与 App 源字节流不同，而签名
  （codec / profile / level / pix_fmt / tag / time_base）可能完全相同（它不含分辨率与时长，
  10 秒 360p 预览与 3 分钟完整集会算出同一个值）——缺了通道段就会串味。内容密钥
  （`plugin.Stream.Key`）**不进键**：通道段就是它在键里的替身。
- 文件**不可变**：内容只由键决定，所以写用「临时文件 + `rename`」，读到半截文件的可能为零。
- **海报的下载超时与失败口径（MKACG-620）**：单次出站超时按 `cover_timeout_sec`
  现读（默认 5 s，1–60，见 `docs/DEV-STANDARDS.md`「配置」表），不跟插件的
  `base_url` / `request_timeout_sec` 走 —— 封面 URL 是上游页面里的三方 CDN 绝对地址。
  取不回来时该剧仍算同步成功：剧与集已入库、`artifact(kind='cover')` 留空、日志一条 warn，
  下一轮刷新自然重试（`internal/cache.CoverCache` 只报 `ErrCoverFetch`，是否上升为失败
  由调用方 `internal/sync` 决定）。

## 失效

只有四种，全部按「保守」处理：

| 触发 | 处理 |
|---|---|
| 键变了 | 自然产生新文件，旧文件由 LRU 淘汰（不需要主动删） |
| `stream_secret` 变了 | `uid` 没变 → 缓存仍命中；只有 URL 失效，缓存不受影响 |
| 通道变了（web ↔ app） | `codec_sig` 带通道段 → 分片键自然变，旧片由 LRU 淘汰；`segment` 索引的通道段与本次请求不一致即**重建**（不沿用另一个通道的片数与时长；加通道段之前写下的老索引一律算失效） |
| 上游同 `vid` 换了内容 | 无法自动感知（`vid` 不变、`codec_sig` 不变）。由 `cache_lru_days` 兜底过期 |

**不做主动失效**：没有指纹比对、没有 ETag 回源校验。理由：单用户场景里
「上游悄悄换了同一集的内容」是可忽略的边缘情况（硬约束 9），为它做指纹比对
要给每次播放加一次上游 HEAD。

## 淘汰

| 参数 | 默认 | 范围 | 说明 |
|---|---|---|---|
| `cache_dir` | `./data/cache`（镜像 `/data/cache`） | 绝对或相对路径 | 缓存根目录；`{cover,index,seg,tmp}` 四个目录在启动时建出（`Cache.EnsureLayout`），写盘时按需再 `MkdirAll` |
| `cache_max_mb` | `8192` | 0–1048576 | 分片缓存上限；`0` = 不按大小淘汰 |
| `cache_lru_days` | `30` | 1–365 | 超过 N 天未命中的条目删除 |

（这三个 key **必须**同时改 `AGENTS.md` 硬约束 5 与 `docs/DEV-STANDARDS.md`「配置」表，
原文写的是「缓存上限与 LRU 天数不是本服务的配置」。见 ADR-0007 的「需要同步修改的文档」。）

淘汰循环（单 goroutine，启动后每 5 分钟一次，与请求路径无关）：

1. `DELETE` 过期：`artifact WHERE last_used_at < now - cache_lru_days`
2. 若 `SUM(bytes) WHERE kind='segment' > cache_max_mb`：
   按 `last_used_at` 升序删到上限以下（一次删一批，避免长事务）
3. 每删一条：先删文件、再删行；文件已不存在也算成功（自愈）

不做的事：不做跨进程锁（单实例）、不做热/冷分区、不做按剧 pin 的例外。

## 并发

| 场景 | 做法 |
|---|---|
| 同一 `(id, seq)` 并发请求 | single-flight：一个生成，其余等结果 |
| 多个不同分片同时生成 | 受现有 ffmpeg 闸限制（`min(4, max(1, NumCPU/2))`，不新增配置） |
| 淘汰与读取撞车 | 读先打开文件句柄再查行；行被删但文件还在 → 命中；文件被删 → 重新生成。不需要锁 |
| 写缓存 | 临时文件 + `rename`，同名并发写同一个内容无害 |

## 命中后的读取路径

```
GET /video/{id}/{seq}.ts
  → 解析 id（HMAC 校验）
  → 查 artifact(kind='segment', key=sha256(键))    1 次主键查询
  → 命中：打开文件，http.ServeContent（Range 由它处理），touch last_used_at
  → 未命中：single-flight 生成 → 落缓存 → 回字节
  → 生成失败：502，不写缓存
```

`last_used_at` 的更新**可丢**：写失败只让这条更早被淘汰，不影响正确性，因此命中
路径不因 touch 失败而失败。S7 的实现是同步一次 `UPDATE`（命中路径一次主键写，量级
可忽略），换来的好处是命中即生效、不用等后台任务。

## 与 WebDAV 的关系

WebDAV 只承载元数据（`.strm` / `.nfo` / 目录），**不经手分片**。
因此 `cache_dir` 与 WebDAV 挂载点无关，rclone 也不再需要 `--vfs-cache-mode full`
（它的缓存以前是给播放用的，现在播放不经过它）。这一点要写进 Emby/rclone 配置说明。

## 磁盘预算（给验收用）

| 量 | 估算 | 依据 |
|---|---|---|
| 一片 `.ts` | 10 s × ~520 kbps ≈ 650 KB | 实测源 651,433 B / 10.07 s（`docs/L1-CORPUS-SCOPE.md` 取证 D） |
| 一集（40 集均值里的 1 集，按 3 分钟） | ≈ 12 MB | 10 s/片 → 18 片 |
| 默认上限 8 GB 可容 | ≈ 650 集 ≈ 16 部剧 | — |
| 海报 | ≈ 100 KB/部 × 可缓存的少量剧 | — |

结论：默认 8 GB 对「同时在追几部剧」的用法足够；`cache_max_mb=0` 时磁盘只受
`cache_lru_days` 约束，这是给磁盘充裕的人留的口子。

## 测试要求（进 `docs/TEST-STANDARDS.md`）

必须真跑的：

1. **缓存命中不再回源**：第一次请求分片 → 假 CDN 计数 +1；第二次 → 计数不变。
2. **命中/未命中的字节一致**：两次响应 `len(body)` 与 `sha256(body)` 相同。
3. **生成失败不写缓存**：假 CDN 返回 500 → 502，且 `artifact` 无该行、目录无该文件。
4. **淘汰按上限生效**：写入超过 `cache_max_mb` 的数据 → 触发淘汰后 `SUM(bytes)` 回落到上限内，
   且被删的是 `last_used_at` 最旧的那些。
5. **`cache_lru_days` 过期**：把一条 `last_used_at` 改到 40 天前 → 淘汰后该行与文件都不在。
6. **single-flight**：并发 10 个同 `(id, seq)` 请求 → 假 CDN 只被调用 1 次。

不测：文件系统异常（磁盘满、权限拒绝）的具体错误文案；跨进程并发。

## 2.0 明确不做

- 缓存预热与预取（把整库提前切好 / 播第 N 片时喂 N+1…）：2847 万集不可能预切。
  1.0 预留的 `seg_prefetch` / `pin_days` 占位键已删除（MKACG-616），代码里没有预取循环。
- 按剧/按用户配额、多级缓存、CDN 回源策略。
- 内容指纹校验 / ETag 回源。
- 缓存统计看板以外的可观测性（`/api/status` 报 `bytes` 与条目数即可）。

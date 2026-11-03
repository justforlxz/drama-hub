# 缓存模型

状态：定稿 v2（2026-11-02 / MKACG-652，ADR-0011：片段改成每集一片 fMP4，缓存按集）
前置：`docs/PLAYBACK-CONTRACT.md`（`/video/{id}.m3u8` 与分片端点）
决策记录：`docs/adr/0007-own-hls-and-local-cache.md`（缓存归本服务）、
`docs/adr/0011-episode-per-segment-fmp4.md`（键的形状、片段形态）

## 为什么由本服务做缓存

ADR-0004 的理由是「视频分片是缓存代理在传输层做的，它拥有访问历史与本地占用，天然适合做 LRU」。
那个前提是：`.strm` 指向 rclone 代理的地址，播放字节从 rclone 的缓存里流出去。

**这个前提不存在了**：`.strm` 里是本服务自己的 `/video/{id}.m3u8`，播放字节由本服务
转封装产出，rclone 不再在播放链路上（它只剩 WebDAV 只读挂载）。链路里没有别人能缓存，
所以缓存必须在链路里唯一的服务身上。详见 ADR-0007。

## 缓存什么、不缓存什么

| 对象 | 缓存？ | 位置 | 键 | 说明 |
|---|---|---|---|---|
| 海报（cover） | **是** | `artifact(kind='cover')` + 文件 | `<provider>/<provider_id>` | 刷新时下载并本地化 |
| 分片（`.m4s`，一整集） | **是** | `artifact(kind='segment')` + 文件 | `sha256("e<episode.id>" \x1f channel)` | 播放的主收益在这；**seq 不进键**，同一集在单集/合并集/重新分组后共用一份（ADR-0011 D3） |
| m3u8 文本 | **否** | — | — | 见下 |
| 源 mp4 整文件 | **否** | — | — | 见下 |

关键帧索引（`artifact(kind='index')`、`<cache_dir>/index/`）随「10 秒切片」一起删除：
每集一片之后 playlist 只需要时长，而时长唯一真值是 `episode.duration_sec`。
`artifact_kind_check` 因此只剩 `cover` / `segment`（迁移 `0006`）。

### 为什么 m3u8 文本不缓存

playlist 是派生数据的**纯函数**（`segment.BuildPlaylist`）：一次查询 + 一次字符串拼接，
实测单次 < 1 ms。把它落盘等于给同一个东西存第二份真值，还要处理
「时长回填了但 playlist 文件没失效」这类不一致。你要的「缓存 m3u8」的实际收益，
全在它引用的分片上——缓存分片就够了。

### 为什么不缓存源 mp4 整文件

源是 360p / 3–10 分钟 / 十几 MB 的整片。缓存整片意味着「这一集全部字节」与我们的
整集分片重复存储（两者内容等价、只是容器不同），且源直链是带签名的短时凭据
（契约要求不落库）。分片缓存本身就是「这一集的字节」，没有多存一份的必要。

## 落盘布局与键

```
<cache_dir>/
├── seg/<hash 前 2 位>/<hash>.m4s        # 分片（一整集 fMP4），内容寻址
├── cover/<hash 前 2 位>/<hash>.<ext>    # 海报
└── tmp/                                 # 写入中的临时文件（rename 前）
```

- 文件名里的 `<hash>` 按 kind 分两种取法（`internal/cache/keys.go` 的 `cacheHash`）：
  - **分片直接用键本身**。分片的键已经是 `sha256("e<id>" \x1f channel)` 的十六进制，
    落盘名就是它，不再二次哈希。
  - **海报取 `sha256(<语义键>)` 的十六进制**：它的键是语义标识（`provider/provider_id`），
    需要先映射成定长十六进制才能做文件名与分桶。
- 前 2 位做一级分桶，避免单目录十万文件。
- 键：
  - 分片：`"e<episode.id>" + "\x1f" + channel`
  - 海报：`provider + "/" + provider_id`
- `channel` ∈ `web` / `app`，**必须进键**：同一集的网页源与 App 源字节流不同
  （App 源是 CENC 密文，ffmpeg 用 `-decryption_key` 解出来）。内容密钥
  （`plugin.Stream.Key`）**不进键**：通道段就是它在键里的替身，判据只有
  `segment.channelOf` 一处。
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
| `merge_chunk_size` 变了 | 合并块的 `uid` 变 → 旧分片键不再被任何 URL 命中，旧文件由 LRU 淘汰（不主动清） |
| 通道变了（web ↔ app） | 键里的通道段变了 → 分片键自然变，旧片由 LRU 淘汰；`segment` 行的 channel 由 playlist 路径覆盖（不沿用另一条通道的片） |
| 上游同 `vid` 换了内容 | 无法自动感知（`vid` / `episode.id` 不变）。由 `cache_lru_days` 兜底过期 |
| 本版本升级（`.ts` → `.m4s`） | 键的 preimage 与后缀都变了 → 旧片**永不命中**；旧行与旧文件不主动删，交给 `cache_lru_days` 淘汰 |

**不做主动失效**：没有指纹比对、没有 ETag 回源校验。理由：单用户场景里
「上游悄悄换了同一集的内容」是可忽略的边缘情况（硬约束 9），为它做指纹比对
要给每次播放加一次上游 HEAD。

## 淘汰

| 参数 | 默认 | 范围 | 说明 |
|---|---|---|---|
| `cache_dir` | `./data/cache`（镜像 `/data/cache`） | 绝对或相对路径 | 缓存根目录；`{cover,seg,tmp}` 三个目录在启动时建出（`Cache.EnsureLayout`），写盘时按需再 `MkdirAll` |
| `cache_max_mb` | `8192` | 0–1048576 | 分片缓存上限；`0` = 不按大小淘汰 |
| `cache_lru_days` | `30` | 1–365 | 超过 N 天未命中的条目删除 |

淘汰循环（单 goroutine，启动后每 5 分钟一次，与请求路径无关）：

1. `DELETE` 过期：`artifact WHERE last_used_at < now - cache_lru_days`
2. 若 `SUM(bytes) WHERE kind='segment' > cache_max_mb`：
   按 `last_used_at` 升序删到上限以下（一次删一批，避免长事务）
3. 每删一条：先删文件、再处理行；文件已不存在也算成功（自愈）。行的处理**按 kind 分两种**：
   - **分片与索引**：行与文件一起删。行只是「文件在哪」的登记，没有字节就没有意义。
   - **海报**：**行留着**，只把 `path` / `bytes` / `sha256` 清空、`last_used_at` 推到当轮时间
     （`store.ReleaseArtifact`）。这一行同时是「这部剧有封面」的唯一真值——`poster.jpg`
     挂在树里、请求能到 `Render`，全靠它。连行一起删会让剧目录少掉 `poster.jpg`、`GET` 404，
     要等该剧被下一轮同步重新本地化才回来，「淘汰后按需重新下载」永远不发生
     （MKACG-650 功能检测抓到的失败项）。清空 path 表达「本地没有字节」，下一次请求按未命中
     回源并写回行；`last_used_at` 推到当下，下一轮过期查询不会再选中同一行。

不做的事：不做跨进程锁（单实例）、不做热/冷分区、不做按剧 pin 的例外。

## 海报：按需回源（MKACG-650）

剧目录下的 `poster.jpg` 是虚拟文件，字节取自这里缓存的封面文件（`artifact(kind='cover')`）。
请求它时的路径与播放面同构：

```
GET /webdav/<分类>/<剧>/poster.jpg
  → 查 artifact(kind='cover', key=<provider>/<provider_id>)    1 次主键查询
  → 命中（行在、文件在）：读文件 → 返回字节，touch last_used_at
  → 未命中（文件被手删、或被淘汰降级为空 path）：single-flight 回源一次 → 原子落盘 → 写回 artifact 行 → 回字节
  → 源未声明封面 / 回源失败：WebDAV 404，不写缓存、不返回半截字节，下次请求可再试
```

- **复用同一套回源逻辑**：回源、落盘布局、键、超时（`cover_timeout_sec`）与同步时的
  `LocalizeCover` 是同一份实现（`CoverCache.download`），没有第二套。
- **single-flight 与同步共用一组**：`CoverCache` 的 `flightGroup` 同时服务「刷新时本地化」
  与「请求时按需回源」，所以一次刷新与一堆 poster 请求撞在同一个封面上时也只回源一次。
- **`HasCover` 只看 artifact 行、不看文件**：行在 = 这部剧在树里有 `poster.jpg`。文件被淘汰
  后节点仍要留着，否则请求根本到不了 `Render`，谈不上「按需重新下载」。
- 这里**只**复用已有的 `cover` kind 与 `<cache_dir>/cover/…` 布局，不新增缓存类型。

## 并发

| 场景 | 做法 |
|---|---|
| 同一缓存键并发请求 | single-flight：一个生成，其余等结果 |
| 同一封面的并发请求（同步 + poster.jpg） | 同一套 single-flight（`CoverCache` 的 `flightGroup`），只回源一次 |
| 多个不同分片同时生成 | 受现有 ffmpeg 闸限制（`min(4, max(1, NumCPU/2))`，不新增配置） |
| 淘汰与读取撞车 | 读先打开文件句柄再查行；行被删但文件还在 → 命中；文件被删 → 重新生成。不需要锁 |
| 写缓存 | 临时文件 + `rename`，同名并发写同一个内容无害 |

## 命中后的读取路径

```
GET /video/{id}/{seq}.m4s
  → 解析 id（HMAC 校验）
  → 查 segment 行拿通道（episode_id 主键查询）    ← 行在就不解析直链
  → 查 artifact(kind='segment', key=sha256(键))    1 次主键查询
  → 命中：打开文件，http.ServeContent（Range 由它处理），touch last_used_at
  → 未命中：解析直链 → single-flight 整集转封装 → 落缓存 → 回字节
  → 生成失败：502，不写缓存
```

`segment` 行是「这一集走哪条通道」的持久事实，也是命中路径零外呼的前提。
行缺失（第一次播放没走 playlist、或升级前的老库）时分片端点自己解析一次直链、
按解析结果写回这一行，再走上面的流程。

缓存里的片段字节一律 **0 基**：它只由「哪一集 + 哪个通道」决定，所以同一集在单集、合并块、
改 `merge_chunk_size` 前后都命中同一个文件。**播放位置不进缓存**：合并块的第 k 片在出流时
把 `moof/traf/tfdt` 的 `baseMediaDecodeTime` 加上「前面各集时长之和」（ADR-0011「修正：
集边界时间戳」），因此出站字节与缓存文件只在 4/8 字节字段上不同、长度相同、片头仍是
`ftyp+moov`（自包含不受影响），`Range` 照常可用。

`last_used_at` 的更新**可丢**：写失败只让这条更早被淘汰，不影响正确性，因此命中
路径不因 touch 失败而失败。实现是同步一次 `UPDATE`（命中路径一次主键写，量级
可忽略），换来的好处是命中即生效、不用等后台任务。

## 结构缓存与 PROPFIND 响应缓存（MKACG-669）

上面几节讲的是**磁盘缓存**（分片、海报）。目录树与 PROPFIND 响应体是另一类：
**只在进程内存里，不落盘，重启即失**。决策与测量见
[ADR-0015](adr/0015-tree-structure-cache.md)，这里只列口径。

| 层 | 缓存什么 | 键 | 上限 | 失效 |
|---|---|---|---|---|
| 结构 | 某个目录的 `[]vfs.Node` | 目录路径（`""` ≡ `短剧`）+ 结构版本号 | 300,000 个节点（LRU） | 版本号变了即整层失效 |
| 响应 | PROPFIND 的 207 状态 + 头 + 完整 body | 版本号 + 路径 + `Depth` + 基地址 + 请求体 SHA-256 | 单条 16MB / 总量 64MB（LRU） | 同上（键里带版本号） |

- **为什么响应也要缓存**：8,329 子项档实测 254ms，其中 `Tree.List` 只占 ~30ms，
  其余是 `x/net/webdav` 逐子项构建 propstat 与 XML 序列化。只缓存结构达不到
  「命中 ≤50ms」。详见 ADR-0015。
- **失效只有一种**：进程内的 `store.TreeRevision()` 单调计数器。任何会影响树结构或
  文件长度的写（剧/集增删改、封面 artifact 变更、任何 `settings` 写入）在**事务提交后**
  +1；版本号一变，结构与响应两层一起失效，下一次请求冷算。不做按子树的精确失效——
  漏一条就是永久读到旧数据。
- **命中不绕过认证**：WebDAV 先做 Basic 认证，通过后才查响应缓存；改 `webdav_user` /
  `webdav_pass` 后旧凭据立即 401，不会命中任何缓存内容。
- **大响应不缓存**：超过 16MB 单条的 PROPFIND（10 万 / 21.8 万子项档）直接透传，
  不占缓存。这两档的改善来自结构缓存，不来自响应回放。
- **不缓存错误**：非 207、写出中断、二次 `WriteHeader` 的响应一律不入缓存。
- **single-flight**：同一目录/同一请求并发未命中时只算一次，等待者尊重自己的 `ctx`。

测试要求（详见 `docs/TEST-STANDARDS.md`）：写入后立刻可见、改 `merge_chunk_size` 立刻
生效、并发只算一次、容量淘汰、错误不缓存、命中字节与未命中一致、改密码后旧凭据 401。

## 与 WebDAV 的关系

WebDAV 只承载元数据（`.strm` / `.nfo` / 目录），**不经手分片**。
因此 `cache_dir` 与 WebDAV 挂载点无关，rclone 也不再需要 `--vfs-cache-mode full`
（它的缓存以前是给播放用的，现在播放不经过它）。这一点要写进 Emby/rclone 配置说明。

## 磁盘预算（给验收用）

| 量 | 估算 | 依据 |
|---|---|---|
| 一集（3 分钟，360p） | ≈ 12 MB | 651,433 B / 10.07 s ≈ 520 kbps（`docs/L1-CORPUS-SCOPE.md` 取证 D） |
| 默认上限 8 GB 可容 | ≈ 680 集 ≈ 17 部剧（按 40 集/部） | 8 GB ÷ 12 MB |
| 海报 | ≈ 100 KB/部 × 可缓存的少量剧 | — |

「一片 = 一集」之后按片计量的时代结束：**上限的含义从「多少片」变成「多少集」**。
默认 8 GB 对「同时在追几部剧」的用法足够；`cache_max_mb=0` 时磁盘只受
`cache_lru_days` 约束，这是给磁盘充裕的人留的口子。

## 测试要求（进 `docs/TEST-STANDARDS.md`）

必须真跑的：

1. **缓存命中不再回源**：第一次请求分片 → 假 CDN 计数 +1；第二次 → 计数不变，
   且「生成」Info 日志仍只有一条（AC「第二次 ffmpeg 调用 0 次」）。
   海报同理：`poster.jpg` 命中本地时计数不变（`TestPosterReadsLocalWithoutRefetch`）。
2. **命中/未命中的字节一致**：两次响应 `len(body)` 与 `sha256(body)` 相同。
3. **生成失败不写缓存**：假 CDN 返回坏字节 → 502，且 `artifact` 无该行、目录无该文件。
4. **键只由「集 + 通道」决定**：同一集在两个通道下算出不同的键；同一集在不同 group 下
   算出相同的键（golden 断言）。
5. **淘汰按上限生效**：写入超过 `cache_max_mb` 的数据 → 触发淘汰后 `SUM(bytes)` 回落到上限内，
   且被删的是 `last_used_at` 最旧的那些。
5. **`cache_lru_days` 过期**：把一条 `last_used_at` 改到 40 天前 → 淘汰后该行与文件都不在。
   海报是唯一的例外，且必须单独测：过期后**文件删掉、行留着且 `path`/`bytes`/`sha256` 清空、
   `last_used_at` 推到当轮时间**；此后 `poster.jpg` 仍在目录里、GET 200 且恰好回源一次
   （`TestEvictExpiredCoverKeepsRow`、`TestPosterRefetchesAfterSystemEviction`、
   `TestPosterSurvivesSystemEviction`——最后一条走真 handler + 真 DAV 请求）。
6. **single-flight**：并发 10 个同键请求 → 假 CDN 只被调用 1 次；
   并发 3 个 `poster.jpg` 请求 → 封面假 CDN 只被调用 1 次（`internal/cache` 与
   `internal/webdav` 各有一条 `TestPosterConcurrentRequestsFetchOnce`：前者测缓存层，
   后者测「真 handler + 真 DAV 请求」）。
7. **旧后缀不再接受**：`Spec{Kind: segment, Ext: ".ts"}` 被拒绝（防止旧 TS 冒充新片）。
8. **整条合并 playlist 不丢帧**：`ffprobe -count_frames -count_packets` 的
   `frames == packets`（缓存的片段是 0 基，出流才平移 tfdt；`TestMergedPlaylist_DecodesEveryFrame`）。

不测：文件系统异常（磁盘满、权限拒绝）的具体错误文案；跨进程并发。

## 2.0 明确不做

- 缓存预热与预取（把整库提前切好 / 播第 N 集时喂 N+1…）：2847 万集不可能预切。
  1.0 预留的 `seg_prefetch` / `pin_days` 占位键已删除（MKACG-616），代码里没有预取循环。
- 按剧/按用户配额、多级缓存、CDN 回源策略。
- 内容指纹校验 / ETag 回源。
- 缓存统计看板以外的可观测性（`/api/status` 报 `bytes` 与条目数即可）。

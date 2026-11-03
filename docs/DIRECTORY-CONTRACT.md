# 目录契约

状态：定稿 v5（2026-11-02 / MKACG-651）
v2 变更：**删除 `全部/` 一级目录**，顶层直接是分类目录；挂载点 `/dav/` → `/webdav/`。
v3 变更：分类目录下加一层分片目录（D2 已定的形状）。
v4 变更（2026-10-30，S8 / MKACG-612）：**分类名按 `sanitizeName` 转义**（`剧情/古装` →
`剧情_古装`），不再「不转义 + 跳过 + warn」。
**v5 变更（2026-11-02，MKACG-651）：删除分片层。** 树形变为
`/<分类>/<剧名> (<年份>)/{tvshow.nfo, Season 01/…}`；`drama.shard` 列、`ShardOf`/
`ShardCount`、`idx_drama_genre_shard_title` 一并下线，换 `idx_drama_genre_title`。
原因与实测见「单目录子项上限」——分片层不但没必要，还让 Emby/Jellyfin 认不出剧。

前置：`docs/DATA-MODEL.md`（`drama.genre` 是分类键）、`docs/PLAYBACK-CONTRACT.md`（`.strm` 的内容）

## 要交付的形状

```
/webdav/                     ← 只读虚拟 WebDAV
├── 其他/                    ← genre='' 的兜底目录（没有这种剧时不出现）
│   └── <剧名> (<年份>)/
│       ├── tvshow.nfo
│       ├── poster.jpg          ← 仅当这部剧有可用封面（MKACG-650）
│       └── Season 01/
│           ├── <剧名> - S01E01 - <第1集标签>.strm
│           └── <剧名> - S01E01 - <第1集标签>.nfo
├── 家庭/
│   └── <剧名> (<年份>)/…
├── 爱情/
│   └── <剧名> (<年份>)/…
└── 都市/
    └── <剧名> (<年份>)/…
```

- **顶层只有分类目录，没有 `全部/`。** 一部剧只出现在它自己那一个分类目录下
  （`drama.genre` 是单值），所以**同一部剧在树里只出现一次**。
- **分类目录下直接是剧目录**：没有分片层（`00`–`7f` 不存在于任何路径里）。
- DAV 挂载点根 = vfs 的 `短剧`（`internal/vfs/tree.go:Root`；`internal/webdav/fs.go:vfsDir`
  已有映射）。vfs 路径是 `短剧/<类型>/…`，DAV 路径是 `/<类型>/…`。
- 季目录仍只有 `Season 01`（对外剧集的 Season 恒为 1，不改）。
- 剧目录里可以有 `poster.jpg`（Emby / Jellyfin 认的剧集封面名，MKACG-650）：**虚拟文件**，
  字节取自该剧的本地封面缓存（`artifact(kind='cover')`），磁盘上不多出实体文件。
  判据是「这部剧的封面曾经本地化成功」（有 artifact 行）：源未声明封面或同步时下载失败的剧
  没有这个节点，`tvshow.nfo` 也没有 `<thumb>`。文件被手删、或被 `cache_lru_days` 淘汰
  （淘汰只清空行的 `path`/`bytes`/`sha256`，**不删行**）之后节点仍在，请求时按需回源一次
  （取不回来即 404，不产出 0 字节文件）。细节见 `docs/DEV-STANDARDS.md`「poster.jpg」。
- 文件名、`tvshow.nfo`、集 `.nfo` 的内容**一个字都不改**：`embyBase` / `embyLabel` /
  `renderItemNFO` / `renderShowNFO` 全部照旧。`.strm` 的内容也只由
  `strm_prefix` + 当前分块算出的 uid 决定，与本契约的目录形状无关
  （`TestStrmContentIndependentOfPath` 钉住）。
- 分类目录集合 = `SELECT DISTINCT genre FROM drama`；一级目录按 `genre` 的字节序排列，
  剧目录按目录名排序。

## 树形示例（真实数据）

```
/webdav/
├── 其他/                                  ← 仅当有 genre='' 的剧时存在
├── 家庭/
│   └── 嫁进沈家第一天，我把规矩掀了 (2026)/
│       ├── tvshow.nfo
│       ├── poster.jpg                    ← 仅当这部剧有可用封面
│       └── Season 01/
│           ├── 嫁进沈家第一天，我把规矩掀了 - S01E01 - 第1集.strm
│           └── 嫁进沈家第一天，我把规矩掀了 - S01E01 - 第1集.nfo
├── 爱情/
│   └── 一念沉沦，霍总心动了 (2026)/
│       └── Season 01/…
└── 都市/
    └── 重返2000，撕毁协议后我成为沪上巨鳄 (2026)/
        └── Season 01/…
```

`.strm` 的内容（一行）：

```
{strm_prefix}/video/e1234-9f3a1c02b7d4.m3u8
```

## 分类缺失的兜底名

| `drama.genre` | 一级目录 |
|---|---|
| `'爱情'` | `爱情` |
| `''` 或 `NULL` | **`其他`** |

- 兜底名常量 `其他`，只出现在 `internal/vfs` 一处。
- **不产生空目录**：没有这种剧时 `其他` 不存在（`List` 只为有剧的分类出节点；
  分片层删除后「恒列 128 个空分片」这个例外也一并消失了）。
- 上游恰好也有一个分类叫「其他」时，两者合并进同一个目录：区分「上游的其他」与
  「兜底的其他」没有用户价值。

## 分类名按文件名规则转义（2026-10-30 lxz 裁决）

| `drama.genre` | 一级目录 |
|---|---|
| `'爱情'` | `爱情` |
| `'剧情/古装'`（上游脏 `tags[0]`） | `剧情_古装` |
| `'剧情\古装'` | `剧情_古装`（与上一条同名，合并进同一目录） |

- 转义规则就是文件名那套 `sanitizeName`（`/ \ : * ? " < > |` → `_`），与剧名、
  文件名共用一份实现。
- **为什么必须转义**：不转义时这类分类无法成为一个路径段，强行拼进根目录会产出跨段
  节点，`x/net/webdav` 的 `walkFS` 给它 `Stat` 时失败并中止，整份 `multistatus` 被
  截断（客户端拿到非法 XML，整个挂载点不可解析）。上一版口径是「不转义 + 跳过 +
  warn」，代价是这批剧在挂载点里静默不可达；QA 已实测转义后可列可播。
- 转义后同名的两个分类并进同一个目录（与「其他」的合并同理），不区分来源。
- 进入目录时目录名 → genre 取值集合不再是恒等映射：先点查（`GenreExists`），点查
  全落空时按「转义后等于目录名」在全部分类里筛（`ListGenres`）。

## 同名剧冲突规则

冲突的定义：**同一个父目录下出现两个完全相同的子目录名**。父目录是分类目录
（`/<类型>/`），所以同一分类里两条同名同年的剧**必定**冲突。

1. 基准名 `base = sanitizeName(title) + " (" + year + ")"`（现有 `dramaDir`，不改）。
2. 若在同一个父目录内 `base` 重复，则该父目录下**所有**冲突成员改用
   `base + " [" + provider + "-" + provider_id + "]"`。
3. 后缀用 `provider_id` 原文（纯数字、稳定、库内二义性为零），不用截断前缀——
   截断到 6 位在 60 万部规模上会再撞一次。
4. 只有冲突成员加后缀（最小改动），非冲突成员保持 `base`。

**去分片层带来的必然变化**：分片在时冲突面是「一个分片内」（~1/128），现在是一个
分类内（最多 21.6 万部），所以**部分同名同年的剧目录名会从基准名变成带
`[provider-provider_id]` 后缀的形态**。这是契约要求的变化，不是回归；路径本来就全变，
Emby 必须重扫（2.0 一次性代价，不做兼容层）。

为什么必须处理：`UNIQUE(provider, provider_id)` 只保证身份唯一，不保证 `(title, year)`
唯一；60 万部里同名同年必然存在。两条剧映射到同一个目录名，后果是 `Render` 只命中其中
一部、另一部永远不可见。

## 单目录子项上限（2026-11-02，MKACG-651 实测）

去掉分片层的**唯一**代价：分类目录一次 `PROPFIND` 要列出该分类的全部剧目录。
这一节交出实测数字与结论，不做外推替代实测。

### 为什么这不是性能调优，而是二值问题

WebDAV 的 `PROPFIND` **没有分页**。客户端说「列出这个目录」，服务端就必须把目录里
**所有**子项的元数据一次拼成一份 XML 返回。成本由两个东西决定：这个目录有多少子项，
以及**请求的 `Depth` 覆盖多大**。

`Depth` 有三档（`x/net/webdav` 的 `parseDepth` 只认这三个字面量）：

| 请求 | 行为 | 成本 |
|---|---|---|
| `Depth: 0` | 只回资源自己 | O(1) |
| `Depth: 1` | 回直接子项 | O(该目录子项数) |
| `Depth: infinity` **或省略 `Depth` 头** | **递归整棵子树** | O(子树里全部节点) |
| `Depth: 2`（任何数值 ≥2） | `x/net/webdav` 直接回 `400` | — |

**注意第三行**：RFC 4918 §9.1 规定省略 `Depth` 等价于 `infinity`，`x/net/webdav` 的实现
就是这样（`handlePropfind` 里 `depth := infiniteDepth` 是缺头时的默认值）。所以
`curl -X PROPFIND <url>` 这种最省事的试法会递归整棵树——实测数字见下面「不带 `Depth` /
`Depth: infinity`」。rclone mount 与 Emby/Jellyfin 都逐层发 `Depth: 1`，日常挂载不受这条
影响，但这**必须**写进上限结论：它是本服务目前最贵的请求形状。

### 实测环境与复现方式

- 真 PostgreSQL 16 + 真 `vfs.Tree` + 真 `webdav.Handler`（含请求内快照），
  **非 `-race`** 构建（= 线上配置），`Depth: 1`，容器内跑（`podman`）。
- 用例：`internal/webdav/bench_propfind_test.go` 的 `TestPropfindDirectoryScale`，
  默认 `t.Skip`，用环境变量开启：

  ```sh
  # 每个 N 必须单独起一个进程再读 VmHWM：同进程连跑会被前一个 N 的峰值污染。
  DRAMA_HUB_BENCH=1 DRAMA_HUB_BENCH_MAX=100000 DRAMA_HUB_BENCH_GENRES=68 \
    go test ./internal/webdav/ -run '^TestPropfindDirectoryScale$' -v
  ```

- 数据造法：最大分类「剧情」放 N 部（标题各不相同，避免冲突后缀），其余 67 个分类
  各 20 部，`ANALYZE` 后测。响应体写进一个只数字节的 `ResponseWriter`——不用
  `httptest.ResponseRecorder`（后者自己缓存整份响应体，会把内存算到服务端头上）。
- 对照组「DB 窄列」= `store.ListDramaDirsByGenre`（`id/provider/provider_id/title/year/
  genre`），「DB 全列」= `store.ListDramas`（整库、含 `plot`/`tags`）——用来量化
  `Drama.Plot/Tags` 物化的占比。

**两套测量，口径不同，别混用**：

| 口径 | 怎么测 | 能回答 | 不能回答 |
|---|---|---|---|
| **进程内 bench**（`TestPropfindDirectoryScale`） | 测试进程里先灌库再发一次请求 | 服务端 CPU / 分配 / 堆峰值、DB 查询耗时 | **进程 RSS 增量**——灌库已把基线抬到 84–140 MB，请求本身的增量就被吃掉了 |
| **冷启动真服务**（本节的数字） | 真 `postgres:16` + 真二进制容器，读数取 `/proc/1/status` 的 `VmHWM`，每次测量前重启服务 | 线上视角的墙钟、响应体、**RSS 增量** | 细粒度的堆分配 |

**本节的表与结论一律用「冷启动真服务」口径**（`VmHWM` 是相对冷启动基线的增量）。
早期版本用进程内 bench 的增量（11–15 MB / 13–40 MB）当结论，被功能检测指出低估 3–5 倍
——那个数字只是「同进程里灌完库再跑请求」的差值，不是线上增量。bench 口径保留在
`internal/webdav/bench_propfind_test.go` 里量 DB 与分配，不再用于 RSS。

### 实测数据（冷启动真服务）

镜像与 commit 同 PR head，语料用 SQL 直接造（`drama` / `episode` / 当时的 `emby_item`；
ADR-0014 删表后同一份语料只需 `drama` / `episode`，对外剧集读时现算），
`ANALYZE` 后测；每次请求前重启服务，读数取容器内 `/proc/1/status` 的 `VmHWM`。
语料里**没有 `artifact(kind='cover')` 行**，所以树里没有 `poster.jpg` 节点（MKACG-650）；
有封面时每个剧目录多一个节点 + 一次封面行点查，成本按节点数线性增加，不会改变下面的
量级结论。

**`Depth: 1`（客户端正常形状：rclone / Emby / Jellyfin 都这么扫）**

| 分类目录子项 N | 响应体 | 墙钟 | VmHWM 增量 |
|---|---|---|---|
| 100（当前真实最大分类量级） | 0.1 MB | 3–4 ms | 0 MB |
| 10,000 | 5.4 MB | 0.20–0.31 s | 1–2 MB |
| 100,000 | 52.9 MB | 1.99 s | **68.0 MB** |
| 216,000（L1 最大分类外推） | 114.7 MB | 4.06 s | **136.9 MB** |

**不带 `Depth` 头 / `Depth: infinity`（= 递归整棵子树）**

| 场景 | 响应体 | 墙钟 | VmHWM 增量 |
|---|---|---|---|
| 100,000 剧（无集） | 170.4 MB | 53.4 s | **234.2 MB** |
| 216,000 剧（无集） | 369.0 MB | 115.9 s | **500.6 MB** |
| 216,000 剧（无集），显式 `Depth: infinity` | 369.0 MB | 120.2 s | 502.7 MB |
| 5,000 剧 × 10 集（50,000 对外剧集，一个分类） | 83.3 MB | 58.2 s | （未测） |

同一分类 5,000 剧 × 10 集的 `Depth: 1` 只要 2.6 MB / 0.10 s —— 也就是说
**同一份数据，换一个请求形状，代价差 ~30 倍墙钟、~32 倍响应体**。按 1.6 KB/对外剧集
线性外推，L1 全量（21.6 万剧 × 46.8 集 ≈ 1000 万 item）的整树递归是 **10 GB 级响应**；
那不是实测，但方向与量级足以说明这种形状不能当正常用法。

`Drama.Plot/Tags` 物化的代价（进程内 bench 口径，同一数据集、同一分类；只看 DB 与堆，
不看 RSS）：

| 最大分类 N | 全列（含 plot/tags） | 窄列 | 差额 |
|---|---|---|---|
| 100,000 | 271–289 ms / 堆峰值 41–43 MB | 190–204 ms / 25 MB | ≈40% 堆峰值 |
| 216,000 | 546–772 ms / 堆峰值 100–130 MB | 362–421 ms / 47–51 MB | ≈50% 堆峰值 |

结论：**分类列举改用窄列**（`store.DramaDir`）——它把 216k 子项列举的堆峰值从
~100–130 MB 压到 47–51 MB，而渲染路径不受影响（它本来就走 `(title, year)` /
`(provider, provider_id)` 点查拿完整 `Drama`）。

### 判据与结论

判据线（本任务 PL 定的）：墙钟 ≤ 5 s、VmHWM 增量 ≤ 200 MB、响应体 ≤ 100 MB。

**`Depth: 1`（正常形状）**

| 判据 | 216,000 实测 | 结论 |
|---|---|---|
| 墙钟 ≤ 5 s | 4.06 s | 通过，**余量只剩 ~19%** |
| VmHWM 增量 ≤ 200 MB | 136.9 MB | 通过 |
| 响应体 ≤ 100 MB | 114.7 MB | **超 15%** |

**不带 `Depth` / `infinity`（递归整棵子树）**

| 判据 | 216,000 实测 | 结论 |
|---|---|---|
| 墙钟 ≤ 5 s | 115.9–120.2 s | **超 23 倍** |
| VmHWM 增量 ≤ 200 MB | 500.6–502.7 MB | **超 2.5 倍** |
| 响应体 ≤ 100 MB | 369.0 MB | **超 3.7 倍** |

**结论**：

1. `Depth: 1` 的成本对子项数**线性**：10 万级 2.0 s / 68 MB，21.6 万级 4.1 s / 137 MB。
   墙钟与内存都还在判据线内，但**余量不大**（墙钟剩 ~19%、内存剩 ~32%）；越线的是
   响应体（21.6 万时 114.7 MB，超 15%）。
2. **整树递归（省略 `Depth` 或 `infinity`）三项全越线，且量级不同**：21.6 万剧时
   369 MB / 116 s / +501 MB。它不是「慢一点」，是「不能用」——任何省略 `Depth` 的
   WebDAV 客户端（`curl -X PROPFIND` 是最常见的一种）都会落到这个形状上。
3. 服务端能控的只有「别为同一份响应多花内存」——已做（窄列 + 请求内快照）；
   响应体大小是 `PROPFIND` 协议本身的下限（无分页），改不动。
4. **超线后的应对路径（不恢复分片层、不在本任务内改 `Depth` 支持）**，按证据强度排序：
   - **客户端必须带 `Depth: 1`**：rclone mount / Emby / Jellyfin 本来就这么扫，实测
     `rclone lsf -R` 正常；`curl` 之类的排障请求也要显式带 `-H 'Depth: 1'`。
   - **Emby/Jellyfin 一个库挂多个 media folder，每个分类一个**（68 个目录）：每个分类
     目录的子项数就是该分类的剧数，21.6 万这种规模只出现在「剧情」这一类上，而它本身
     就是 L1 最坏的假设。纯客户端配置，**不需要本服务改任何东西**。
   - 次选：给单个超大分类单独限库（同上，只是把「剧情」挪进一个独立库）。
   - **若要真的挡住整树递归**（缺头/`infinity` 一律 `400` 或按 1 处理），必须改
     `internal/webdav` 的 `Depth` 处理——那超出本任务的契约（Planner 明确 MUST NOT
     「不改 `Depth` 支持」），需要 PL 另开任务决策。本任务只把它**测出来并写进上限**。
   - 不建议：恢复分片层——Jellyfin 会把分片目录当 Season（见「被推翻的做法」）。
5. 脏标题的单文件 `Render`（`locateDrama` 点查不中 → 分类内比对）实测：
   21.6 万部规模下 **0.77–1.41 s/文件**（10 万部规模 0.36–0.39 s）。这类剧
   （标题含 `/ : * ?`）在真实库里是少数；若某天量大到影响扫描，升级路径是加**派生列或
   函数索引**（把 sanitize 后的目录名落成一列并建索引），**不要**再退回分片层。

### 被推翻的做法（v3 的 D2）

v3 曾定「分类目录下加一层纯函数分片目录（`00`–`7f`，N=128）」，理由是让单个目录的子项
数落回「≤3000」。**2026-11-02 在 nas 上读 `jellyfin.db` 实测推翻了它**：

- Jellyfin 把**分类目录**当 Series、把**分片目录**当 Season（`东方仙侠/66` →「第 66 季」），
  真正的 1436 个剧目录退化成普通 Folder；
- 也就是说分片层不但没解决「扫不进去」，反而让 Emby/Jellyfin **完全认不出剧**。

推论也一并落实：**「分类 = Series」这个根因不在树形状，而在库根挂在哪**。库根挂
`/webdav`（顶层就是分类目录）时，分类目录一定被当 Series；库根挂到具体分类目录
（每个分类一个 media folder）才是正确配法。分片层是否存在于这个结论无关，所以去掉它
不损失任何能力。

## Emby / Jellyfin 侧影响（逐条回答）

| 问题 | 答案 |
|---|---|
| 会产生重复条目吗？ | **不会。** 每部剧只在它的分类目录下出现一次（没有 `全部/` 了）。 |
| 库怎么配？ | **库根 = 分类目录，每个分类一个 media folder**（68 个，真实 `genre` 词表是 68 而非 19）。这是唯一能扫出「剧集列表」的配法。库根挂 `/webdav` 时分类目录会被当 Series（nas 实测：59 部剧 = 分类数），见上「被推翻的做法」。 |
| 会再出现「第 66 季」吗？ | **不会。** 分片目录已不在路径里；Season 只有 `Season 01`。 |
| 集号会变吗？ | **不会。** 集号来自「原始集 + 当前 `merge_chunk_size`」的现算（硬约束 6），本契约只改父级目录。 |
| 已有库要重建吗？ | 要。路径全变，Emby 会当新库重扫。2.0 一次性代价，不做路径兼容层。 |
| rclone 侧有影响吗？ | `--max-depth` 从 5 降到 4（`分类/剧/Season 01/文件`）；**不再需要 `--vfs-cache-mode full`**（播放不经过 rclone，见 ADR-0007）。 |
| 分类变了怎么办？ | 剧会在 WebDAV 里换目录（上游 `tags[0]` 变了就变）。Emby 表现为「旧条目消失 + 新条目出现」。这是上游行为的忠实反映，不做目录名稳定性妥协。 |
| 合并集有影响吗？ | 没有。合并集仍是一个 `Season 01/<文件名>` 对。 |

## MUST：目录列举必须是「一个目录一次查询」

**旧实现（会直接炸）**：`vfs.Tree.List(ctx)` 枚举**整棵树**——1 次 `ListDramas` +
每剧 1 次 `ListEmbyItems`，而 `internal/webdav/fs.go` 的 `Stat` / `OpenFile`
**每个请求都调它**。1.0 的库只有几部剧，看不出来。

**在 L1 规模下**：608,372 部 × 46.8 集 × 2 文件 ≈ **5,700 万个节点**要在**一次请求**里
构建出来。这不是慢，是不可能。

**契约**：

```go
// List 只返回某一个目录的直接子项，不返回孙子及以下。
// dir 是相对 vfs 根的路径（"" = vfs 根）；不存在的目录 → ErrNotFound。
// 成本：根目录 1 次 DISTINCT genre；分类目录 1–2 次 genre 存在性点查 + 1 次
//       WHERE genre IN (…) ORDER BY title；Season 目录 1 次 ListEmbyItems。
//       主路径与全库规模无关。唯一退化：分类目录名不是库内 genre 原值（脏分类被转义的
//       形态，如 `剧情_古装`）以致点查全落空时，genresForDir 退回一次 ListGenres 全索引扫
//       （60 万行约 74ms）——只在脏分类与不存在的目录上付，见「分类名按文件名规则转义」。
List(ctx context.Context, dir string) ([]Node, error)
```

- `internal/webdav/fs.go` 的 `Tree` 接口跟着改；`Stat` / `OpenFile` 只取直接子项。
  请求内快照（`snapshot`）保留，键是「列过的目录」——它是 PROPFIND 保持线性的
  **唯一**前提（绕过它即 O(n²)：`x/net/webdav` 的 `walkFS` 会对每个子项再 `Stat` 一次，
  实测 1 万子项 79 s、分配 54 GB）。
- 剧目录名 → `drama_id` 的映射必须走索引：`WHERE title = ? AND year = ?`
  （`idx_drama_title`）或后缀形态下 `WHERE provider=? AND provider_id=?`。
  **不许再 `ListDramas` 全表扫**。

## 现有实现的落点

| 位置 | 现状 |
|---|---|
| `internal/vfs/tree.go` | `List` 4 层（根/分类/剧/Season）；`parsePath` 段数 4/5（`短剧/<分类>/<剧>/tvshow.nfo` = 4，`…/Season 01/<文件>` = 5） |
| `internal/vfs/tree.go:embyBase` / `embyLabel` / `renderItemNFO` / `renderShowNFO` | **未改**（内容与命名零改动） |
| `internal/store/directory.go` | `ListDramaDirsByGenre(ctx, genres)`（窄列，分类目录列举）、`ListGenres`、`GenreExists`、`DramaByTitleYear` |
| `internal/store/directory.go:DramaDir` | 分类列举的窄行（`id/provider/provider_id/title/year/genre`），见「单目录子项上限」 |
| `internal/webdav/fs.go` | **未改**（分片层从来不在它这一层） |
| `scripts/acceptance.sh` | D3 的形状正则改成 4 段；D1/E1 的注释 |

## 2.0 明确不做

- 按年份/按源的目录层（分类之下直接是剧）。
- `全部/` 汇总目录（本次按 lxz 口径删除；要「全部」用 Emby 的多文件夹挂载）。
- **任何分片/中间目录层**（v5 明令删除，见「被推翻的做法」）。
- 软链接、跨分类共享目录（虚拟只读，WebDAV 没有符号链接语义）。
- 剧目录名带集数、简介等元数据。
- 目录路径兼容层（老路径与新路径同时可用）。

# 目录契约

状态：定稿 v3（2026-10-30 / MKACG-604）
v2 变更：**删除 `全部/` 一级目录**，顶层直接是分类目录；挂载点 `/dav/` → `/webdav/`。
v3 变更：**D2 已定 —— 分类目录下加一层分片目录**（原因见「D2」）；分片数 N 待 S4 实测校准。
v4 变更（2026-10-30，S8 / MKACG-612）：**分类名按 `sanitizeName` 转义**（`剧情/古装` →
`剧情_古装`），不再「不转义 + 跳过 + warn」；见「分类名按文件名规则转义」。
前置：`docs/DATA-MODEL.md`（`drama.genre` 是分类键）、`docs/PLAYBACK-CONTRACT.md`（`.strm` 的内容）

## 要交付的形状

```
/webdav/                     ← 只读虚拟 WebDAV
├── 其他/                    ← genre='' 的兜底目录（没有这种剧时不出现）
│   └── <分片 00–7f>/        ← 纯函数分片，见「D2」
│       └── <剧名> (<年份>)/
│           ├── tvshow.nfo
│           └── Season 01/
│               ├── <剧名> - S01E01 - <第1集标签>.strm
│               └── <剧名> - S01E01 - <第1集标签>.nfo
├── 家庭/
│   └── <分片>/<剧名> (<年份>)/…
├── 爱情/
│   └── <分片>/<剧名> (<年份>)/…
└── 都市/
    └── <分片>/<剧名> (<年份>)/…
```

- **顶层只有分类目录，没有 `全部/`。** 一部剧只出现在它自己那一个分类目录下
  （`drama.genre` 是单值），所以**同一部剧在树里只出现一次**。
- **分类与剧之间有一层纯函数分片目录**（`00`–`7f`，128 个）：`shard = fnv32a(provider_id) % N`，
  `N` 是代码常量（默认 128），不落配置、不依赖时间、剧改名也不换分片。
  理由见「D2」——这是让 Emby 能扫描的前提，不是可选项。
- **分片层恒列 `00`–`7f` 全部 128 个，包括该分类里一部剧都没有的空分片。**
  这是实际行为，不是回归：分片集合是代码常量（`store.ShardCount`），列空分片不发生任何
  查询；换成 `SELECT DISTINCT shard` 则每次列分类都要扫该分类的全部索引行（183k 行约
  20–40 ms，随分类规模线性增长）。代价是分类目录下永远有 128 个子项，其中空的是空目录。
  「不产生空目录」（下一节）约束的是**分类层**，不是分片层。
- DAV 挂载点根 = vfs 的 `短剧`（`internal/vfs/tree.go:root`；`internal/webdav/fs.go:resolve`
  已有映射）。vfs 路径是 `短剧/<类型>/…`，DAV 路径是 `/<类型>/…`。
- 季目录仍只有 `Season 01`（`emby_item.season` 恒为 1，不改）。
- 文件名、`tvshow.nfo`、集 `.nfo` 的内容**一个字都不改**：`embyBase` / `embyLabel` /
  `renderItemNFO` / `renderShowNFO` 全部照旧。本契约只改「剧目录挂在哪一级」
  与「`.strm` 里写什么」（后者见 `docs/PLAYBACK-CONTRACT.md`）。
- 分类目录集合 = `SELECT DISTINCT genre FROM drama`；一级目录按 `genre` 的字节序排列，
  剧目录按目录名排序。

## 树形示例（真实数据）

```
/webdav/
├── 其他/                                  ← 仅当有 genre='' 的剧时存在
├── 家庭/
│   ├── 0a/
│   │   └── 嫁进沈家第一天，我把规矩掀了 (2026)/
│   │       ├── tvshow.nfo
│   │       └── Season 01/
│   │           ├── 嫁进沈家第一天，我把规矩掀了 - S01E01 - 第1集.strm
│   │           └── 嫁进沈家第一天，我把规矩掀了 - S01E01 - 第1集.nfo
│   └── 3f/ …
├── 爱情/
│   ├── 1c/
│   │   └── 一念沉沦，霍总心动了 (2026)/
│   │       └── Season 01/…
│   └── 5e/ …
└── 都市/
    └── 72/
        └── 重返2000，撕毁协议后我成为沪上巨鳄 (2026)/
            └── Season 01/…
```

（分片号是 `fnv32a(provider_id) % 128` 的两位十六进制，只保证均匀与稳定，
**不承载任何业务含义**——不要试图从中推断任何东西。）

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
- **不产生空目录（只对分类层成立）**：没有空分类的剧时 `其他` 不存在（`List` 只为有剧的
  分类出节点）。分片层例外：分类目录恒列 `00`–`7f` 全部 128 个，空分片就是空目录，
  理由见上一节与 `docs/DEV-STANDARDS.md`「列举成本」。
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

冲突的定义：**同一个父目录下出现两个完全相同的子目录名**。加了分片层之后，
父目录是 `/<类型>/<分片>/`，所以两条同名同年的剧若落在不同分片，**不构成冲突**，
两边都用基准名——冲突面反而变小了。

1. 基准名 `base = sanitizeName(title) + " (" + year + ")"`（现有 `dramaDir`，不改）。
2. 若在同一个父目录内 `base` 重复，则该父目录下**所有**冲突成员改用
   `base + " [" + provider + "-" + provider_id + "]"`。
3. 后缀用 `provider_id` 原文（纯数字、稳定、库内二义性为零），不用截断前缀——
   截断到 6 位在 60 万部规模上会再撞一次。
4. 只有冲突成员加后缀（最小改动），非冲突成员保持 `base`。

为什么必须处理：`UNIQUE(provider, provider_id)` 只保证身份唯一，不保证 `(title, year)`
唯一；60 万部里同名同年必然存在。两条剧映射到同一个目录名，后果是 `Render` 只命中其中
一部、另一部永远不可见。

## Emby 侧影响（逐条回答）

| 问题 | 答案 |
|---|---|
| 会产生重复条目吗？ | **不会。** 每部剧只在它的分类目录下出现一次（没有 `全部/` 了）。 |
| Emby 库怎么配？ | **一个「剧集」库，库根 = `/webdav`**，Emby 逐层往下扫（`分类/分片/剧/Season 01`）。不需要逐个挂 19×128 个文件夹。若 Emby 在 `/webdav` 上不能正确识别（把分类当成剧），退一步挂具体分类目录（`/webdav/爱情` 等，19 个，可接受）。**这条是 S7 的验收项，必须实测**——两种配法本服务的树都支持。 |
| 集号会变吗？ | **不会。** 集号来自 `emby_item.ep_no`（硬约束 6），本契约只改父级目录。 |
| 已有库要重建吗？ | 要。路径全变（多一级分类目录），Emby 会当新库重扫。2.0 一次性代价，不做路径兼容层。 |
| rclone 侧有影响吗？ | `--max-depth` +1（`docs/ACCEPTANCE.md` P2 的 `--max-depth 3` → 4）；**不再需要 `--vfs-cache-mode full`**（播放不经过 rclone，见 ADR-0007）。 |
| 分类变了怎么办？ | 剧会在 WebDAV 里换目录（上游 `tags[0]` 变了就变）。Emby 表现为「旧条目消失 + 新条目出现」。这是上游行为的忠实反映，不做目录名稳定性妥协。 |
| 合并集有影响吗？ | 没有。合并集仍是一个 `Season 01/<文件名>` 对。 |

## MUST：目录列举必须是「一个目录一次查询」

**现状（会直接炸）**：`vfs.Tree.List(ctx)` 枚举**整棵树**——1 次 `ListDramas` +
每剧 1 次 `ListEmbyItems`（`internal/vfs/tree.go:dramaEntries`），而
`internal/webdav/fs.go` 的 `Stat` / `OpenFile` **每个请求都调它**（`f.nodes(ctx)`）。
1.0 的库只有几部剧，看不出来。

**在 L1 规模下**：608,372 部 × 46.8 集 × 2 文件 ≈ **5,700 万个节点**要在**一次请求**里
构建出来。这不是慢，是不可能。

**契约**：

```go
// List 只返回某一个目录的直接子项，不返回孙子及以下。
// dir 是相对 vfs 根的路径（"" = vfs 根）；不存在的目录 → ErrNotFound。
// 成本：根目录 1 次 DISTINCT genre；分类目录 1–2 次 genre 存在性点查（分片集合是
//      代码常量，恒回 00–7f，不查库）；
//      分片目录 1 次 WHERE genre IN (…) AND shard=? 查询；Season 目录 1 次 ListEmbyItems。
//      主路径与全库规模无关。唯一退化：分类目录名不是库内 genre 原值（脏分类被转义的
//      形态，如 `剧情_古装`）以致点查全落空时，genresForDir 退回一次 ListGenres 全索引扫
//      （60 万行约 74ms）——只在脏分类与不存在的目录上付，见「分类名按文件名规则转义」。
List(ctx context.Context, dir string) ([]Node, error)
```

- `internal/webdav/fs.go` 的 `Tree` 接口跟着改；`Stat` / `OpenFile` 只取直接子项。
  请求内快照（`snapshot`）保留，键从「整棵树」改成「已列过的目录」。
- 剧目录名 → `drama_id` 的映射必须走索引：`WHERE title = ? AND year = ?`（`idx_drama_title`）
  或后缀形态下 `WHERE provider=? AND provider_id=?`。**不许再 `ListDramas` 全表扫**
  （现有 `dramaByDir` 是线性匹配）。

## D2（已定）：分类目录下必须有分片层

### 问题是什么（一句话）

WebDAV 的 `PROPFIND` **没有分页**。客户端说「列出这个目录」，服务端就必须把目录里
**所有**子项的元数据一次拼成一份 XML 返回。所以「一个目录里放多少东西」不是性能调优，
而是**能不能列出来**的二值问题。

### 会具体怎么炸

`golang.org/x/net/webdav@v0.62.0/webdav/file.go:787` 的 `walkFS`：

```go
fileInfos, err := f.Readdir(0)   // 0 = 要全部，拿到内存里
for _, fi := range fileInfos {
    fi, err := fs.Stat(ctx, filename)   // 逐个再 Stat 一次
```

删掉 `全部/` 之后，最大的分类目录里有 **≈ 21.6 万个剧目录**
（`/category` 800 抽样里「爱情」占 35.5%，按 608,372 部推算；19 个分类平均 3 万）。
Emby 扫到 `/webdav/爱情` 时会发生：

1. 服务端先在内存里造出 21.6 万个 `FileInfo`（约 22 MB），
2. 再对每一个调一次 `Stat`（21.6 万次），
3. 再拼成一份约 **65 MB** 的 XML 一次性写出去。

`PROPFIND Depth:1` 于是超时/被 Emby 判定失败，扫描停在这一层——**这部剧库根本建不起来**。
Emby 不会「分批慢慢扫」，协议层没有这个选项。

### 定下来的做法

`/<类型>/<分片>/<剧>/…`，`shard = fnv32a(provider_id) % N`，分片目录名是两位十六进制
（`00`–`7f`）。默认 `N = 128`。

- 后果：最大分类 ≈ 216k/128 ≈ **1.7k** 个子项/目录，一份 XML 约 500 KB，Emby 正常。
- 分片是**纯函数**：不落配置、不依赖时间、不随剧改名变化（用 `provider_id` 而非 `title`）。
- 用户看不出规律，也不需要看出规律：它只是把一批目录摊薄，没有业务含义。
- 代价只有一处：目录深了一层；rclone `--max-depth` 与 `lsf` 的层数各 +1。
  **Emby 侧不用多配**：库根仍是 `/webdav`，Emby 会逐层往下扫。

### `N` 怎么定（S5 已用真实数据校准）

`N` 是代码常量，不是 schema。目标只有一个：**最大分类的子项数 / N ≤ 3000**。

S5（MKACG-607）用真实源灌了 17,558 部后跑
`SELECT genre, COUNT(*) FROM drama GROUP BY genre ORDER BY 2 DESC`：

| 量 | 实测（17,558 部样本） | 按样本占比外推到 608,372 部 |
|---|---|---|
| 分类数（`genre` 不同值） | 68 | 68（分类集合是封闭词表，不随样本增长） |
| 最大分类 | `剧情` 5,282 部（30.1%） | ≈ 183,000 部 |
| 最大分类 / `N=128`（均值） | 41.3 部/分片 | ≈ 1,430 部/分片 |
| 最大分类 / 单分片最多 | 55 部（比均值高 1.33×，`fnv32a` 分布偏斜） | ≈ 1,900 部/分片 |
| 第二大分类 `脑洞` | 1,811 部（10.3%） | ≈ 62,700 部 → `N=128` ≈ 490 部/分片 |

**结论：`N = 128` 保持不动。** 最大分片目录外推 ≈ 1,900 子项，仍在「≤ 3000」的判据内
（也贴着「≈ 1.7k」的经验目标）。若 S6 想要更大余量，改 `N=256` 则 ≈ 950 子项，代价是一条
`UPDATE drama SET shard = …` 重算 + 重扫 Emby，**不改 schema**、不改分片函数。

**另一个 S6 要用的数**：分类目录不是 19 个而是 **68 个**（真实 `genre` 词表），
所以「退一步逐个挂分类目录」那条备选方案是 68 个挂载点，不是 19 个。

### 被否决的两个做法

| 做法 | 为什么不 |
|---|---|
| 不分片、按现在的形状全量暴露 | Emby 在分类层就扫不动，stage 4 的产物等于不可用 |
| WebDAV 只暴露「显式上架」的剧 | 要新增「上架」概念 + 一张表 + 管理面开关，而且把「全部」变成「部分」，与 L1 口径冲突 |

### `0002` 里的落地

分片键要能进 SQL（否则 `WHERE shard=?` 只能全表扫），因此存一列：

```sql
-- 目录分片键：fnv32a(provider_id) % N，写库时由框架算好存下。
-- 只服务分类目录的分页列举，不参与任何业务语义；改 N 时一条 UPDATE 重算。
ALTER TABLE drama ADD COLUMN shard INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_drama_genre_shard_title ON drama (genre, shard, title);
```

`idx_drama_title` 仍然保留：`Render` 用 `WHERE title = ? AND year = ?` 做点查，
那是索引寻址，不需要经过分片（分片只用于「列举一个目录」）。

### 对 Emby 与扫描成本的老实话

- 库根 `/webdav`，Emby 递归下去，**不需要挂 19 个文件夹，也不需要挂 128×19 个**。
- 但全量入库 = Emby 里有 **60 万个剧目录**。一次完整扫描要逐个 `PROPFIND` 剧目录，
  量级是十几 GB 的 XML 与数小时到数天的机器时间，这是 L1「全部」自带的代价，
  分片解决的是「扫不进去」，不解决「扫得慢」。
- 若嫌慢：Emby 库里只挂你想看的几个分类目录（`/webdav/爱情` 等），
  这是 Emby 自带能力，不需要改本服务任何东西。
## 现有实现的改动清单（给 S6 的输入）

| 位置 | 改动 |
|---|---|
| `internal/webdav/handler.go` | `MountPath = "/dav/"` → `"/webdav/"`（PROPFIND 的 href 由它生成，无第二处字面量）；测试与 `cmd` 的注册跟着改 |
| `internal/vfs/tree.go` | 顶层改成分类目录 + 分片层；`List` → `List(ctx, dir)`；`parsePath` 段数 3/4 → 5/6（`短剧/<分类>/<分片>/<剧>/tvshow.nfo` = 5，`…/Season 01/<文件>` = 6）；`dramaByDir` 改走 `idx_drama_title` |
| `internal/vfs/render.go` | `renderStrm` 改成 `{strm_prefix}/video/{id}.m3u8`（`docs/PLAYBACK-CONTRACT.md`） |
| `internal/vfs/tree.go:embyBase` / `embyLabel` / `renderItemNFO` / `renderShowNFO` | **不动** |
| `internal/webdav/fs.go` | `Tree` 接口改签名；`nodes()` 按路径取子项；`snapshot` 键改成目录 |
| `internal/store/crud.go` | 新增 `ListGenres(ctx)`（`SELECT DISTINCT genre`）、`ListDramasByGenreShard(ctx, genre, shard, page)`、剧名→id 的索引查询；`shard` 列在 `UpsertDrama` 里按 `fnv32a(provider_id) % N` 写入 |
| `docs/ACCEPTANCE.md` | 路径 `/dav/` → `/webdav/`；`--max-depth` +1；Emby 库配法改成「一个库多个媒体文件夹」 |

## 2.0 明确不做

- 按年份/按源的目录层（分类之下直接是剧或分片）。
- `全部/` 汇总目录（本次按 lxz 口径删除；要「全部」用 Emby 的多文件夹挂载）。
- 软链接、跨分类共享目录（虚拟只读，WebDAV 没有符号链接语义）。
- 剧目录名带集数、简介等元数据。
- 目录路径兼容层（老路径与新路径同时可用）。

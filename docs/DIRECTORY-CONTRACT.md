# 目录契约

状态：定稿（2026-10-30，MKACG-604 / stage 1），**含 1 个待 PL 拍板项（D2）**
前置：`docs/DATA-MODEL.md`（`drama.genre` 是分类键）、`docs/L1-CORPUS-SCOPE.md`（规模）

## 要交付的形状（lxz 口径，不解释、不改）

```
<DAV 根>/
├── 全部/
│   └── <剧名> (<年份>)/
│       ├── tvshow.nfo
│       └── Season 01/
│           ├── <剧名> - S01E01 - <标签>.strm
│           └── <剧名> - S01E01 - <标签>.nfo
└── <类型>/
    └── <剧名> (<年份>)/
        └── …（同上）
```

- DAV 挂载点根 = vfs 的 `短剧`（`internal/vfs/tree.go:root`，`internal/webdav/fs.go:resolve`
  已有映射）。所以 vfs 路径是 `短剧/全部/…`、`短剧/<类型>/…`，DAV 路径是 `/全部/…`、`/<类型>/…`。
- **同一部剧在「全部」与它自己的类型目录下各出现一次**，内容逐字节相同。
- 季目录仍只有一个 `Season 01`（`emby_item.season` 恒为 1，不改）。
- 文件名与 NFO 内容**一个字都不改**：`embyBase` / `embyLabel` / `renderItemNFO` /
  `renderShowNFO` 全部照旧。目录契约只改「剧目录挂在哪一级」。
- 类型目录的集合 = `SELECT DISTINCT genre FROM drama`，再加上兜底名「其他」。
- 一级目录顺序：`全部` 在前，其余按 `genre` 的字节序；剧目录按它自己那级的目录名排序。

## 树形示例（真实数据，取 4 部）

```
/
├── 全部/
│   ├── 一念沉沦，霍总心动了 (2026)/
│   │   ├── tvshow.nfo
│   │   └── Season 01/
│   │       ├── 一念沉沦，霍总心动了 - S01E01 - 第1集.strm
│   │       └── 一念沉沦，霍总心动了 - S01E01 - 第1集.nfo
│   └── 除夕归家路漫漫 (2026)/
│       └── Season 01/ …
├── 其他/                         ← 只有 genre='' 的剧会出现在这里
│   └── … 
├── 家庭/
│   └── 嫁进沈家第一天，我把规矩掀了 (2026)/
│       └── Season 01/ …
├── 爱情/
│   └── 一念沉沦，霍总心动了 (2026)/     ← 与 /全部/ 下同名同内容
│       └── Season 01/ …
└── 都市/
    └── 重返2000，撕毁协议后我成为沪上巨鳄 (2026)/
        └── Season 01/ …
```

`全部/` 与 `/<类型>/` 是同一批剧的两种索引：每部剧在两个父目录下各有一个条目，
因此**剧目录条目数是剧数的约 2 倍**（60 万部 → 约 120 万个目录条目）。
`strm`/`nfo` 的内容按请求渲染、不落盘、不预生成，所以渲染成本不因出现两次而增加
（同一路径在一次请求内只渲染一次，`internal/webdav` 的请求内快照保证）。

## 分类缺失的兜底名

| `drama.genre` | 一级目录 |
|---|---|
| `'爱情'` | `爱情` |
| `''` 或 `NULL` | **`其他`** |

- 兜底名常量 `其他`，只出现在 `internal/vfs` 一处。
- `其他` 目录**恒存在吗**？—— **不**。没有空分类的剧时，不产生空目录：
  Emby 遇到空目录只是多扫一次，但「本服务凭空造目录」会让用户以为有数据。
  这与现有实现一致（`List` 只为由剧产生的目录出节点）。
- `其他` 与某个上游分类真叫「其他」时合并进同一个目录：分类名不做转义，
  因为它们是**目录名**，而目录名已经过 `sanitizeName`；人为区分「上游的其他」与
  「兜底的其他」没有用户价值。

## 同名剧冲突规则

冲突的定义：**同一个父目录下出现两个完全相同的子目录名**。

1. 基准名 `base = sanitizeName(title) + " (" + year + ")"`（现有 `dramaDir`，不改）。
2. 若在**同一个父目录**内 `base` 重复，则该父目录下**所有**冲突成员都改用
   `base + " [" + provider + "-" + provider_id + "]"`。
3. 后缀用的是 `provider_id` 原文（纯数字、稳定、库内二义性为零），**不是**截断前缀：
   截断到 6 位在 60 万部规模上会再撞一次。
4. 只有冲突成员加后缀，非冲突成员保持 `base`——规则是「最小改动」，不是为了统一好看。

为什么必须处理：`UNIQUE(provider, provider_id)` 只保证身份唯一，不保证
`(title, year)` 唯一；60 万部里同名同年的剧一定存在（短剧重名率高于长剧）。
两条剧映射到同一个目录名，后果是 `Render` 只命中其中一部、另一部永远不可见。

## Emby 侧影响（逐条回答）

| 问题 | 答案 |
|---|---|
| 集号会变吗？ | **不会。** 集号来自 `emby_item.ep_no`（硬约束 6），目录契约只改剧目录的父级，不碰派生表。 |
| 会产生重复条目吗？ | **取决于 Emby 库路径。** 库路径若指向 DAV 根 `/`，每部剧会被扫到两次（`/全部/<剧>` 与 `/<类型>/<剧>`）→ 产生重复条目。**Emby 库路径 MUST 指向 `/全部` 或某一个 `/<类型>`，MUST NOT 指向 DAV 根。** 这一条要写进 `docs/ACCEPTANCE.md` 与 README。 |
| 已有库要重建吗？ | 要。剧目录多一级父目录，路径全变，Emby 会当成新库重扫。这是 2.0 的一次性代价，不做路径兼容层（兼容层要同时在两处渲染同一部剧，与「虚拟、按请求渲染」的成本模型冲突）。 |
| rclone 侧有影响吗？ | `--max-depth` 要 +1；`docs/ACCEPTANCE.md` P2 的 `rclone lsf --max-depth 3` 改成 4。 |
| 合并集有影响吗？ | 没有。合并集仍然只是一个 `Season 01/<文件名>` 对。 |

## MUST：目录列举必须是「一个目录一次查询」

这是本契约里唯一动到架构的一条，必须写清楚。

**现状（会直接炸）**：`vfs.Tree.List(ctx)` 枚举**整棵树**——1 次 `ListDramas` +
每剧 1 次 `ListEmbyItems`（`internal/vfs/tree.go:dramaEntries`），并且
`internal/webdav/fs.go` 的 `Stat` / `OpenFile` **每个请求都调它**（`f.nodes(ctx)`）。
1.0 的库只有几部到几十部剧，这个成本看不出来。

**在 L1 规模下**：608,372 部 × 46.8 集 × 2 文件 ≈ **5,700 万个节点**要在**一次请求**里
构建出来。这不是慢，是不可能。

**契约**：

```go
// List 只返回某一个目录的直接子项，不返回孙子及以下。
// dir 是相对 vfs 根的路径（"" = vfs 根）；不存在的目录 → ErrNotFound。
// 成本：根目录 1 次查询；类型目录 1 次 DISTINCT genre；剧目录 1 次
//      WHERE genre/shard 分页查询；Season 目录 1 次 ListEmbyItems。
//      与全库规模无关。
List(ctx context.Context, dir string) ([]Node, error)
```

- `internal/webdav/fs.go` 的 `Tree` 接口跟着改；`Stat` / `OpenFile` 只取 `dir` 的
  直接子项，不再拿全树。请求内快照（`snapshot`）保留，键从「整棵树」改成「已列过的目录」。
- **每部剧的 `season` 目录必须能只靠自己的 `drama_id` 列举**，不许再 `ListDramas` 全表扫
  （现有 `dramaByDir` 是 `ListDramas` 后线性匹配，60 万行 × 每次请求 = 不可接受）。
  → 剧目录名到 `drama_id` 的映射要能走索引：`WHERE title = ? AND year = ?`（`idx_drama_title`）
  或 `WHERE provider=? AND provider_id=?`（后缀形态时）。
- 单剧内的 `ListEmbyItems(dramaID)` 本来就是常数成本（≤ 252 集），不改。

## D2（待 PL 拍板）：一级目录的子项数

这是 stage 1 唯一没有默认答案的事，因为它是**产品取舍**，不是实现细节。

**实测**：`全部/` 的直接子项 = 608,372；最大的类型目录（`爱情`，占 `/category` 800 抽样
的 35.5%）≈ **21.6 万**。
`golang.org/x/net/webdav@v0.62.0/webdav/file.go:787` 的 `walkFS` 对一次 `PROPFIND`
无条件 `f.Readdir(0)`——把**全部**子项读进内存，再对每个子项 `Stat` 一次。
60 万子项时：内存约 60 MB 的 `FileInfo`，外加 60 万次 `Stat`，响应体约 180 MB。
`PROPFIND Depth:1` 因此不可用，Emby 扫描在此处停住。

三个选项：

| 选项 | 树形 | 代价 | 最坏目录子项数 |
|---|---|---|---|
| **D2-a（推荐）** | `/全部/<分片>/<剧>/…`、`/<类型>/<分片>/<剧>/…`，分片 `00`–`3f` 共 64 个 | 多一级目录，Emby/rclone 的 `--max-depth` +1；分片名是机器生成的两字符，不好看 | 全部 ≈ 9.5 k；最大类型 ≈ 3.4 k |
| D2-b | 形状不变，WebDAV 只暴露「显式上架」的剧（新增 subscribe/上架概念 + 一张表 + 一个管理面开关） | 新概念 + 新状态 + 新接口，且用户要手工上架 60 万部里想看的那几部 | 取决于上架量 |
| D2-c | 形状不变，全量暴露 | stage 4 的产物 Emby 扫不动，等于没做 | 608 k |

**推荐 D2-a**，理由（按 ponytail 的梯子）：

- 没有任何新状态：分片是纯函数 `shard = fnv32a(provider_id) % 64`，不落库、不配置、
  不依赖时间。剧改名了它也不变（用 `provider_id` 而不是 `title`）。
- 不引入新概念：不需要「上架」这种用户要理解的东西，只是把同一批目录多切一层。
- D2-b 是唯一能减数据量的方案，但它把「全部」变成「部分」，与 L1 口径直接冲突，
  而且新增的状态（谁上架了什么）要进迁移、要进备份、要进管理面。
- D2-c 不是方案，是承认失败。

**若采纳 D2-a，0002 需要加一列**（否则 `WHERE shard=?` 只能全表扫）：

```sql
-- 目录分片键：fnv32a(provider_id) % 64，写库时由框架算好存下。
-- 只服务 /全部/ 与 /<类型>/ 的分页列举，不参与任何业务语义。
ALTER TABLE drama ADD COLUMN shard INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_drama_shard_title     ON drama (shard, title);
CREATE INDEX IF NOT EXISTS idx_drama_genre_shard_title ON drama (genre, shard, title);
```

（此时 0002 里原来的 `idx_drama_genre_title` 与 `idx_drama_title` 被上面两条取代，
不要两套并存。）

**不阻塞**：D2 只影响 stage 4（S6）。S2（插件化）、S3（调度）、S4（全量同步）、
S5（数据字段）都不依赖它——它们只用 `drama.genre`，不用目录形状。
因此 D2 的截止时间是 **S6 派单之前**。

## 现有实现的改动清单（给 S6 的输入）

| 位置 | 改动 |
|---|---|
| `internal/vfs/tree.go` | `root` 下新增 `全部` 与类型目录两级；`List` 改成 `List(ctx, dir)`；`parsePath` 从 3/4 段改成 4/5 段（或 5/6 段，取决于 D2）；`dramaByDir` 改成按 `(title, year)` / `(provider, provider_id)` 走索引，不再 `ListDramas` |
| `internal/vfs/tree.go:embyBase` / `embyLabel` / `render*.go` | **不动** |
| `internal/webdav/fs.go` | `Tree` 接口改签名；`nodes()` 改成按路径取子项；`snapshot` 的键从「整棵树」改成「目录」 |
| `internal/store/crud.go` | 新增 `ListGenres(ctx)`（`SELECT DISTINCT genre`）、`ListDramasByGenre(ctx, genre, page)`、`ListDramasAll(ctx, page)`；剧名→id 的索引查询 |
| `docs/ACCEPTANCE.md` | P2 的 `--max-depth` 3→4（D2-a 则 5），并写明 Emby 库路径不得指向 DAV 根 |

## 2.0 明确不做

- 按年份/按源的目录层（第二层目录只由类型决定）。
- 软链接、跨类型共享目录（WebDAV 只读且虚拟，符号链接在协议层没有对应物）。
- 剧目录名里带上集数、简介等元数据（目录名只由 `title` + `year` + 冲突后缀决定）。
- 目录路径兼容层（老路径与新路径同时可用）。

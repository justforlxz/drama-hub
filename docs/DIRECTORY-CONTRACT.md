# 目录契约

状态：定稿 v2（2026-10-30 / MKACG-604，按 lxz 2026-10-30 评论重写）
v2 变更：**删除 `全部/` 一级目录**，顶层直接是分类目录；挂载点 `/dav/` → `/webdav/`。
含 1 个待 PL 拍板项（**D2**）。
前置：`docs/DATA-MODEL.md`（`drama.genre` 是分类键）、`docs/PLAYBACK-CONTRACT.md`（`.strm` 的内容）

## 要交付的形状

```
/webdav/                     ← 只读虚拟 WebDAV
├── 其他/                    ← genre='' 的兜底目录（没有这种剧时不出现）
│   └── <剧名> (<年份>)/
│       ├── tvshow.nfo
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
│   └── 嫁进沈家第一天，我把规矩掀了 (2026)/
│       ├── tvshow.nfo
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
- **不产生空目录**：没有空分类的剧时 `其他` 不存在（与现有实现一致：`List` 只为有剧的
  目录出节点）。
- 上游恰好也有一个分类叫「其他」时，两者合并进同一个目录：分类名不做转义，
  区分「上游的其他」与「兜底的其他」没有用户价值。

## 同名剧冲突规则

冲突的定义：**同一个父目录下出现两个完全相同的子目录名**。

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
| Emby 库怎么配？ | **一个「剧集」库，媒体文件夹添加全部 `/<类型>` 目录**（19 个左右）。Emby 支持一个库挂多个文件夹，每个文件夹各自扫剧；`/webdav` 本身**不要**作为库根——那样 `爱情` 目录会被当成一部剧。 |
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
// 成本：根目录 1 次 DISTINCT genre；分类目录 1 次 WHERE genre=? 分页查询；
//      Season 目录 1 次 ListEmbyItems。与全库规模无关。
List(ctx context.Context, dir string) ([]Node, error)
```

- `internal/webdav/fs.go` 的 `Tree` 接口跟着改；`Stat` / `OpenFile` 只取直接子项。
  请求内快照（`snapshot`）保留，键从「整棵树」改成「已列过的目录」。
- 剧目录名 → `drama_id` 的映射必须走索引：`WHERE title = ? AND year = ?`（`idx_drama_title`）
  或后缀形态下 `WHERE provider=? AND provider_id=?`。**不许再 `ListDramas` 全表扫**
  （现有 `dramaByDir` 是线性匹配）。

## D2（待 PL 拍板）：分类目录的子项数

删掉 `全部/` 之后，问题没有消失，只是挪到了分类目录上：

**实测**：`/category` 全 800 部里「爱情」占 35.5%。按 608,372 部推算，最大的分类目录
**≈ 21.6 万**个剧目录；19 个分类里平均也有 3 万。
`golang.org/x/net/webdav@v0.62.0/webdav/file.go:787` 的 `walkFS` 对一次 `PROPFIND`
无条件 `f.Readdir(0)`——把全部子项读进内存再逐个 `Stat`。21.6 万子项时约 22 MB
`FileInfo` + 21.6 万次 `Stat` + 约 65 MB 响应体：`PROPFIND Depth:1` 不可用，Emby 扫不动。

三个选项：

| 选项 | 树形 | 代价 | 最坏目录子项数 |
|---|---|---|---|
| **D2-a（推荐）** | `/<类型>/<分片>/<剧>/…`，分片 `00`–`3f` 共 64 个 | 多一级目录；Emby 媒体文件夹从 ~19 个变成 ~1216 个（一次性配置）；分片名是机器生成的两字符 | ≈ 3.4 k |
| D2-b | 形状不变，WebDAV 只暴露「显式上架」的剧 | 新概念 + 新表 + 管理面开关，用户要手工上架；与 L1「全部」口径冲突 | 取决于上架量 |
| D2-c | 形状不变，全量暴露 | stage 4 的产物 Emby 扫不动，等于没做 | 216 k |

**推荐 D2-a**：

- 零新状态：`shard = fnv32a(provider_id) % 64` 是纯函数，不落库、不配置、不依赖时间，
  剧改名也不换目录。
- 零新概念：不需要用户理解「上架」，只是把同一批目录多切一层。
- D2-c 不是方案，是承认失败；D2-b 与 L1 口径直接冲突。

**若采纳 D2-a，`0002` 要加一列**（否则 `WHERE shard=?` 只能全表扫）：

```sql
-- 目录分片键：fnv32a(provider_id) % 64，写库时由框架算好存下。
-- 只服务分类目录的分页列举，不参与任何业务语义。
ALTER TABLE drama ADD COLUMN shard INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_drama_genre_shard_title ON drama (genre, shard, title);
```

（此时 `0002` 里原来的 `idx_drama_genre_title` 与 `idx_drama_title` 被上面这条取代，
不要两套并存。）

**不阻塞**：D2 只影响 stage 4（S6）。S2（插件化）、S3（调度）、S4（全量同步）、
S5（数据字段与缓存）都不依赖它。截止时间：**S6 派单之前**。

## 现有实现的改动清单（给 S6 的输入）

| 位置 | 改动 |
|---|---|
| `internal/webdav/handler.go` | `MountPath = "/dav/"` → `"/webdav/"`（PROPFIND 的 href 由它生成，无第二处字面量）；测试与 `cmd` 的注册跟着改 |
| `internal/vfs/tree.go` | 顶层改成分类目录；`List` → `List(ctx, dir)`；`parsePath` 段数 3/4 → 4/5（D2-a 则 5/6）；`dramaByDir` 改走索引 |
| `internal/vfs/render.go` | `renderStrm` 改成 `{strm_prefix}/video/{id}.m3u8`（`docs/PLAYBACK-CONTRACT.md`） |
| `internal/vfs/tree.go:embyBase` / `embyLabel` / `renderItemNFO` / `renderShowNFO` | **不动** |
| `internal/webdav/fs.go` | `Tree` 接口改签名；`nodes()` 按路径取子项；`snapshot` 键改成目录 |
| `internal/store/crud.go` | 新增 `ListGenres(ctx)`（`SELECT DISTINCT genre`）、`ListDramasByGenre(ctx, genre, page)`、剧名→id 的索引查询 |
| `docs/ACCEPTANCE.md` | 路径 `/dav/` → `/webdav/`；`--max-depth` +1；Emby 库配法改成「一个库多个媒体文件夹」 |

## 2.0 明确不做

- 按年份/按源的目录层（分类之下直接是剧或分片）。
- `全部/` 汇总目录（本次按 lxz 口径删除；要「全部」用 Emby 的多文件夹挂载）。
- 软链接、跨分类共享目录（虚拟只读，WebDAV 没有符号链接语义）。
- 剧目录名带集数、简介等元数据。
- 目录路径兼容层（老路径与新路径同时可用）。

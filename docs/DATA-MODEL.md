# 数据模型

状态：定稿（2026-10-30，MKACG-604 / stage 1）
当前基线：`internal/store/migrations/0001_init.sql`（`main @ d0bb513`）
本文的 SQL 是**草案**，落地时机：0002 归 S5，0003 必须与它的代码改动同一个提交。

**实际落地编号**（2026-10-30，MKACG-608 的 PL 裁定）：迁移文件名必须从 0001 连续、
不许留空号，而「删 `play_url`」随 S8（MKACG-612）落地，因此两类约束同时成立的唯一
结果是编号整体前移 —— 仓库里现在是 `0002_add_genre_and_source_state.sql`
（本文「0002」块）与 `0003_segment_cache.sql`（本文「0004」块），删列由 S8 以
`0004_drop_play_url.sql` 落地。SQL 内容逐字不变，本章的「0004」指的是 artifact
重建这件事，不是文件名。因此 S3 收口时是 `applied=3` / schema 版本 3。

## 上游能给什么（先定数据来源，再定列）

红果 `GET /player/{id}` 的一个响应里（`seriesDetail`）能给到的：

| 上游字段 | 内容 | 实测 |
|---|---|---|
| `series_name` | 剧名 | 948/948 部有值 |
| `series_cover` | 封面 URL | 948/948 部非空；**无签名、不过期**（`p3-novel.byteimg.com/...~tplv-shrink:640:0.image`） |
| `series_intro` | 简介 | 948/948 部非空 |
| `tags` | 分类标签数组 | 948/948 部非空；抽样 800 部里每部 3–12 个（均值 6.99），去重 259 个 |
| `episode_cnt` | 声明总集数 | 948/948 部 = `len(vid_list)` |
| `vid_list` | 全部集标识数组 | 下标即集号 |
| `first_visible_time` | 上线时间（unix 秒） | 部分缺失 → `year` 尽力而为 |
| `accessible_episode_cnt` | **免付费可见集数** | 88/88 部 = 3（全集需付费，`protectedPlayback=true`） |

**给不到的**（因此不能进列，或必须恒为空）：每集标题、每集时长、每集直链（除第 1 集
的 10 秒预览）、任何付费解锁后的数据。取证见 `docs/L1-CORPUS-SCOPE.md`。
逐集去取的成本是 1 请求 × 2847 万集（≈ 720 天），已否决。

## 表结构变更

### 0002：剧目分类 + 数据源调度状态

```sql
-- 0002_add_genre_and_source_state.sql

-- 主类型：上游声明的第一个标签（红果 tags[0]，948 部样本 100% 有值）。
-- WebDAV 的 /<类型>/ 一级目录按本列分组；空串表示上游未声明，目录兜底为「其他」。
ALTER TABLE drama ADD COLUMN genre TEXT NOT NULL DEFAULT '';

-- 全部标签：上游 tags 的原样 JSON 数组（保序、去重）。只为「刷新下来的数据全部入库」
-- 的保真，不做检索；归类只认 genre 单值。
ALTER TABLE drama ADD COLUMN tags TEXT NOT NULL DEFAULT '[]';

-- 目录分片键：fnv32a(provider_id) % N（N 是代码常量，默认 128），写库时由框架算好存下。
-- 只服务分类目录的分页列举，不参与任何业务语义；改 N 时一条 UPDATE 重算，不改 schema。
-- 为什么必须有分片层：PROPFIND 没有分页，单个目录子项上万时 Emby 扫不动
-- （docs/DIRECTORY-CONTRACT.md「D2」）。
ALTER TABLE drama ADD COLUMN shard INTEGER NOT NULL DEFAULT 0;

-- 目录列举：WHERE genre = ? AND shard = ? ORDER BY title 走本索引
-- （同一索引也覆盖纯 genre / 纯 shard 的 DISTINCT 与 GROUP BY）。
CREATE INDEX IF NOT EXISTS idx_drama_genre_shard_title ON drama (genre, shard, title);

-- 剧目录名 → 行的点查：WHERE title = ? AND year = ?（Render 每个请求一次）。
CREATE INDEX IF NOT EXISTS idx_drama_title ON drama (title);

-- 数据源级调度状态：每源一行。
--
-- cursor 是插件枚举的不透明断点（全库枚举跨重启续跑的唯一依据）；
-- last_* 驱动状态页与「下次时间由配置推导、不依赖内存残留」。
-- 不存 next_due_at：它由 last_run_at + 当前 interval 现算，存下来就是第二份真值。
-- 与 settings 分表：settings 是运行配置（人写），本表是调度进展（框架写）。
CREATE TABLE IF NOT EXISTS source_state (
    name        TEXT PRIMARY KEY,
    cursor      TEXT NOT NULL DEFAULT '',
    last_run_at TEXT NOT NULL DEFAULT '',
    last_ok_at  TEXT NOT NULL DEFAULT '',
    last_error  TEXT NOT NULL DEFAULT ''
);
```

每列 / 每个索引的用途：

| 对象 | 用途 | 谁写 | 谁读 |
|---|---|---|---|
| `drama.genre` | `/<类型>/` 一级目录的分类键；一条剧只属一个类型 | 框架（`Work().Genres[0]`） | `internal/vfs` 目录列举 |
| `drama.tags` | 保真：上游声明的全部标签 | 框架（`Work().Genres`） | 只出现在管理面/AI 检索，不进目录 |
| `drama.shard` | 分片目录键：`fnv32a(provider_id) % N`，把每个分类目录摊薄到 ≤3000 子项 | 框架（写 `drama` 时算） | `internal/vfs` 分类目录列举 |
| `idx_drama_genre_shard_title` | `/<类型>/<分片>/` 的分页列举（`WHERE genre=? AND shard=? ORDER BY title`）；也覆盖 `SELECT DISTINCT genre` 与 `GROUP BY genre` | — | `internal/vfs`、`internal/web` |
| `idx_drama_title` | 剧目录名点查（`WHERE title=? AND year=?`），`Render` 每请求一次 | — | `internal/vfs` |
| `source_state.cursor` | 枚举断点，38 小时全量灌库续跑 | 调度器（每轮成功后写） | 调度器 |
| `source_state.last_run_at` | 算 `next_due` 的唯一真值来源（重启可算） | 调度器 | 调度器、状态页 |
| `source_state.last_ok_at` | 状态页「上次成功」 | 调度器 | 状态页 |
| `source_state.last_error` | 状态页「上次失败原因」；成功一轮后清空 | 调度器 | 状态页 |

**S4 对端确认（MKACG-606）**：调度器的全部读写点（启动铺空行、每轮读游标、写回
游标 + 三个时间 + 错误、状态页读五列）都只用上面五列，**够用，不需要新增或修改列**；
`next_due` 继续不落库（请求时由 `last_run_at` + 现读 `refresh_interval_sec` 算出）。

### 0003：直链不落库

```sql
-- 0003_drop_play_url.sql

-- 直链是请求时向插件解析的短时凭据，持久化等于把过期链接写进库；
-- 播放改为请求时解析（DESIGN：直链不落库）。
ALTER TABLE episode DROP COLUMN play_url;
ALTER TABLE episode DROP COLUMN play_url_at;
```

**0003 必须与它的代码改动同一个提交**，否则仓库处于编译不过的状态：

- `internal/store/crud.go`：`episodeColumns` / `scanEpisode` / `UpsertEpisode` 去掉两列；
  `store.Episode` 去掉 `PlayURL` / `PlayURLAt`。
- `internal/store/stats.go`：`PlayableEpisodes` 现在按 `play_url <> ''` 计数
  （`internal/store/stats.go:30`）。删列后该定义不存在，改成按「最近一次 `Resolve` 是否
  成功」无法从库里得知 —— 因此**直接删掉 `PlayableEpisodes` 字段与它在
  `internal/web/api_dramas.go` 的 `Playable` 输出**，管理面不再展示「可播集数」。
  理由：这个数字在当前实现里恒为 0（同步从不写 `play_url`），删列只是让它别再骗人。
- **`internal/segment` 与 `internal/stream` 直接读 `ep.PlayURL`**（`segment/server.go:59`、
  `segment/server.go:100`、`segment/cutter.go:94`、`stream/server.go:110`），删列必然编译不过。
  而它们读到的值在真实库上**恒为空**：同步从不写 `play_url`（`UpsertEpisode` 的调用方
  一律传空），`provider.FetchDirectURL` 只有测试在调、没有任何生产接线。
  因此 **1.0 的播放端点在真实库上恒返回 502「没有播放地址」**——播放链路是断的。
  这也正是 ADR-0007 要把直链解析搬进 `/video` 的原因：解析必须在请求时发生在服务内，
  而不是期望库里存着一个 URL。
  改法：四处改为调用 `plugin.Resolve(ctx, ref, epNo)`，去掉对 `ep.PlayURL` 的依赖。

### 0004：本地分片缓存的索引

```sql
-- 0004_segment_cache.sql

-- 本地分片缓存（ADR-0007 把视频分片缓存从 rclone 收回本服务）。
-- 复用 artifact 作为缓存索引：kind 增加 'segment'。
-- SQLite 不支持修改 CHECK 约束，所以重建表并搬数据（artifact 无外键引用，安全）。
CREATE TABLE artifact_new (
    kind         TEXT    NOT NULL CHECK (kind IN ('cover','index','segment')),
    key          TEXT    NOT NULL,
    path         TEXT    NOT NULL,
    bytes        INTEGER NOT NULL,
    sha256       TEXT    NOT NULL DEFAULT '',
    last_used_at TEXT    NOT NULL,
    PRIMARY KEY (kind, key)
);

INSERT INTO artifact_new (kind, key, path, bytes, sha256, last_used_at)
     SELECT kind, key, path, bytes, sha256, last_used_at FROM artifact;

DROP TABLE artifact;
ALTER TABLE artifact_new RENAME TO artifact;

-- 淘汰：按 kind 取 last_used_at 最旧的一批（LRU 与过期都走这条索引）。
CREATE INDEX idx_artifact_kind_last_used ON artifact (kind, last_used_at);
```

| 对象 | 用途 |
|---|---|
| `artifact(kind='segment')` | 分片缓存索引：`key = sha256(uid\x1fseq\x1fcodec_sig)`，`path` 是本地文件，`bytes` 供淘汰算总量，`last_used_at` 供 LRU 与过期 |
| `artifact(kind='cover')` | 海报（已有；本次把「刷新时下载」接上） |
| `artifact(kind='index')` | 关键帧索引（已有；改为按需建立） |
| `idx_artifact_kind_last_used` | `WHERE kind=? ORDER BY last_used_at` 的淘汰查询；也覆盖按 `kind` 的计数/求和 |

**缓存里也不存直链**：`key` 是内容哈希，`path` 是本地路径，两者都不含上游 URL。
换成 `stream_secret` 或上游换链都不会在缓存索引里留下痕迹。

## 字段来源逐个定稿

### 类型 / 分类

- **来源**：`Work().Genres`，即上游 `seriesDetail.tags`，保序。
- **两个落点**：`drama.genre` = `Genres[0]`（主类型，目录用，单值）；
  `drama.tags` = 全部标签的 JSON 数组（保真用）。
- **为什么不用「多对多两张表」**：目录只需要一个单值类型；多值表要
  `drama_genre` 关联表（≈ 200 万行）+ 两条查询路径，换来的能力 2.0 用不到
  （没有「按标签筛」的对外接口）。JSON 列足以保真。
- **空值**：`Genres` 为空 → `genre = ''`，目录兜底为 `其他`（`docs/DIRECTORY-CONTRACT.md`）。
  实测 948/948 部有标签，但兜底必须有：兜底不是给红果的，是给第二个插件的。
- **改名/改分类**：刷新时 `genre` 随 `tags[0]` 覆盖。这会让剧在 WebDAV 里换目录
  ——Emby 会当成「旧剧消失 + 新剧出现」。这是上游行为的忠实反映，不做稳定性妥协。

### 海报

- **不新增列、不新增表**：复用现有 `artifact`（`kind='cover'`）。
- **key 约定**：`<provider>/<provider_id>`（确定性派生，无需在 `drama` 上加外键列）。
- **为什么复用 `artifact` 而不是加 `drama.cover_path`**：`internal/cache` 已经在管理
  `artifact`（`kind IN ('cover','index')`），封面下载/落盘/读回已有实现。
  淘汰口径（ADR-0007 把缓存收回本服务后）：海报**不参与** `cache_max_mb` 的体积淘汰
  （只有分片参与），但**参与** `cache_lru_days` 的过期淘汰——命中即 touch，所以天天
  在用的海报不会被当成过期；「源图失效不影响已入库海报」仍然成立（只要它还在用）。
- `drama.cover_url` 保留为**上游原值**（溯源用），不做本地路径回填：两列表达两件事。
- 上游封面地址无签名、不过期（实测），所以「源图失效」目前不是红果的问题；
  本地资产仍要做，理由是第二个源可能会给短时签名链。

### 剧集信息

- `episode.ep_no` = `vid_list` 下标 + 1；`episode.vid` = `vid_list[i]`。
- `episode.title` **恒为空**、`episode.duration_sec` **入库恒为 0**：
  源侧批量拿不到（每集要 1 请求，2847 万次已否决）。
  → 契约上这是**允许留零值**（`docs/PLUGIN-CONTRACT.md` `Episode` 注释），
  实现上不许为此逐集抓。
  → 首次播放该集（建关键帧索引）时用 `ffprobe` 实测时长回填 `duration_sec`
  （`store.BackfillEpisodeDuration`：只填 0、不覆盖非零值），并同事务重建该剧
  `emby_item`，让 `.nfo` 的 `<runtime>` 当次就变成真实值；没播过的集仍是 0
  （见 `docs/DEV-STANDARDS.md`「关键帧索引」）。
  → 刷新（`UpsertEpisode`）携带上游的 0 时不清零已回填的值，上游真给了非零值才覆盖。
  → NFO 的 `<title>` 因此回落到 `第 N 集`（`internal/vfs/tree.go:embyLabel` 已这么写）。
- `episode.source` = 插件名（`"web"`）。
- `drama.episode_total` = `episode_cnt`；`drama.sync_at` = 本次刷新时间（UTC RFC3339）。

### 合并数据

- `merge_group` / `merge_item` **完全不新增、不修改**。
- **刷新不冲掉编排**：这是结构保证——刷新只做 `UpsertDrama` / `UpsertEpisode` /
  `RebuildEmbyItems`，而 `RebuildEmbyItems` 是**读** `merge_group` + `merge_item`
  再重算派生表（`internal/store/merge.go:232` 起），不写这两张表。
- 需要 Review 的一条：任何新的刷新代码 **MUST NOT** 写 `merge_*`，也 MUST NOT
  `DELETE FROM emby_item`（那是 `RebuildEmbyItems` 的职责，它整体替换但保留合并语义）。

### 播放入口（直链不落库）

- 库中**没有任何一列**保存直链（0003 删两列后），缓存索引里也没有（见 0004）。
- 请求路径：`/video/{id}.m3u8` → 读 `segment` 表出 playlist；`/video/{id}/{seq}.ts`
  → 查本地分片缓存 → 未命中则 `plugin.Resolve` 取直链、ffmpeg 切片、落缓存、回字节。
  详见 `docs/PLAYBACK-CONTRACT.md` 与 `docs/CACHE-MODEL.md`。
- 失败语义：`ErrNoStream` / `ErrNotFound` → **502，不写库、不写缓存**。
- 红果实况：只有第 1 集有直链且是 10 秒预览，第 2..N 集 `ErrNoStream`，
  第 4 集起 `ErrNotFound`。这是源侧事实，不是本服务缺陷。

## provider 标识

`Plugin.Name()` 直接进 `drama.provider` / `episode.source`。红果插件返回 **`"web"`**。

- **理由**：库中已有 `provider='web'` 的数据；改名要写数据迁移（`UPDATE drama SET
  provider='hongguo' WHERE provider='web'` + 同等的 `episode.source`），而 2.0 不需要
  「同一剧目出现在两个源」。零迁移、零风险。
- **被否决的备选**：`Name()="hongguo"` + 0002 里带一条 `UPDATE`。
  否决理由：迁移一旦写错就污染身份列（`UNIQUE(provider, provider_id)`），
  而收益只是名字好看。
- **代价**：`"web"` 这个名字不表达「红果」。缓解：状态页与文档一律显示
  `web（红果官网 SSR）`，接口层用 `WorkRef.Plugin` 而不是硬编码字符串。

## 增删改语义

| 情形 | 处理 | 理由 |
|---|---|---|
| 源里新增剧目 | 插入 | L1 的正向情形 |
| 源里新增集（连载中） | 追加 `ep_no` 更大的行，`episode_total` 更新 | 现有 `UpsertEpisode` 已支持 |
| 源里剧目标题/简介/封面/标签变化 | 覆盖 | 上游是真值 |
| 源里集数变少 | **不删任何 `episode` 行**，只更新 `episode_total` | 删除会连带删 `merge_item`（`ON DELETE CASCADE`），静默破坏人工编排；少集的剧在 Emby 里多出几集，比丢掉编排轻 |
| 源里剧目消失（下架） | **不删 `drama` 行**，只在 `source_state.last_error` / 日志里留痕 | 同上；且 sitemap 会滞后（实测最新 53 部缺失），把「枚举不到」当「下架」会误删正在上架的剧 |
| 用户显式删剧 | `DELETE FROM drama`（现有 `DeleteDrama`，级联删集与编排） | 唯一删除入口 |

一句话：**自动流程只增改，删除只能由人发起。**

## 0002 的 dry-run 验证（历史记录，SQLite 时代）

> 本节是换 PostgreSQL 之前（SQLite 后端）的一次 dry-run 记录：SQL 块、命令与输出都按
> 当时的口径，保留下来只为说明「本文曾是 SQL 的唯一真值、文档与代码一起改」这条工作方式。
> **现行迁移文件是 `internal/store/migrations/0001…0004` 的 PG DDL**（ADR-0009），
> 与本节的 SQLite 版本不要互相校对：`0003` 现在是就地 drop/add CHECK，`0004` 的列名
> 与顺序也不变但表类型是 PostgreSQL。

验证方式：把本文上面三个 `sql` 代码块**原样抽出来**（不是手抄），放进仓库副本的
`internal/store/migrations/`，用**仓库自己的迁移 runner**（`store.Open` → `migrate`）跑：

```sh
# 从本文抽出三个 SQL 块 → 0002 / 0003 / 0004（顺序即它们在文中出现的顺序）
python3 -c 'import re,io,pathlib;s=io.open("docs/DATA-MODEL.md",encoding="utf-8").read();n=["0002_add_genre_and_source_state.sql","0003_drop_play_url.sql","0004_segment_cache.sql"];[pathlib.Path("/tmp/d",a).write_text(b,encoding="utf-8") for a,b in zip(n,re.findall(r"```sql\n(.*?)```",s,re.S))]'
cp /tmp/d/0002_*.sql /tmp/d/0003_*.sql /tmp/d/0004_*.sql <repo-copy>/internal/store/migrations/
cd <repo-copy> && go test ./internal/store/ -run TestS1 -v
```

这样本文就是 SQL 的唯一真值：文档改了而验证没重跑，抽取出来的文件就会跑失败
（本次改稿时它确实失败过一次——旧 schema 里的 `next_due_at` 已删）。

结果：

```
首次 applied=4                       # 空库 → 最新（0001+0002+0003+0004）
二次 applied=0                       # 重复执行幂等
schema 版本=4                        # 两次打开后都停在 4
play_url 已移除: no such column: play_url
索引 idx_drama_genre_shard_title / idx_drama_title / idx_artifact_kind_last_used 均存在
默认值 genre="" tags="[]" shard=0
artifact 允许 kind='segment'；CHECK 拒绝未知 kind
重建前插入的 cover/index 两行在重建后仍在（逐字段相同，总行数 2）
迁移 0001 0001_init.sql    (3806 bytes)
迁移 0002 ...              (2099 bytes)
迁移 0003 ...              (275 bytes)
迁移 0004 ...              (996 bytes)
```

三条要写清楚的幂等边界：

1. **幂等的保证在 runner，不在 SQL。** `migrate` 用 `schema_migrations` 的最大 version 比对版本，
   第二次调用一个文件都不执行（`applied=0`）。因此「重复执行幂等」= 重复 `Open` 幂等。
2. **裸重复执行 SQL 不幂等**，且这是 SQL DDL 的语义决定的（PostgreSQL 同）：`ADD COLUMN` /
   `DROP COLUMN` 重复执行必然报错。不许为此写成「先查 `pragma_table_info` 再决定」
   的动态 SQL —— 那会让迁移文件变成不可 review 的脚本。
3. **迁移文件名必须从 0001 连续递增**（`migrate.go` 强制），所以 0002–0004 之间
   不能留空号；如果 S5 决定把删列提前，编号整体前移，内容不变。

## 对现有代码的影响清单（给 S5 的输入，不是本文的实现）

| 文件 | 影响 |
|---|---|
| `internal/store/crud.go` | `Drama` 加 `Genre`/`Tags`/`Shard`；`Episode` 删 `PlayURL`/`PlayURLAt`；列清单与 scan 同步；`UpsertDrama` 写入 `shard` |
| `internal/store/migrations/0002_*.sql`、`0003_*.sql` | 新增（内容见上） |
| `internal/store/state.go`（新） | `source_state` 的读写：`LoadState` / `SaveState` |
| `internal/store/stats.go` | 删 `PlayableEpisodes` |
| `internal/web/api_dramas.go` | 删 `dramaView.Playable`、`episodeView.HasPlayURL` |
| `internal/provider/provider.go` | `SyncSeries` 的写库部分移到 `internal/sync`，本包退化为 `internal/plugin/hongguo` |
| `docs/DEV-STANDARDS.md` | 「配置」表加 5 个新 key；「数据源」章改指向插件契约 |

## 2.0 明确不做

- 类型/标签的多对多表、按标签检索接口。
- 海报之外的图片资产（剧照、演员头像）：上游 `celebrities` 有数据但不进 2.0。
- 每集标题/时长的补齐（源侧拿不到，逐集抓已否决）。
- 跨源同剧去重。
- `drama` / `episode` 的软删除列：不删就不需要 `deleted_at`。

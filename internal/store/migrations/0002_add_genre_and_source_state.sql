-- 0002_add_genre_and_source_state.sql（PostgreSQL 版）

-- 主类型：上游声明的第一个标签（红果 tags[0]，948 部样本 100% 有值）。
-- WebDAV 的 /<类型>/ 一级目录按本列分组；空串表示上游未声明，目录兜底为「其他」。
ALTER TABLE drama ADD COLUMN genre TEXT COLLATE "C" NOT NULL DEFAULT '';

-- 全部标签：上游 tags 的原样 JSON 数组（保序、去重）。只为「刷新下来的数据全部入库」
-- 的保真，不做检索；归类只认 genre 单值。
ALTER TABLE drama ADD COLUMN tags TEXT NOT NULL DEFAULT '[]';

-- 目录分片键：fnv32a(provider_id) % N（N 是代码常量，默认 128），写库时由框架算好存下。
-- 只服务分类目录的分页列举，不参与任何业务语义；改 N 时一条 UPDATE 重算，不改 schema。
-- 为什么必须有分片层：PROPFIND 没有分页，单个目录子项上万时 Emby 扫不动
-- （docs/DIRECTORY-CONTRACT.md「D2」）。
ALTER TABLE drama ADD COLUMN shard INTEGER NOT NULL DEFAULT 0;

-- 目录列举：WHERE genre = … AND shard = … ORDER BY title 走本索引
-- （同一索引也覆盖纯 genre / 纯 shard 的 DISTINCT 与 GROUP BY）。
-- title / genre 都是 COLLATE "C" 列，索引与查询的排序规则一致。
CREATE INDEX IF NOT EXISTS idx_drama_genre_shard_title ON drama (genre, shard, title);

-- 剧目录名 → 行的点查：WHERE title = … AND year = …（Render 每个请求一次）。
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

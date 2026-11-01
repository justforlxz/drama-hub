-- 0003_segment_cache.sql（PostgreSQL 版）

-- 本地分片缓存（ADR-0007 把视频分片缓存从 rclone 收回本服务）。
-- 复用 artifact 作为缓存索引：kind 增加 'segment'。
--
-- PostgreSQL 可以就地改 CHECK（SQLite 只能重建表），所以这里是 drop + add：
-- 约束名在 0001 里显式写成 artifact_kind_check，不靠数据库自动命名。
ALTER TABLE artifact DROP CONSTRAINT artifact_kind_check;
ALTER TABLE artifact ADD CONSTRAINT artifact_kind_check
    CHECK (kind IN ('cover','index','segment'));

-- 淘汰：按 kind 取 last_used_at 最旧的一批（LRU 与过期都走这条索引）。
CREATE INDEX IF NOT EXISTS idx_artifact_kind_last_used ON artifact (kind, last_used_at);

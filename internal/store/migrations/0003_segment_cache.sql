-- 0003_segment_cache.sql

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

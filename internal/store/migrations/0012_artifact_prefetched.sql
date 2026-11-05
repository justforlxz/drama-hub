-- 0012_artifact_prefetched.sql（PostgreSQL 版）：分片预热标记（MKACG-682）。
--
-- 预热出来的分片「还没被任何一次播放请求用过」，和「用户真正看过」的缓存不该
-- 同等对待：容量吃紧时前者先淘汰（docs/CACHE-MODEL.md「淘汰」）。
--
-- 用一列布尔而不是新增一张表：标记天然属于 artifact 这一行，单独建表要再做一次
-- 关联查询；历史行全部是「前台生成」的，默认 FALSE 即真实语义，升级不需要回填。
ALTER TABLE artifact ADD COLUMN IF NOT EXISTS prefetched BOOLEAN NOT NULL DEFAULT FALSE;

-- 0011_episode_stream_fail.sql（PostgreSQL 版）

-- 预取失败的退避记录（docs/adr/0017-prefetch-and-stream-health.md）。与 episode_stream
-- 分表：失败时还没有可信通道（App 通道的换身份限流发生在解析过程中，没拿到直链就判不出
-- web/app），而 episode_stream 的主键是 (episode_id, channel) 且 url NOT NULL —— 为了记
-- 一次失败去放宽 CHECK、塞空 url，会把「已入库直链」的语义搅浑。分表还让「作废直链」
-- （DELETE episode_stream）不会连带删掉失败记录。
CREATE TABLE IF NOT EXISTS episode_stream_fail (
    episode_id  BIGINT PRIMARY KEY REFERENCES episode (id) ON DELETE CASCADE,
    -- rejected|upstream|schema|no_stream|login|unsupported|other（stream.FailureReason）
    reason      TEXT NOT NULL,
    failed_at   TEXT NOT NULL,   -- UTC RFC3339（与 0001 的业务时间列同一约定）
    retry_after TEXT NOT NULL    -- UTC RFC3339：到这个时刻之后才允许再预取
);

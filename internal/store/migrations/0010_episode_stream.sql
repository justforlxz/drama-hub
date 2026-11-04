-- 0010_episode_stream.sql（PostgreSQL 版）

-- 直链入库（docs/adr/0016-episode-stream-cache.md）。0004 删掉 episode.play_url 防的
-- 是「1.0 把很久以前存的 URL 当作永久有效」；这里存的是带 expires_at 的短时凭据，
-- 过期即刷新，是反过来利用「可判定的过期」避免每次播放都重新申请（MKACG-680）。
--
-- 一集一通道一行：channel 与 segment.channel 同一判据（带不带内容密钥），url 与 key
-- 同生命周期（一起失效、一起重取）。key 为 NULL 表示明文源。
CREATE TABLE IF NOT EXISTS episode_stream (
    episode_id BIGINT NOT NULL REFERENCES episode (id) ON DELETE CASCADE,
    channel    TEXT   NOT NULL CHECK (channel IN ('web','app')),
    url        TEXT   NOT NULL,
    key        BYTEA,
    -- 业务时间列与 0001 一致：UTC RFC3339 的不透明 TEXT（不用 TIMESTAMPTZ）。
    expires_at TEXT   NOT NULL,           -- 到这个时刻之后不再复用
    fetched_at TEXT   NOT NULL,           -- 解析落库时刻
    last_ok_at TEXT   NOT NULL DEFAULT '' -- 上次确认能用；空串 = 还没有确认过
);

ALTER TABLE episode_stream ADD PRIMARY KEY (episode_id, channel);

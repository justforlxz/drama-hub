-- F3 初始表结构（DESIGN v4 §5.4）。
--
-- 迁移只向前：本文件一经提交不再修改，后续变更另写 0002_…。
-- settings 表块必须与 internal/store/settings.go 的 settingsDDL 逐字一致
-- （F2 约定：两处幂等共存，改一处必须同步改另一处）。

CREATE TABLE IF NOT EXISTS drama (
    id            INTEGER PRIMARY KEY,
    provider      TEXT    NOT NULL,              -- 'web' | 'app'
    provider_id   TEXT    NOT NULL,              -- 上游剧目 id（sid）
    title         TEXT    NOT NULL,
    year          INTEGER NOT NULL DEFAULT 0,
    plot          TEXT    NOT NULL DEFAULT '',
    cover_url     TEXT    NOT NULL DEFAULT '',
    episode_total INTEGER NOT NULL DEFAULT 0,
    sync_at       TEXT    NOT NULL DEFAULT '',
    UNIQUE (provider, provider_id)
);

CREATE TABLE IF NOT EXISTS episode (
    id           INTEGER PRIMARY KEY,
    drama_id     INTEGER NOT NULL REFERENCES drama (id) ON DELETE CASCADE,
    ep_no        INTEGER NOT NULL,
    title        TEXT    NOT NULL DEFAULT '',
    duration_sec INTEGER NOT NULL DEFAULT 0,
    source       TEXT    NOT NULL,               -- 'web' | 'app'
    vid          TEXT    NOT NULL,
    play_url     TEXT    NOT NULL DEFAULT '',
    play_url_at  TEXT    NOT NULL DEFAULT '',
    UNIQUE (drama_id, ep_no)
);

-- segment 只存「每集的分片索引」：episode_id + start_sec 唯一，按 start_sec 排序即片内序。
-- 不存 uid/seq：那是合并播放时的推导视图（merge_item.ord + 片内序），不落库，
-- 否则 RebuildEmbyItems 的幂等重建会连带冲掉索引。
CREATE TABLE IF NOT EXISTS segment (
    id         INTEGER PRIMARY KEY,
    episode_id INTEGER NOT NULL REFERENCES episode (id) ON DELETE CASCADE,
    start_sec  REAL    NOT NULL,
    dur        REAL    NOT NULL,
    keyframe   INTEGER NOT NULL DEFAULT 1,       -- 1=切点为关键帧（可 -c copy）0=需转码
    codec_sig  TEXT    NOT NULL DEFAULT '',
    mode       TEXT    NOT NULL DEFAULT 'copy' CHECK (mode IN ('copy','transcode')),
    UNIQUE (episode_id, start_sec)
);

CREATE TABLE IF NOT EXISTS merge_group (
    id       INTEGER PRIMARY KEY,
    drama_id INTEGER NOT NULL REFERENCES drama (id) ON DELETE CASCADE,
    label    TEXT    NOT NULL,
    UNIQUE (drama_id, label)
);

CREATE TABLE IF NOT EXISTS merge_item (
    group_id   INTEGER NOT NULL REFERENCES merge_group (id) ON DELETE CASCADE,
    episode_id INTEGER NOT NULL REFERENCES episode (id) ON DELETE CASCADE,
    ord        INTEGER NOT NULL,
    PRIMARY KEY (group_id, ord),
    UNIQUE (group_id, episode_id)
);

-- emby_item 是派生表（M1/M7 通过 RebuildEmbyItems 幂等重建），uid 生成规则属 M5/M7；
-- 本阶段只约束「非空 + 唯一」。
CREATE TABLE IF NOT EXISTS emby_item (
    id          INTEGER PRIMARY KEY,
    drama_id    INTEGER NOT NULL REFERENCES drama (id) ON DELETE CASCADE,
    season      INTEGER NOT NULL DEFAULT 1,
    ep_no       INTEGER NOT NULL,
    kind        TEXT    NOT NULL CHECK (kind IN ('single','merged')),
    uid         TEXT    NOT NULL UNIQUE,         -- /s/{token}/e/{uid}
    runtime_sec INTEGER NOT NULL DEFAULT 0,
    UNIQUE (drama_id, season, ep_no)
);

-- artifact 只承载封面与关键帧索引；视频分片缓存归 rclone（ADR-0004）。
CREATE TABLE IF NOT EXISTS artifact (
    kind         TEXT    NOT NULL CHECK (kind IN ('cover','index')),
    key          TEXT    NOT NULL,
    path         TEXT    NOT NULL,
    bytes        INTEGER NOT NULL,
    sha256       TEXT    NOT NULL DEFAULT '',
    last_used_at TEXT    NOT NULL,
    PRIMARY KEY (kind, key)
);

-- 与 settingsDDL 逐字一致（见文件头）。
CREATE TABLE IF NOT EXISTS settings (
	key        TEXT PRIMARY KEY,
	value_json TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

-- 0004_drop_play_url.sql

-- 直链是请求时向插件解析的短时凭据，持久化等于把过期链接写进库；
-- 播放改为请求时解析（docs/DATA-MODEL.md「0003：直链不落库」，
-- 编号按 PL 2026-10-30 裁定落在 0004）。
ALTER TABLE episode DROP COLUMN play_url;
ALTER TABLE episode DROP COLUMN play_url_at;

-- 0009_add_jellyfin_category_switch.sql（PostgreSQL 版）：Jellyfin 媒体库的分类开关（MKACG-668）。
--
-- 为什么单独一张表而不是塞进 settings：开关是按分类的多行状态，settings 是单值键表。
-- 「行不存在 = 启用」是默认值本身，所以升级不需要回填，新增分类也不需要预建行 ——
-- 只有被关掉的分类才占一行。
--
-- category 存分类目录名（与 vfs 根目录同名，也是自管库名去掉 jellyfin_library_prefix
-- 的后缀）。更新用 upsert，重新开启就删行，读写都是幂等的。
CREATE TABLE IF NOT EXISTS jellyfin_category_switch (
    category   TEXT PRIMARY KEY,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

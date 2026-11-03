-- 0008_drop_emby_item.sql（PostgreSQL 版）

-- 对外剧集不再落库（docs/adr/0014-runtime-derived-episodes.md）：
-- 「哪几集并成一条」只由 `episode`（原始集）+ 当前 `settings.merge_chunk_size`
-- 在每次读到时现算，派生结果没有任何需要持久化的状态。
--
-- 留着这张表只有坏处：它是同一件事的第二份真相，改 N 之后要么过期、要么得有人
-- 记得重建 —— 这正是 MKACG-666 要消除的「预生成」体感。
--
-- 迁移不迁数据：表里每一行都能由 episode + 当前 N 重新算出来（整体替换语义本来
-- 就丢弃旧值），所以直接丢表即等价。uid 也没变（单集 `e<episode.id>`、合并集
-- `m<dramaID>_<epFrom>_<epTo>`），已发出去的播放 URL 在同一 N 下仍然有效。
--
-- 0001 仍在空库上建这张表、0005 仍要读它清旧行：迁移是不可回放的顺序历史，
-- 只有最新一步的结果有意义，回头改历史会让已升到中间版本的库失去可重放性。
DROP TABLE IF EXISTS emby_item;

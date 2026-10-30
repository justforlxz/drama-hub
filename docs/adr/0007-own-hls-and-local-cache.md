# ADR-0007：播放统一走自造 HLS，分片缓存收回本服务

状态：提议，待 PL 确认（2026-10-30，MKACG-604）
上游口子：lxz 2026-10-30 评论（原文：「提供 /video 来播放一个uuid的m3u8，strm里填写的就是自己的
/video/xxxxxx.m3u8 这种结构……需要一个缓存模块，用来本地缓存海报照片和m3u8，因为m3u8里是包含
多个视频文件的，本服务负责在播放m3u8时提供数据转换和缓冲功能」）

## 背景

1.0 的播放链路是两套：

- **单集**：`.strm` → `/s/{token}/e/{uid}` → 上游 CDN 的 MP4 字节透传（零拷贝、零缓存）。
- **合并集**：`.strm` → `/s/{token}/e/{uid}/master.m3u8` → 自造 playlist → 分片端点。

缓存由 rclone（`serve http --vfs-cache-mode full`）负责（ADR-0004），因为它当时在
「Emby → rclone 代理 → 本服务」这条链路的中间。

lxz 的要求把链路改成：`.strm` → **本服务的** `/video/{id}.m3u8` → 本服务的分片端点。
rclone 从播放链路里退出，只留 WebDAV 只读挂载。

## 决定

1. **对外只有一个播放家族** `/video/{id}.m3u8` + `/video/{id}/{seq}.ts`。
   单集与合并集都走 HLS，不再有「单集 MP4 透传」这条路径。
   `/dav/` 改挂 `/webdav/`；`/s/{token}/…` 退役，不留兼容层。
2. **`{id}` 是无状态签名 id**：`uid + "-" + hex(HMAC(stream_secret, uid)[:12])`，
   不落库、不过期、稳定（`uid` 只依赖 `episode.id` / `merge_group.id`）。
   → `docs/PLAYBACK-CONTRACT.md`「`id` 怎么来」。
3. **分片缓存归本服务**，键为 `sha256(uid|seq|codec_sig)`，落 `<cache_dir>/seg/`，
   索引复用 `artifact` 表（扩 `kind='segment'`），按 `cache_max_mb` + `cache_lru_days` 淘汰。
   → `docs/CACHE-MODEL.md`。
4. **m3u8 文本不缓存**：它由 `segment` 表纯函数生成；缓存的实际收益在分片上。
5. **关键帧索引按需建立**，不批量预建（L1 规模下 `episode` 有 2847 万行）。

## 这条 ADR 推翻了哪些旧决定

| 旧决定 | 原文 | 现在 |
|---|---|---|
| ADR-0001 | 「单集（不合并）不走这条路，仍是 MP4 字节透传」 | **改**：单集也走 HLS 分片。合并与单集只差 playlist 里有没有 `#EXT-X-DISCONTINUITY` |
| ADR-0003 | 出流 token 无过期、无状态、无 token 表 | **不变**，但形态从 `/s/{token}/e/{uid}` 的两段压成 `/video/{id}` 的一段（签名并入 id） |
| ADR-0004 | 「视频分片缓存由缓存代理负责；本服务不实现任何缓存淘汰」 | **推翻**：播放不再经过 rclone，缓存必须由链路里唯一的服务做 |
| `AGENTS.md` 硬约束 5 | 「缓存上限与 LRU 天数不是本服务的配置，它们只在建议的 rclone 配置里体现」 | **改**：`cache_dir` / `cache_max_mb` / `cache_lru_days` 成为本服务配置 |
| `AGENTS.md`「做什么/不做什么」 | 「做：…分片缓存策略」 | 不变，但现在是真的由本服务执行 |

## 被否决的方案

1. **保留 rclone 缓存，`.strm` 仍指 rclone。**
   否决理由：rclone 的 `--vfs-cache-mode full` 缓存的是**它从上游读到的整个文件**，
   而我们的播放单元是「一集 MP4 里的一段」。要让它缓存分片，就得先把分片做成真实文件
   再让 rclone 去读——那已经在发明一套二级缓存，比直接自己缓存多一跳、多一份配置、
   多一个要排障的进程，且无法预热、无法按 seq 淘汰。
2. **两套路径并存**（单集仍 MP4 透传、只有合并集走 HLS）。
   否决理由：lxz 的口径是「每一条 strm 都是 `/video/xxx.m3u8`」。并存就要维护两条
   播放路径、两套缓存语义、两套测试；而单集的 MP4 透传在有了本地缓存后没有任何优势
   （缓存命中后本地文件读取比回源更快）。
3. **`{id}` 用 RFC-4122 UUID 落库。**
   否决理由：`emby_item` 是派生表、整体重建，UUID 挂在那里每次重建都会变 → URL 漂移 →
   分片缓存全废。挂到 `episode` / `merge_group` 可行但要在两张表各加一列 + 一次迁移，
   换来的只是形态好看。签名 id 已经满足「不透明 / 稳定 / 不可猜 / 无状态」四件事。
4. **缓存 m3u8 文本文件。**
   否决理由：playlist 是 `segment` 表的纯函数视图（< 1 ms 生成），落盘等于第二份真值，
   还要处理索引重建后的失效。
5. **缓存源整片 MP4。**
   否决理由：整片十几 MB 且不可切片复用，与分片缓存重复存储；分片缓存天然按需
   （只看 30 秒就只存 30 秒）。
6. **批量预建关键帧索引。**
   否决理由：2847 万集 × 一次 ffprobe 不可完成。按需建立，30 s 硬上限，超时 503 + `Retry-After`。

## 后果

- 单集首次播放从「零延迟字节透传」变成「ffprobe（0.3–1 s）+ ffmpeg 切片（300–600 ms）」，
  之后是本地文件读取。这是「能缓存」的代价，也是 lxz 要的「转换与缓冲」。
- 播放路径不再依赖 rclone 的缓存参数：rclone 只需要 `serve webdav`（或 `mount`）读元数据。
  Emby/rclone 的配置建议要重写。
- 本服务多两件事要自己保证：缓存不写坏值、生成失败不留半截文件。都写进了
  `docs/CACHE-MODEL.md` 的测试要求。
- SQLite 多一类 `artifact` 行（`kind='segment'`）：行数 = 缓存条目数（默认 8 GB / 650 KB ≈ 1.3 万行），
  不是全库规模，可接受。
- 换 `stream_secret` 会让所有已发出的 URL 失效（与 1.0 一致），但**不影响已缓存的分片**
  （缓存键用 `uid` 不用 `id`）。

## 需要同步修改的文档（本 ADR 生效时一起改）

| 文件 | 要改的段落 |
|---|---|
| `AGENTS.md` | 硬约束 5 的「缓存上限与 LRU 天数不是本服务的配置」整句删除，改为指向 `docs/CACHE-MODEL.md`；「做」清单加「本地分片缓存」 |
| `docs/DEV-STANDARDS.md` | 「配置」表加 3 个 key；「出流（stream）」整章按 `/video` 重写；「数据源」章的 `play_url` 段落改指向 `docs/PLAYBACK-CONTRACT.md` |
| `docs/TEST-STANDARDS.md` | 加 `docs/CACHE-MODEL.md`「测试要求」的 6 条 |
| `docs/ACCEPTANCE.md` | P2/P3/P4 的路径 `/dav/` → `/webdav/`、URL 形态改成 `/video/{id}.m3u8`；P4（缓存代理）改成「本地分片缓存」验收 |
| `README.md` | HTTP 接口表、接到 Emby 一节（库根与 rclone 配置） |

这些文档的修改属于 S2/S3/S5/S6 的落地范围：本 ADR 只定方向，不与设计稿抢同一份文件。
`AGENTS.md` 是项目宪法，**由 PL 确认后改**，不由实现方顺手改。

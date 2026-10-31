# 播放契约（`/webdav` + `/video`）

状态：定稿 v2（2026-10-30 / MKACG-604，按 lxz 2026-10-30 评论重写）
取代：旧口径——`/s/{token}/e/{uid}` 出流家族、`/dav/` 挂载点、「单集走 MP4 字节透传、只有合并集走 HLS」
配套：`docs/CACHE-MODEL.md`、`docs/adr/0007-own-hls-and-local-cache.md`

## 两个口子

| 口子 | 用途 | 认证 |
|---|---|---|
| `/webdav/` | 只读虚拟 WebDAV，Emby/rclone 从这里读目录、`.strm`、`.nfo`、封面 | HTTP Basic（`webdav_user` / `webdav_pass`） |
| `/video/` | 播放：`{id}.m3u8` 取播放列表，`{id}/{seq}.ts` 取分片 | id 内嵌 HMAC 签名（见下），无 Basic（Emby 取 strm 里的 URL 时不好带凭据） |

`/dav/`、`/s/{token}/…` **退役，不留兼容层**：`.strm` 是按请求渲染的，
下一次读取就是新内容，没有存量文件需要兼容。

## `.strm` 的内容

```
{strm_prefix}/video/{id}.m3u8
```

- 一行 + 尾换行，与现状一致。
- `strm_prefix` 仍是 `settings` 的 key（指向本服务自己的对外地址），不新增配置。
- 空 `strm_prefix` 时按**请求的 `Host`** 推导绝对地址
  （`{scheme}://{Host}/video/{id}.m3u8`，scheme 取 `X-Forwarded-Proto`，没有就按是否 TLS）：
  相对路径 Emby 与第三方播放器都放不了（MKACG-616 B1 改的契约）。渲染拿不到请求上下文
  （单测、内部调用）时才退回相对路径。
- `strm_prefix` 的尾 `/` 写入时归一掉，渲染时再去一次，避免 `//video/`（MKACG-616 B2）。
- **每一条 `.strm` 都指向 `.m3u8`**，单集与合并集没有区别。这是本次最关键的统一：
  对外不再有「单集是 MP4、合并集是 HLS」两种形态。

## `id`（lxz 说的 uuid）怎么来

```
id = uid + "-" + hex(HMAC-SHA256(stream_secret, uid)[:12])
uid ∈ { "e<episode.id>", "m<merge_group.id>" }   （沿用现网 emby_item.uid 的两个空间）
```

- 形如 `e1234-9f3a1c02b7d4`（约 17 字符），对外是一个不透明 id。
- **稳定**：`uid` 只依赖 `episode.id` / `merge_group.id`，而这两者不随
  `emby_item` 重建而变（重建只换 `emby_item.id`）。→ 同一集/同一合并组的 URL 永久不变，
  缓存键不会漂（ADR-0003 立的不变量在这里继续成立）。
- **无状态**：不查表、不落库、不过期。校验 = 拆出 `uid`、重算 HMAC、常数时间比较。
- **不可猜**：没有 `stream_secret` 造不出合法 id。
- 换 `stream_secret` 后所有 URL 失效（与现状一致，`docs/DEV-STANDARDS.md`「鉴权」已这么定）。

**D1 已定（lxz 2026-10-30）**：「可以不用 UUID，只要保证不飘移即可」。因此就是上面这个方案。

**为什么不是 RFC-4122 UUID**：真 UUID 要么落库（`emby_item` 整体重建会换值 → URL 飘 →
缓存全废），要么挂到 `episode` / `merge_group`（两张表各加一列 + 一次迁移），
换来的只是「看起来像 UUID」。而「不飘移」这个真正的要求，签名 id 用一条恒等式就满足了：
`id` 只由 `uid` 与 `stream_secret` 决定，`uid` 只由 `episode.id` / `merge_group.id` 决定，
这两样在库的整个生命周期里都不变。

**「不飘移」的可验证形式**（必须有一条测试钉住）：

```
1. 同一集连续 GET /video/{id}.m3u8 两次 → 两次的 {id} 逐字符相同
2. 触发一次全量刷新（UpsertEpisode + RebuildEmbyItems）后再取 → {id} 仍然相同
3. 建/删一个合并组，使某集在 single 与 merged 之间转换 → 该集 old id 返回 **404**
   （`e<id>` 已不在 `emby_item` 里，签名仍然有效所以要区分 401 与 404）、
   new id 稳定；**未受影响的其它集 id 一个都不变**
4. 重启进程（settings 不变）→ 所有 {id} 不变
```

第 3 条是唯一会让 id 变化的情形，而且是语义上必须变（播放单元变了）。
只要 `stream_secret` 不变，id 就不会因为「同步 / 重建派生表 / 重启 / 改目录」而飘。

## `GET /video/{id}.m3u8`

返回自造 playlist（`content-type: application/vnd.apple.mpegurl`）：

```
#EXTM3U
#EXT-X-VERSION:3
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-TARGETDURATION:<最大片长向上取整>
#EXTINF:<片长>,
/video/{id}/0.ts
#EXTINF:<片长>,
/video/{id}/1.ts
…（合并集在集边界插 #EXT-X-DISCONTINUITY）
#EXT-X-ENDLIST
```

- 生成逻辑复用现有 `segment.BuildPlaylist`，只把切片 URI 模板从 `/s/{token}/seg/{uid}/{seq}.ts`
  改成 `/video/{id}/{seq}.ts`。
- playlist 是 `segment` 表的**纯函数视图**，不落盘、不缓存（理由见 `docs/CACHE-MODEL.md`
  「为什么 m3u8 文本不缓存」）。
- `uid` 是单集时切片空间 = 本集片内序；是合并组时 = 跨集连续时间轴（现有 `Timeline`）。
  **两条路径现在都走 HLS**，单集不再是特例。

| 情形 | 状态码 |
|---|---|
| 正常 | 200 |
| id 签名不对 / 缺 `-` | 401 |
| `uid` 在库里不存在 | 404 |
| 该集解析不到直链（`ErrNoStream`） | 502 |
| 关键帧索引建立超时（30 s 硬上限） | 503 + `Retry-After: 5` |

**索引按需建立**：`m3u8` 需要每片时长，因此第一次请求某 `uid` 时若 `segment` 无行，
**同步** ffprobe 建索引（通常 0.3–1 s）再出 playlist；超过 30 s 返回上面的 503。
**禁止批量预建索引**：L1 规模下可播集数量是「每部剧 1 集」，但库里 `episode` 有 2847 万行，
对全库 ffprobe 是不可完成的任务（`docs/L1-CORPUS-SCOPE.md` 取证 D/E）。

## `GET /video/{id}/{seq}.ts`

返回 MPEG-TS 分片。顺序是：**查本地缓存 → 命中即回文件；未命中 → 生成、落缓存、再回**。

- 生成：`ffmpeg -ss … -t … -c copy`（关键帧对齐时可 copy），不可 copy 时转码（现有 `Cutter`）。
- 生成失败：502，**不写缓存**（不许把坏值写进缓存）。
- `Range`：分片是完整不可变对象，支持 `Range` 但不需要分段生成；`HEAD` 返回同样的
  `Content-Length`（硬约束 3）。
- 并发同一个 `(id, seq)` 只生成一次（single-flight），其余请求等同一个结果。
- 预取：**2.0 不做**（MKACG-603「不在范围」；1.0 的 `seg_prefetch` 占位键已删除，MKACG-616）。

这就是 lxz 说的「本服务负责在播放 m3u8 时提供数据转换和缓冲」：
**转换** = ffmpeg 切片/转码，**缓冲** = 本地分片缓存（2.0 不预取）。

## 与现有实现的关系（明确哪些留、哪些退）

| 现有 | 处置 |
|---|---|
| `segment.BuildPlaylist` / `Timeline` / `Cutter` / `segment` 表 | **保留**，只换切片 URI 模板与 id 校验入口 |
| `emby_item.uid`（`e<id>` / `m<id>`） | **保留**，是 `/{id}` 里 `uid` 段的来源 |
| `stream.Server` 的单集 MP4 透传端点（`/s/{token}/e/{uid}`） | **退休**。字节中继 + Range 透传能力留给分片路径复用，不再对外暴露单集 MP4 |
| `stream.Verify` | 保留算法，移到 `/video` 的 id 校验 |
| `webdav.MountPath = "/dav/"` | 改成 `"/webdav/"`；`PROPFIND` 的 href 由该常量生成，无需另改 |
| `ep.PlayURL` 读库取直链（`segment/cutter.go` 现在这么干） | **必须改**：0003 已删该列，改为调用 `plugin.Resolve`（`docs/PLUGIN-CONTRACT.md`） |

## 对外端点总表（2.0）

| 端点 | 方法 | 说明 |
|---|---|---|
| `/webdav/…` | PROPFIND / GET | 只读虚拟文件系统 |
| `/video/{id}.m3u8` | GET / HEAD | 播放列表 |
| `/video/{id}/{seq}.ts` | GET / HEAD | 分片 |
| `/api/dramas` / `/api/dramas/{id}/…` / `/api/merge-groups` | — | 管理 API（不变） |
| `/api/refresh` | POST | 手动触发一轮刷新（`docs/SCHEDULER-MODEL.md`） |
| `/api/status` | GET | 含各源调度状态（同上） |
| `/healthz` | GET | 存活探测（不变） |

## 2.0 明确不做

- 多码率 / `master.m3u8` 变体列表：红果只给 360p，做 ABR 是无源之水。
- 直播语义（`#EXT-X-ENDLIST` 恒在，VOD）。
- 加密分片（`EXT-X-KEY`）：本地单用户，没有需要防的人。
- 转码到高于源的码率/分辨率。
- `/s/` 与 `/dav/` 的兼容层。

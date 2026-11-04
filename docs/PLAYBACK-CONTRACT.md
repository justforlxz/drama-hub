# 播放契约（`/webdav` + `/video`）

状态：定稿 v3（2026-11-02 / MKACG-652，ADR-0011：片段改成每集一片 fMP4）
取代：v2 的「10 秒切片 + 关键帧索引」口径；旧一代口径——`/s/{token}/e/{uid}` 出流家族、
`/dav/` 挂载点、「单集走 MP4 字节透传、只有合并集走 HLS」仍是历史
配套：`docs/CACHE-MODEL.md`、`docs/adr/0007-own-hls-and-local-cache.md`、
`docs/adr/0011-episode-per-segment-fmp4.md`

## 两个口子

| 口子 | 用途 | 认证 |
|---|---|---|
| `/webdav/` | 只读虚拟 WebDAV，Emby/rclone 从这里读目录、`.strm`、`.nfo`、封面 | HTTP Basic（`webdav_user` / `webdav_pass`） |
| `/video/` | 播放：`{id}.m3u8` 取播放列表，`{id}/{seq}.m4s` 取分片（每集一片 fMP4） | id 内嵌 HMAC 签名（见下），无 Basic（Emby 取 strm 里的 URL 时不好带凭据） |

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
uid ∈ { "e<episode.id>", "m<dramaID>_<epFrom>_<epTo>" }   （合并集是自动分块的块句柄）
```

- 形如 `e1234-9f3a1c02b7d4`（约 17 字符），对外是一个不透明 id。
- **稳定**：单集 `uid` 只依赖 `episode.id`；合并集 `uid` 只依赖
  `(dramaID, 块内集号区间)`，不含任何自增主键或派生表主键。→ 集集合与
  `merge_chunk_size` 不变时，同一集/同一块的 URL 逐字符不变、缓存键不会漂
  （ADR-0003 与 ADR-0014 的不变量在这里继续成立）。
- **分隔符**：合并集 uid 用 `_` 分段，**不含 `-`**。id 是 `<uid>-<签名>`，
  `stream.ParseID` 按首个 `-` 切；uid 里带 `-` 会把 404 顶成 401。
- **无状态**：不查表、不落库、不过期。校验 = 拆出 `uid`、重算 HMAC、常数时间比较。
- **不可猜**：没有 `stream_secret` 造不出合法 id。
- 换 `stream_secret` 后所有 URL 失效（与现状一致，`docs/DEV-STANDARDS.md`「鉴权」已这么定）。

**D1 已定（lxz 2026-10-30）**：「可以不用 UUID，只要保证不飘移即可」。因此就是上面这个方案。

**为什么不是 RFC-4122 UUID**：真 UUID 要么落库（改分块 / 重新派生就会换值 → URL 飘 →
缓存全废），要么挂到 `episode` / 新建一张块表（各加一列 + 一次迁移），换来的只是
「看起来像 UUID」。而「不飘移」这个真正的要求，签名 id 用一条恒等式就满足了：
`id` 只由 `uid` 与 `stream_secret` 决定，`uid` 只由 `episode.id` / `(dramaID, 集号区间)`
决定，这两样在「集集合与 N 不变」时都不变。

**「不飘移」的可验证形式**（必须有一条测试钉住）：

```
1. 同一集连续 GET /video/{id}.m3u8 两次 → 两次的 {id} 逐字符相同
2. 触发一次全量刷新（UpsertEpisode）后再取 → {id} 仍然相同
3. 改 `merge_chunk_size`（下一次读取即生效），使某集在 single 与 merged 之间转换 →
   该集 old id 返回 **404**（`e<id>` 已不是当前播放单元，签名仍然有效所以要区分
   401 与 404）、new id 稳定；**未受影响的其它集 id 一个都不变**
4. 重启进程（settings 不变）→ 所有 {id} 不变
```

第 3 条是唯一会让 id 变化的情形，而且是语义上必须变（播放单元变了）。
只要 `stream_secret` 不变，id 就不会因为「同步 / 重启 / 改目录」而飘。

## `GET /video/{id}.m3u8`

返回自造 playlist（`content-type: application/vnd.apple.mpegurl`）：

```
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-TARGETDURATION:<最大集时长向上取整>
#EXTINF:<该集真实时长>,
/video/{id}/0.m4s
#EXT-X-DISCONTINUITY                ← 只在合并集的集边界出现
#EXTINF:<该集真实时长>,
/video/{id}/1.m4s
#EXT-X-ENDLIST
```

- **一集一片**（ADR-0011）：播放单元的分片数 = 成员集数，单集 1 片、合并块 N 片；
  `seq` 就是块内成员下标，不存在「集内第几片」这一层。片段后缀 `.m4s`。
- `#EXTINF` 取该集真实时长，来源只有一处：`episode.duration_sec`（见下「时长按需探测」）。
  `#EXT-X-TARGETDURATION` 是它的最大值向上取整。
- playlist 是派生视图（`Timeline` 的纯函数渲染），不落盘、不缓存（理由见
  `docs/CACHE-MODEL.md`「为什么 m3u8 文本不缓存」）。
- 单集与合并集走同一个函数：单集的块里只有一集，因此没有 `#EXT-X-DISCONTINUITY`。
  **两条路径都走 HLS**，单集不是特例。

| 情形 | 状态码 |
|---|---|
| 正常 | 200 |
| id 签名不对 / 缺 `-` | 401 |
| `uid` 在库里不存在 | 404 |
| 该集解析不到直链（`ErrNoStream`，含受保护剧只给 10 秒预览） | 502 |
| 该集需要登录态或无权限（`ErrLoginRequired`：站点回 403 `protected_playback_unavailable`，含 Cookie 未配/已失效） | 403（`code=forbidden`） |
| 该集仅提供不支持的形态（`ErrUnsupported`：只给 DASH-CENC 且拿不到 key） | 501（`code=unsupported`） |
| 时长探测超时（30 s 硬上限） | 503 + `Retry-After: 5` |

**502 的语义（MKACG-626 契约 2）**：可播性只在请求时判定，失败一律 502，不降级。
「受保护剧只给 10 秒预览」不产出「200 + 10 秒 playlist」：预览不当直链交付，站点通道与
App 通道也不可用时就是 502 + 一条 `WarnContext`（`uid` / `ep_no` / `channel` / 错误类别）。
不回落、不静默给一个「看起来能播」的短片 —— 判据与顺序见 `docs/PLUGIN-CONTRACT.md`
「通道与取流顺序」。

**403 / 501 的语义（MKACG-676）**：这两种不是上游故障，不该显示成「源坏了」。
它们只可能来自兜底路径：App 通道（主路径，带请求签名）不成时站点播放通道给出的确定答复。
403 = 这一集需要登录态（`hongguo_cookie` 没配、已失效，或该账号本就无权限）；补一条
`PATCH /api/settings` 就能播，重试无用。501 = 这一集只提供加密流（`play_info` 里只有
`kid` / `player_access_key`，拿不到 ffmpeg 能用的 16 字节 key），本服务不实现 DASH-CENC
解密。两格都不进 `WarnContext` 的「上游失败」口径，文案直接告诉人该干什么。

**时长按需探测**：`m3u8` 需要每集时长，而 `episode.duration_sec` 是唯一真值。
第一次播放某 `uid` 时，`duration_sec` 为 0 的成员**同步** ffprobe 一次
（只取 `format=duration`，通常 0.3–1 s）再出 playlist；整批探测超过 30 s 返回上面的 503。
`duration_sec` 已非 0 的成员不再探测（零外呼）。

同一次解析还顺手对齐 `segment` 行的通道：本次解析出的通道与行里不一致时覆盖它
（通道翻转的判据仍然只有 `channelOf` 一处）。分片端点复用这一行，所以命中缓存的分片
请求不解析直链。

**禁止批量预建**：L1 规模下可播集数量是「每部剧 1 集」，但库里 `episode` 有 2847 万行，
对全库 ffprobe 是不可完成的任务（`docs/L1-CORPUS-SCOPE.md` 取证 D/E）。

探测到的源时长会回填 `episode.duration_sec`（只填 0，不覆盖非零值）；对外 runtime 在
列举时现算，`.nfo` 的 `<runtime>` 与 playlist 的 `#EXTINF` 下一次读取就是真实值；没播过
的集仍是 0。细节见 `docs/DEV-STANDARDS.md`「时长探测」。

## `GET /video/{id}/{seq}.m4s`

返回**一整集**的自包含 fMP4。顺序是：**查本地缓存 → 命中即回文件；未命中 → 生成、
落缓存、再回**。

- `seq` 直接是播放单元里的成员下标（第 `seq` 集），越界即 404。
- 生成：`ffmpeg -c copy -movflags +frag_keyframe+empty_moov+default_base_moof -f mp4`，
  **一条路径**：不切、无 `-ss/-t`、无 `-output_ts_offset`。片头自带 `ftyp+moov`，
  所以每一片都能单独 `ffprobe`（自包含），playlist 不需要 `#EXT-X-MAP`。
- **集边界时间戳**：缓存里的片段一律 0 基（自包含、只由「集 + 通道」决定），出流时按本片
  在播放单元里的起点（前面各集 `#EXTINF` 之和）给 `moof/traf/tfdt` 的
  `baseMediaDecodeTime` 加 `offset × 轨道 timescale`（每轨 timescale 不同），因此整条
  playlist 的 PTS 单调递增。**只靠 `#EXT-X-DISCONTINUITY` 不够**：MSE 系播放端按它重定
  时间轴，但 ffmpeg 系播放端（Emby 转码/DirectStream、mpv）只按 PTS 解码，会因回退丢帧
  （实测 3 集 325 帧只解出 275 帧；平移后 325/325）。ffmpeg 的 `-output_ts_offset` 在
  fMP4 上无效（mov 封装器把 tfdt 归一到轨道起点），所以平移在出流这一步做。
  起点 = 前面各集 `duration_sec` 之和；前置集还没回填（只请求分片、或首播时 playlist 与
  分片并发）时**先补齐再平移**，不能按 0 出流（那等于又回到「每片从 0 起」，实测 1550 包
  只解 800 帧）。补齐会解析一次直链 + `ffprobe` 并回填，失败按 502/503 返回。
  细节与实测表见 ADR-0011「修正：集边界时间戳」，回归钉子
  `TestMergedPlaylist_DecodesEveryFrame` 与 `TestSegmentDirectRequestFillsDuration`。
- 加密片源：`Resolve` 带 `Key`（`plugin.Stream.Key`，16 字节 AES-128）时，转封装与
  时长探测的命令 MUST 在 `-i` **之前**加 `-decryption_key <hex>`，交给 ffmpeg 的
  CENC-AES-CTR 原生解密；本服务不实现解密、不落整集明文。key 只在本次请求的内存里
  过一手：不进日志、不进错误文本、不落库、不进缓存键
  （`docs/PLUGIN-CONTRACT.md`「加密切片与 Key」）。
- 出站身份**按通道两份**（MKACG-638）：`ffmpeg` / `ffprobe` 取直链 MUST 带 `-headers`，
  但同一份头不能同时喂给两个通道 —— 网页通道（明文直链 `hongguoduanju.com/video/public/…`）
  要浏览器 UA + 源站根 `Referer`，与同步抓页面是同一份身份；App 通道**只带 UA、不带
  `Referer`**，它的媒体 CDN 对带站内 `Referer` 的请求回 `403`（实测二分：仅 `Referer`
  403，无头 / 仅 UA 都能探出完整集时长）。两条通道各一份、MUST NOT 互相顶掉。
  挑哪一份的判据与「缓存键里的通道」同一个：本次解析的直链带不带内容密钥
  （`Stream.Key`）。两份都由装配根从插件常量派生、按通道注入
  （`internal/segment` 不认识任何源，只按判据挑一份），理由见 `docs/PLUGIN-CONTRACT.md`
  硬性要求 8。
- 生成失败：502，**不写缓存**（不许把坏值写进缓存）。
- `Content-Type: video/mp4`。`Range`：分片是完整不可变对象，支持 `Range` 但不需要分段
  生成；`HEAD` 返回同样的 `Content-Length`（硬约束 3）。
- 并发同一个缓存键只生成一次（single-flight），其余请求等同一个结果。
- **缓存键按集带通道**（ADR-0011 D3）：`cache.SegmentKey = sha256("e<episode.id>" \x1f channel)`，
  落 `<cache_dir>/seg/<hash 前 2 位>/<hash>.m4s`。`seq` 不进键：合并集第 k 片、单集播放、
  重新分组后的同一集命中同一个文件。通道必须进键 —— 同一集的网页源与 App 源字节流不同
  （App 源是 CENC 密文），只按集算键会串味。通道的唯一判据是「本次解析有没有带内容
  密钥」：带 `Key` = App 通道，否则网页通道——契约里没有通道名字段，不新增。
- 预取：**2.0 不做**（MKACG-603「不在范围」；1.0 的 `seg_prefetch` 占位键已删除，MKACG-616）。

这就是 lxz 说的「本服务负责在播放 m3u8 时提供数据转换和缓冲」：
**转换** = ffmpeg 整集转封装，**缓冲** = 本地分片缓存（2.0 不预取）。

## 与现有实现的关系（明确哪些留、哪些退）

| 现有 | 处置 |
|---|---|
| `segment.BuildPlaylist` / `Timeline` / `Cutter` / `segment` 表 | **保留**，`Timeline` / `Slice` 收敛成「一集一片」，`segment` 表收窄成 `(episode_id, channel)`（0006） |
| 对外剧集 uid（`e<id>` / `m<dramaID>_<from>_<to>`） | **保留**，是 `/{id}` 里 `uid` 段的来源；由当前分块规则运行时派生（ADR-0014） |
| `stream.Server` 的单集 MP4 透传端点（`/s/{token}/e/{uid}`） | **退休**。字节中继 + Range 透传能力留给分片路径复用，不再对外暴露单集 MP4 |
| `stream.Verify` | 保留算法，移到 `/video` 的 id 校验 |
| `webdav.MountPath = "/dav/"` | 改成 `"/webdav/"`；`PROPFIND` 的 href 由该常量生成，无需另改 |
| `ep.PlayURL` 读库取直链（`segment/cutter.go` 现在这么干） | **必须改**：0003 已删该列，改为调用 `plugin.Resolve`（`docs/PLUGIN-CONTRACT.md`） |

## 对外端点总表（2.0）

| 端点 | 方法 | 说明 |
|---|---|---|
| `/webdav/…` | PROPFIND / GET | 只读虚拟文件系统 |
| `/video/{id}.m3u8` | GET / HEAD | 播放列表 |
| `/video/{id}/{seq}.m4s` | GET / HEAD | 分片（整集 fMP4） |
| `/api/dramas` | — | 管理 API |
| `/api/refresh` | POST | 手动触发一轮刷新（`docs/SCHEDULER-MODEL.md`） |
| `/api/status` | GET | 含各源调度状态（同上） |
| `/healthz` | GET | 存活探测（不变） |

## 2.0 明确不做

- 多码率 / `master.m3u8` 变体列表：红果只给 360p，做 ABR 是无源之水。
- 直播语义（`#EXT-X-ENDLIST` 恒在，VOD）。
- 加密**输出**分片（`EXT-X-KEY`）：本地单用户，没有需要防的人。源的加密输入是另一件事，
  解密在 ffmpeg 那一侧做（见 `GET /video/{id}/{seq}.m4s`「加密片源」）。
- 转码：源是 HEVC，容器与切分改完之后能否播出取决于播放端的 HEVC 能力
  （`isTypeSupported`），转码是独立决策（ADR-0011「黑屏结论」）。
- 转码到高于源的码率/分辨率。
- `/s/` 与 `/dav/` 的兼容层。

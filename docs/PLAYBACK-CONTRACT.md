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
- **条件请求（MKACG-682）**：响应带 `ETag: "<正文 sha256 前 8 字节>"` 与
  `Cache-Control: no-cache`；`If-None-Match` 命中（支持 `*` / 逗号分隔 / `W/`）
  返回 `304`、无正文。内容只由播放单元与时长决定，时长回填或换块都会改变 ETag。
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

## 直链复用与失败自愈（ADR-0016）

直链与解密 key 存在 `episode_stream(episode_id, channel)`，每行带 `expires_at`：

- **未过期直接复用、零外呼**：`Resolver.Direct` / `DirectMany` 先读库；播放列表与分片
  生成共用这份缓存，一个合并块首播之后的请求不再重复申请。
- **过期才重解析一次**：`expires_at` = 插件给的 `Stream.ExpiresAt`，没给就是
  `fetched_at + stream_url_ttl_sec`（默认 900 s，可调、即时生效）。
- **CDN 拒绝（401/403/404）**：作废**这一集**的直链（不是整部剧）→ 重解析一次 →
  重试一次；再失败按上面的状态码矩阵上报，不静默重试到超时。重解析后通道翻转则上报，
  不把新通道的字节写进旧通道的缓存键。
- **单飞**：同一集的并发请求只解析一次，其余等同一份结果。
- **敏感面**：`url` 与 `key` 都不进日志、不进错误文本；错误只说「某集某通道的直链」。
  换 `stream_secret` 不会让直链缓存失效（直链与出流签名无关）。

## 播放触发的预取与退避（ADR-0017）

播放列表成功之后，播放面会为**整部剧**起一次后台预取（`kickPrefetch`）：只申请直链、
不生成分片，按「当前块 → 下一块 → 其余块」的顺序，让拖到相邻块时落到
`episode_stream` 的缓存命中。

- **触发点**：默认在**首个分片请求之后**（`prefetch_on=first_segment`，MKACG-687），
  这样只读播放列表的探测（ffprobe 读 m3u8 之类）不会白烧预取、也不占前台 ffmpeg 闸位；
  `prefetch_on=playlist` 回退旧的「播放列表成功即预取」行为。

- **只复用 `Resolver.DirectMany`**：缓存判定、单飞、落库、通道判据都只有那一份实现；
  预取不是第二条解析路径，也不改变任何对外路由与 id 形态。
- **前台不等预取**：任务在后台跑，结果不进入当前请求；预取失败不影响本次播放。
  `ServeSegment` 只在分片出流之后触发预热（`startPlaybackWarmup`），不在请求前抢闸
  （MKACG-687）。
- **限速**：`prefetch_concurrency`（默认 1，`0` = 关闭）限制同时在途的批数（一批 =
  一个合并块）；默认串行，避免和前台抢 App 通道的换身份配额。
- **失败退避**：失败落 `episode_stream_fail`（`reason` + `retry_after`），一次任务对
  每一集最多尝试一次；`prefetch_retry_sec`（默认 900）之后的下一次播放才会再试。
  **前台从不读这张表**：正在播的那一集仍现解析，失败记录不会变成直接 502。
- **取消**：「不再起新批」与「中断在途批」是两个信号。换剧只发前者（关 `stop`），
  在途的那一批用旧任务自己的进程 ctx 跑完；只有进程停机才 cancel 在途批。因此换剧
  不会把 `context.Canceled` 传播给前台等同一集的请求（那会被映射成 502）。
- **并发上限是进程级的**：换剧后新任务先等旧任务在途批收尾再开始，切剧瞬间不会
  翻倍外呼，仍受 `prefetch_concurrency` 约束。
- **没有周期巡检**：没有 ticker / 常驻循环；没人播放时零上游请求。
- 敏感面不变：失败记录只有集标识、原因分类与时间，不含 URL / key。

## 播放触发的分片预热（ADR-0018）

同一个触发点（默认：首个分片请求之后；`prefetch_on=playlist` 时仍是播放列表成功后）
还会起一次**分片**预热（`kickSegmentPrefetch`）：把当前块里当前集之后的成员、以及后面
`segment_prefetch_blocks` 个块（默认 1，`0` = 关闭）真的转封装成 `.m4s` 落进缓存，
让「拖到下一集」的首片直接命中（MKACG-687 之后探测不再触发它）。

- **有界**：不做整库 / 整剧预切；顺序执行（同时只跑一个 ffmpeg），不越上游闸。
- **前台优先**：与前台共用 ffmpeg 闸与 single-flight。槽位被预热占满时，前台请求会
  取消在跑的预热；预热拿不到空槽位就跳过这一集，从不排队堵住前台。
- **可丢**：换播放单元 / 换剧 / 停机 / 前台静默超过 30 s 都取消，取消后不再生成新分片；
  失败只记日志，不影响任何一次播放。
- **淘汰优先**：预热生成的分片带 `artifact.prefetched` 标记，容量吃紧时先被淘汰
  （`docs/CACHE-MODEL.md`「淘汰」）。
- 与直链预取的分工：ADR-0017 只申请直链、不生成字节；本段生成字节。两者都不改对外
  路由、id 形态与切片粒度。

## `GET /video/{id}/{seq}.m4s`

返回**一整集**的自包含 fMP4。顺序是：**查本地缓存 → 命中即回文件；未命中 → 边生成
边出流（首个分片落盘即可播）、生成完落缓存**（MKACG-686）。

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
  CENC-AES-CTR 原生解密；本服务不实现解密、不落整集明文。key 与直链一起落库
  （`episode_stream`，ADR-0016）、一起失效重取，但 MUST NOT 进日志、错误文本与缓存键
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
- **冷启动边生成边出流（MKACG-686）**：缓存未命中时不再「整集转完再回」，而是把
  ffmpeg 的 stdout 直接接进出流：`ftyp+moov`、每个 `moof+mdat` 一落盘就发给客户端，
  因此首播等待 ≈ 首个分片（默认 ffmpeg 的 `frag_keyframe`，关键帧 1 s 量级），不再
  ≈ 整集 remux。响应因此是 **chunked、不设 `Content-Length`** —— 总长度只有整集转完
  才知道，先给长度就只能等整集，那正是首播慢的根因。
  - 这是硬约束 3「每个虚拟文件给出精确 `Content-Length`」的**唯一例外**，只适用于
    `GET` 的冷路径；字节流仍是完整整集产物（与命中路径逐字节一致，回归钉子
    `TestSegment_StreamedBytesEqualDirectRemux`）。
  - `HEAD` 与带 `Range` 的请求**不走流式**：等整集生成完再回，长度与区间仍精确
    （`Content-Length` / `206 + Content-Range`）。WebDAV / rclone 读的是文件元数据，
    不受这条例外影响。
  - 首字节前失败：仍回干净的错误码（与命中路径同一套 502/403/501 映射）；领跑者被
    后台预热抢占取消时可换新领跑者重领一次（`segmentReleads = 1`），预算耗尽回
    **503 + `Retry-After: 5`**，绝不留「200 + 空 body」。已经开始出流后失败：chunked
    流中断（客户端看到 `unexpected EOF`），**绝不补一个假的 `Content-Length`**。
  - 半成品落在 `<cache_dir>/tmp/.partial-*`，**不进 artifact 表**，因此进程重启后
    不会被当成完整产物服务；只有转封装成功、原子 rename 到 `seg/` 并写行之后才可命中
    （`docs/CACHE-MODEL.md`）。
  - 可观测：日志 `segment streamed` 带 `bytes` / `first_segment_ms` / `total_ms`；
    `/api/playback` 的会话里带 `time_to_first_segment_ms`（首个分片到达客户端前耗时）。
- `Content-Type: video/mp4`。命中路径 `Range` 由 `ServeContent` 就地切完整文件；
  冷路径见上一条。
- 并发同一个缓存键只生成一次（在途表 single-flight，前台与预热共用），其余请求等
  同一份字节；同一集还在生成时第二次播放会直接读这份在途字节，不会起第二个 ffmpeg。
- **缓存键按集带通道**（ADR-0011 D3）：`cache.SegmentKey = sha256("e<episode.id>" \x1f channel)`，
  落 `<cache_dir>/seg/<hash 前 2 位>/<hash>.m4s`。`seq` 不进键：合并集第 k 片、单集播放、
  重新分组后的同一集命中同一个文件。通道必须进键 —— 同一集的网页源与 App 源字节流不同
  （App 源是 CENC 密文），只按集算键会串味。通道的唯一判据是「本次解析有没有带内容
  密钥」：带 `Key` = App 通道，否则网页通道——契约里没有通道名字段，不新增。
- 预取：**分片预热**按合并块做（MKACG-682 / ADR-0018）：默认在首个分片请求之后预热
  当前块剩余成员与后面 `segment_prefetch_blocks` 个块（`prefetch_on=playlist` 时回到
  播放列表成功后触发，MKACG-687），与前台共用 single-flight 与 ffmpeg 闸，
  预热生成的分片先淘汰。**直链**的整剧预取是另一件事（ADR-0017）：它只申请播放直链、
  落 `episode_stream`，不生成、不落任何分片字节。

这就是 lxz 说的「本服务负责在播放 m3u8 时提供数据转换和缓冲」：
**转换** = ffmpeg 整集转封装，**缓冲** = 本地分片缓存（按合并块预热）。

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
| `/api/playback` | GET | 正在播放的内存会话与汇总（只读；重启清空，TTL 由 `play_session_ttl_sec` 控制） |
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

# ADR-0019：分片冷路径边生成边出流（首片不等整集）

状态：**已接受**（2026-11-05，MKACG-686）

配套：`docs/PLAYBACK-CONTRACT.md`「`GET /video/{id}/{seq}.m4s`」、
`docs/CACHE-MODEL.md`、ADR-0011（每集一片 fMP4）、ADR-0018（分片预热）

## 背景

680 / 681 / 682 / 683 之后，播放列表、直链、预热都不再是首播瓶颈，但第一次点播某一集
仍要等几秒到几十秒，第二次播放很快。根因在出流结构，不在上游：

- `ServeSegment` 未命中 → `caches.GetOrGenerate` → 生成函数里调 `remux(...)`；
- `remux` 是 `cmd.Output()`：**整集字节先全收进内存**，ffmpeg 退出后才返回；
- 之后才 `WriteHeader` + `ServeContent`。

所以「首片」实际等的是一次完整的「整集下载 + 整集重封装 + 整集读进内存」。
基线实测（90 s / 320×180 集，源站限速 1.5 MB/s，5 集）：

| 指标 | 改造前 | 改造后 |
|---|---|---|
| 首字节 P50 / P95 | 2042 ms / 2048 ms | 55 ms / 59 ms |
| 首个完整 `moof`（首个可解码分片）P50 / P95 | 2042 ms（整集就绪才写响应） | 76 ms / 79 ms |
| 整片读完 P50 | 2043 ms | 2032 ms（不变，带宽决定） |
| 同集二次播放 P50 / P95 | 5 ms / 7 ms | 8 ms / 9 ms |
| 整集 remux 单跑 | 2030 ms | 2024 ms |

「首片等待 ≈ 整集 remux」由此坐实；改造后首片只等**第一个分片**，整片总时长不变。

## 决策

### 1. 生成函数变成「写进 writer」，不再返回 `[]byte`

`cache.WriteFunc = func(ctx, io.Writer) error`；`remux` 拆出 `remuxTo(ctx, logger, src,
headers, w)`（`cmd.Stdout = w`，stderr 仍只进日志）。字节一产出就往下一层推，不再有
「整集在内存里」这一步。

### 2. 缓存层：在途表 + 可读流

`Cache.GetOrStream(ctx, spec, gen, prefetched)` 返回 `*Stream`：

- 命中：`*Stream` 就是已完成的文件视图（`Cached() == true`）；
- 未命中：在**在途表**（`Cache.builds`）里登记，领跑者在 goroutine 里跑 `gen`，
  写进 `tmp/.partial-*` 并同步广播进度；读者 `Reader(ctx, off)` 按已落盘长度读，
  写完一段读一段；
- 转完：`Chmod` → `rename` 到 `seg/<hash>.m4s` → 写 `artifact` 行 → 从在途表摘除；
- 失败：删半成品、摘除在途表、读者拿到终止错误。

`GetOrGenerate` 变成 `GetOrStream + 读完` 的薄适配；`Prefetch` 变成 `GetOrStream +
Wait`。**前台与预热共用同一张在途表**，因此同一集并发播放 + 预热合计只起一个 ffmpeg
（验收第 5 条，回归钉子 `TestSegment_ConcurrentPlaybackOneFFmpeg` 用 PATH shim 真数进程）。

四条并发不变量（代码评审补的钉子，`TestSingleFlightLeavesNoWindow` /
`TestGetOrGenerateRetriesAfterLeaderCancel` / `TestSegment_RetriesAfterLeaderCancel`）：

- **读路径与 `rename` 同锁切**：`rename` 与 `Stream.path` 指向最终文件在同一临界区完成，
  `Reader` 也在锁内 `open` —— 否则「半成品已被搬走、读路径还没切」这一小段里新读者会
  `open` 到不存在的文件，同集并发播放的后到者直接 500。
- **锁内复查缓存**：`GetOrStream` 进锁前查一次缓存，进锁后再查一次。领跑者「写 artifact
  行 → 摘在途表」之间，晚到者的第一次查询可能落在两者之前；不在锁内复查就会成为第二个
  领跑者，再跑一遍 ffmpeg（MKACG-640 窗口 2）。
- **取消后由调用方重领一次**：领跑者可能是后台预热，被前台 `preempt` 取消时挂在同一份
  字节上的前台请求若一个字节都还没拿到，`ServeSegment` / `GetOrGenerate` 换一个新领跑者
  重试一次；已经出流则按「流中断」处理，不补假长度。
- **重领预算显式且有明确出口**：`segmentReleads = 1`，预算耗尽时 `streamSegment` 必须写出
  错误（503 + `Retry-After`）而不是 `return` —— 后者会留下「200 + 空 body」，播放端把它当
  解码失败，比 5xx 更难定位（AC-4）。钉子：`TestSegment_ReleadBudgetExhaustedWritesError`
  （连续两次领跑者被取消，断言状态码非 200 且响应非空）。

### 3. 出流：冷路径 200 + chunked，不设 `Content-Length`

`ServeSegment` 未命中时把流接进响应，每个 `moof+mdat` 落盘即 `Flush`。

**这是硬约束 3「每个虚拟文件给出精确 Content-Length」的唯一例外，只限 `GET` 冷路径**：
整集长度要等 ffmpeg 退出才知道，先给长度就只能等整集——正是要修的问题。取舍如下：

| 情形 | 行为 |
|---|---|
| `GET` 未命中 | 200 + chunked，字节 = 完整整集产物（与命中路径 sha256 相同） |
| `GET` 命中 | 不变：`Content-Length` 精确，`Range` 由 `ServeContent` 切 |
| `HEAD`（未命中） | 阻塞到整集转完，回精确 `Content-Length` |
| 带 `Range`（未命中） | 阻塞到整集转完，回 `206` + 精确 `Content-Range` |
| 第一个字节前失败 | 干净错误码（502 / 403 / 501），与命中路径同一套映射 |
| 已出流后失败 | chunked 流中断（客户端见 `unexpected EOF`），**绝不补假长度** |

WebDAV / rclone 读的是文件元数据与 `HEAD` / `Range`，不在这条例外里。

### 4. 半成品不进 artifact 表

`tmp/.partial-*` 没有行指向它，进程重启后不可能被当成完整产物命中（验收第 6 条，
回归钉子 `TestPartialFileNeverServedAsComplete`）；淘汰第一步就是清扫 `tmp/`（1 小时）。
「未完成产物」与「prefetched 预热片」是两件事：后者已经是完整产物，只是带
`artifact.prefetched` 标记，容量吃紧时先于播过的分片被淘汰（ADR-0018）。

### 5. 时间轴平移改成增量解析

出流前仍要按集边界平移 `moof/traf/tfdt`（ADR-0011 D6'），但不能再等整集：
`shiftStream(io.Reader, offset)` 逐顶层 box 处理——`moov` 到齐时建 track→timescale，
每个 `moof` 到齐时平移，其它字节（`ftyp` / `mdat`）直接往前搬。它的输出必须与
`shiftTimeline` 逐字节一致（回归钉子 `TestShiftStream_MatchesShiftTimeline`，含逐字节读、
box 头被拆开的情形）；`offset == 0` 时原样透传。

### 6. 可观测

- 日志 `segment streamed`：`bytes` / `first_segment_ms` / `total_ms`；`segment generated`
  保留（领跑者的总耗时与字节）。
- `/api/playback` 会话新增 `time_to_first_segment_ms`：从进入 `ServeSegment` 到首块写出。
  命中路径为 0（没有「等待生成」这段）。

## 影响

- **正确的**：首播等待从「整集」降到「首个分片」；整片总时长、字节内容、缓存键、
  切片粒度、m3u8 结构、合并集机制、WebDAV 长度语义全部不变。
- **放弃的**：冷路径 `GET` 没有 `Content-Length`（见决策 3 的取舍表）。任何依赖「分片
  `GET` 必有长度」的新客户端都不允许接入；需要长度时用 `HEAD`。
- **风险**：生成中途失败会让响应截断（chunked 无法撤回）。播放端会当网络错误重试，
  下一次请求重新生成；不会留下可命中的坏产物。

## 明确不做

- 不改切片粒度、不引入 `EXT-X-BYTERANGE` / 多片 m3u8（范围外）。
- 不做「先返回已生成区间 + 明确错误」的伪完整语义：截断的 segment 会让 HLS 播放端
  把 2 秒当成本集结束、跳到下一集，比等一下更坏。
- 不给冷路径做「边生成边支持 `Range`」：区间语义要求总长度已知，与首片优先互斥。

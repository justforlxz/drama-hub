# ADR-0018：分片缓存预热、预热标记淘汰与 playlist 条件请求

状态：**已接受**（2026-11-05，MKACG-682）

配套：`docs/CACHE-MODEL.md`、`docs/PLAYBACK-CONTRACT.md`、ADR-0011（每集一片 fMP4）、
ADR-0017（直链预取）

## 背景

MKACG-680 / 681 之后，播放列表与直链已经不再等上游页面请求，但**分片本身仍然是
按需生成**：第一次播到某一集，`ServeSegment` 才现场跑一次整集 `ffmpeg -c copy`
（`remux`），落盘后才返回首片。5 分钟 1080p 的集要等整集转封装完才有第一个字节。

缓存的原材料（`.m4s` 按「集 + 通道」落盘、single-flight、LRU/容量淘汰）在
MKACG-652 / ADR-0011 已经齐了，缺的是**预读**与**淘汰优先级细分**。同时 playlist
每次都是 200 + 全文，客户端连条件请求都做不了。

这与 AGENTS.md 硬约束 5 里「2.0 不做分片缓存预热」的旧口径相反；本条 ADR 是该口径的
契约反转，AGENTS.md 与 `docs/CACHE-MODEL.md`、`docs/DEV-STANDARDS.md` 同步改。

## 决策

### 1. 按合并块预热，触发点仍只有播放列表成功

`ServePlaylist` 在 `kickPrefetch`（直链预取）之后调 `kickSegmentPrefetch(uid, u.eps)`：

- 计划 = **当前块里当前集之后的成员** + 后面 `segment_prefetch_blocks` 个块
  （默认 1 = 「当前块 + 下一块」；`0` = 关闭）。当前集不预热，它走前台生成。
- 纯函数 `segmentPrefetchPlan` 从 `store.DramaBlocks` 的结果里取，顺序固定、
  有界（≤ 当前块剩余 + N 块成员），不滚雪球到整部剧。
- 同一播放单元（`uid`）已经在跑就跳过：重复请求 playlist 不重启任务。
- 非阻塞：只加锁、起 goroutine；失败只记日志，不影响本次响应。

### 2. 与前台共用一个闸，但前台优先

预热与前台共用 `Cutter.gate`（ffmpeg/ffprobe 并发闸）和 `cache.Cache` 的
single-flight，区别只在取槽方式：

- **前台** `acquire`：先试空位；没空位就取消正在跑的后台预热（`ffmpegGate.preempt`），
  再排队等它释放。预热在跑时不排队、不占位。
- **后台** `acquireBackground`：拿不到空槽位就**跳过这一集**，从不排队。预热是
  可丢的后台工作，唯一目的是让前台更快，不能反过来堵住前台。
- 预热取消的是它自己的生成 ctx，`ffmpeg` 子进程随 ctx 结束被杀。

取消可能落在「前台也在等的那个 single-flight」上：如果预热正好是领跑者，前台等待者
会读到 `context.Canceled`。因此 `flightGroup.do` 增加一条规则 —— **取消类错误不是
内容失败**：等待者摘掉这条已关闭的 flight 并自己领跑，绝不把取消映射成 502。
非取消类错误（ffmpeg 真失败）仍原样传播。

配套地，预热生成闭包里**每个失败出口**（`resolve` / `refreshRejected` / `remux`）都要
再看一次 `ctx.Err()`：这三个函数只会把取消折成上游错误（`resolveFailure` 拼原文 +
`httpx.ErrUpstream`，`remux` 包成 `ffmpeg failed: ErrUpstream`），不还原取消语义，上面
那条 flight 规则在真实路径上就不成立。还原只做在预热闭包里，`resolveFailure` /
`playErr` / `remux` 的全局语义不动（前台的 403/501/502/超时规则不能跟着变）。

### 3. 预热生成的分片带 `prefetched` 标记，容量吃紧时先淘汰

迁移 `0012` 给 `artifact` 加 `prefetched BOOLEAN NOT NULL DEFAULT FALSE`：

- 预热生成写 `true`；前台生成写 `false`；前台命中/生成成功都会清成 `false`
  （`store.MarkArtifactPlayed`）。
- `PutArtifact` 的冲突更新用 `prefetched = artifact.prefetched AND excluded.prefetched`：
  **只升不降**，预热重写不会把「用户看过」降级成「可优先淘汰」。
- 超限淘汰（`cache_max_mb`）分两轮：先按 `last_used_at` 升序删 `prefetched=true`
  的分片，删完仍超限才动播过的缓存。过期淘汰（`cache_lru_days`）不变 ——
  那一项问的是「多久没用」，与预热无关。

### 4. playlist 只加条件请求头，不引入服务端文本缓存

`BuildPlaylist` 是纯函数，正文只由播放单元与时长决定：

- `ETag: "<sha256(正文)[:8] 的十六进制>"`（强 ETag），`Cache-Control: no-cache`
  （每次使用前必须回源校验，命中 304 时拿到的仍是最新播放列表）。
- `If-None-Match` 命中（支持 `*`、逗号分隔、`W/` 弱标签）→ `304 Not Modified`，
  无正文。
- **不做**服务端 m3u8 缓存：落盘等于第二份真值，还要处理「时长回填但文本没失效」
  （`docs/CACHE-MODEL.md`「为什么 m3u8 文本不缓存」）。

### 5. 取消条件：换单元 / 换剧 / 停机 / 前台静默

- 新的 playlist 到来且播放单元变了：取消旧任务（立即，不等这一集跑完）。
- `StopPrefetch`（进程停机）一并停分片预热并等待收尾（≤ 5 s）。
- **前台静默**：`Cutter.lastPlay` 记录最近一次 playlist/分片成功的时间；预热在每集
  开始前检查，超过 `segmentPrefetchIdle`（30 s）就取消，日志
  `segment prefetch run cancelled` + `reason=idle`。播放结束或客户端断开后不会再
  生成新分片。
- 取消之后不再有新的 `segment prefetched` 日志与 artifact 行。

### 6. 新参数

| key | 默认 | 范围 | 生效 |
|---|---|---|---|
| `segment_prefetch_blocks` | `1` | 0–10 | 下一次播放列表触发时（每个任务开始时读一次） |

`0` = 关闭分片预热（回退开关）。ffmpeg 闸容量仍是固定的
`min(4, max(1, NumCPU/2))`，不做成可调项。

## 实测（同机同网，MKACG-682 验收）

`DRAMA_HUB_BENCH=1` 跑 `TestBenchSegmentPrefetch`（同一构建内用
`segment_prefetch_blocks=0` 复现改造前，默认值复现改造后；每轮新建库与缓存目录，
1 秒 fixture，n=12）：

| 场景 | 改造前 P50 / P95 | 改造后 P50 / P95 |
|---|---|---|
| 冷启动首片（当前集，必须转封装） | 78.1 ms / 111.2 ms | 80.9 ms / 100.1 ms |
| 拖到下一集首片（已预热） | 55.6 ms / 64.1 ms | 1.8 ms / 2.5 ms |

冷启动首片是同一件事（都要现场转封装），差异在噪声内 —— 预热没有拖慢前台；
跳转首片从「一次整集 remux」降到「读本地文件」。

命令：

```
DRAMA_HUB_TEST_DB_URL=... DRAMA_HUB_BENCH=1 \
  docker run --rm --network <net> -e DRAMA_HUB_TEST_DB_URL -e DRAMA_HUB_BENCH=1 \
  -v "$PWD:/src:ro" -w /src drama-hub-e2e:local integration \
  -test.run 'TestBenchSegmentPrefetch$' -test.v
```

## 不做

- 不做整库 / 整剧预切：2847 万集不可能预切（`docs/L1-CORPUS-SCOPE.md`）。
- 不做周期巡检 / 常驻预热循环：没人播放时零后台任务。
- 不做服务端 m3u8 缓存、不做 ETag 回源校验（硬约束 9）。
- 不改 m3u8 切片粒度、树形与对外 URL 协议。

## 回退

- 设 `segment_prefetch_blocks=0`：不再生成任何预热分片（已生成的按普通缓存淘汰）。
- 迁移只需保留列（默认 `FALSE`），旧代码忽略它也能工作。

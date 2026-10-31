# ADR-0008：多副本的共享状态——只在 PG 上共享「必须单写」的那一格

状态：**已提议（待 lxz 批准）**（2026-10-31，MKACG-619）。

上级口径（lxz 2026-10-31）：「单容器负载过大时能否多副本，用 Redis 共享状态」。
本 ADR 的答案是：**能多副本，但不引入 Redis**——Redis 的每一项候选职责在 PG 上
都有一条更短的实现，而它带来的第二个有状态中间件与第二个故障域，换不到本项目
需要的任何能力。

范围：只出设计，不改代码（MKACG-619 的边界）。参考实现口径（键设计、故障矩阵、
容量推算、运维清单）在 `docs/MULTI-REPLICA.md`。

前置：**MKACG-618（PG）必须先在当前版本落地**。今天 `main @ 0ea9520` 仍是 SQLite
单文件（`internal/store/db.go`），`settings` / `artifact` / `source_state` / `segment`
都在这一个文件里，跨机不可共享——PG 之前任何多副本方案都不成立。

## 背景

### 单实例假设今天散在六处（逐处有出处）

| # | 假设 | 出处 | 多副本下会怎样 |
|---|---|---|---|
| 1 | 源级单飞是进程内锁 | `internal/scheduler/scheduler.go:61`（`locks map[string]*sync.Mutex`）、`:251`（`TryLock`） | N 个副本同时刷同一源：上游 `/player` 的 444 限流被 N 倍触发，游标互相覆盖 |
| 2 | 分片 single-flight 是进程内表 | `internal/cache/cache.go:100`（`flights map[string]*flight`） | 同一分片被 N 个副本各切一次（多花 ffmpeg，数据不错） |
| 3 | 缓存文件在本机盘 | `cache_dir`（settings）＋`internal/cache/layout.go:42` | 两个副本的 `<cache_dir>/seg/…` 不在同一台机器 |
| 4 | `artifact.path` 是拼了本机根目录的路径 | 写入点 `internal/cache/cache.go:188`、`internal/cache/cover.go:91` | **最危险的一条**：行在共享库里、路径指向 A 机器的文件。B 读到该行 → 文件不存在 → 走 `hit()` 自愈分支删行（`internal/cache/cache.go:147`）→ A 的缓存行被 B 删掉，两边反复互删、反复重切 |
| 5 | `/healthz` 只证明进程能接请求 | `cmd/drama-hub/main.go:238` | LB 会把流量打进「PG 连不上 / 缓存目录不可写」的副本 |
| 6 | 文档口径写着单实例 | `docs/SCHEDULER-MODEL.md`「2.0 明确不做：分布式调度 / 多实例」；`docs/CACHE-MODEL.md`「不做跨进程锁（单实例）」 | 多副本落地时这两句必须改写，否则文档与代码打架 |

第 4 条不是「以后会出问题」，而是**一旦多副本就立刻出问题**：今天的 `artifact`
主键是 `(kind, key)`，文件却是每台机器一份。本 ADR 的核心就是让**索引的作用域
跟着文件的作用域走**。

### 多副本能分担什么、不能分担什么

先钉死这条口径，否则「多副本」会被当成「同步提速」——上线后发现 30.5 小时的首次
全量灌库一点没变快。

| 负载 | 单副本现状 | 多副本能分担？ | 原因 |
|---|---|---|---|
| 播放切片（ffmpeg / ffprobe） | 每进程 `min(4, max(1, NumCPU/2))` 并发（`internal/segment/cutter.go:35`） | **能**，最直接的收益 | CPU 在本地，请求可分散 |
| WebDAV 列目录 / `/video/{id}.m3u8` 渲染 | 单进程 CPU ＋ 每请求数次 PG 查询 | **能** | 无状态读 |
| 出流字节 / Range | 单进程 | **能** | 无状态读 |
| 全量同步（灌库） | 单源单飞，一轮最长 900 s | **不能** | 瓶颈是上游礼貌度（444），多副本只会更容易触发；要提速只能分源，本版不做 |
| 缓存淘汰 | 每 5 min 一轮，单 goroutine | 不需要 | 量小 |

结论：多副本的目标是**播放面与元数据面的水平扩展**，不是同步提速。

## 决定

### D1 不引入 Redis。跨副本共享状态全部落在 PG（唯一真值源）

逐项对比「Redis 做什么 / PG 能不能做 / 谁更省」。每一行都正面回答「为什么不是
PG 就能做」。

| 候选职责 | Redis 做法 | PG 做法 | 选谁 | 理由 |
|---|---|---|---|---|
| 源级单飞锁 | `SET NX PX` ＋ 续租 | `lease` 表 ＋ 条件 `UPDATE` / `INSERT … ON CONFLICT` | **PG** | 锁的持有者、到期时间、fencing 与业务写回同一事务域；Redis 做锁则「锁在 Redis、进度在 PG」，两处不一致时没有仲裁者 |
| 分片生成锁 | 每 key 一个 `SET NX PX` | **不做**（见 D3） | 都不用 | 分片是内容寻址＋原子 rename 的幂等产物；锁只能省一次 ffmpeg，省不掉任何一致性工作 |
| segment「生成中」状态 | key 存在性 ＋ TTL | **不做** | 都不用 | 生成是本地纯计算，`artifact` 行只记完成态；跨副本「生成中」需要 TTL 与陈旧状态清理，换一次 ffmpeg |
| 热 playlist 缓存 | `GET/SET m3u8` | playlist 是 `segment` 表纯函数，单次 < 1 ms（`docs/CACHE-MODEL.md`） | 都不用 | 给 1 ms 的纯函数加外部缓存 = 给同一份真值造第二份 |
| `last_used_at` 高频写 | `INCR` 聚合后批量刷 | 每命中一次 `UPDATE`（现状 `internal/cache/cache.go:158`） | **PG** | 单用户播放量级每秒个位数请求；一次主键 `UPDATE` 比多一个中间件便宜 |
| 任务队列领取 | `RPOP` / Streams | `SELECT … FOR UPDATE SKIP LOCKED` | **PG**（本版不需要） | 分片生成是请求驱动、不是后台任务；本版没有需要领队列的活 |
| 主动失效 / 通知 | Pub/Sub | 无（60 s 轮询已够，`docs/SCHEDULER-MODEL.md`「间隔热读」） | 都不用 | 「配置 60 s 内生效」已被接受 |

Redis 的真实优势只有两条：亚毫秒延迟与原生 TTL 过期。本项目的锁竞争是「一个源、
≥300 s 一次」，延迟优势无处发挥；TTL 过期在 PG 里是一列 `expires_at` 加一条
`WHERE expires_at < now()`。

**再评估条件（不是「永不」）**：出现下列任一证据再重开本决定——

1. 副本数 > 5 且租约表成为可测量的热点（`pg_stat_statements` 里 `lease` 相关语句进 Top 3）；
2. 需要跨副本主动失效（共享存储下要求秒级缓存一致）；
3. 需要 > 1000 QPS 的分布式计数 / 限流。

### D2 跨副本共享面收敛到**一类机制**：PG 租约

D1 的推论，也是本 ADR 最省的一条：共享状态越少，多副本的失效模式越少。默认拓扑（T2）
只有 `source:<源>` 一个租约 name；共享存储拓扑（T1）多一个 `cache:evict`。**分片生成、
索引建立、playlist、生成中状态一概不共享。** 逐项结论（状态归属、判断依据、丢了的后果）见
`docs/MULTI-REPLICA.md`「状态归属」。

### D3 分片生成与关键帧索引建立**不加跨副本锁**，用幂等替代

- **分片**：键 `sha256(uid | seq | codec_sig)` 只由全局真值（`episode.id` /
  `merge_group.id`、`segment` 表、`ffprobe` 结果）决定；落盘名即键，写盘是
  `tmp/` ＋ `rename`（`internal/cache/layout.go:70`）。两个副本同时生成同一片：
  两份相同内容、两次原子 rename、后到者生效，读到的永远是完整文件。
  → **正确性无损，代价是一次多余 ffmpeg（约 300–600 ms）＋ 一次多余上游读取。**
- **索引**：`ReplaceSegments` 已是「事务内整体替换」。两个副本同时 `ffprobe` 同一集时，
  后提交者的 `INSERT` 撞 `UNIQUE(episode_id, start_sec)`（`internal/store/migrations/0001_init.sql:44`）。
  **MUST** 把这类冲突映射成 `503 + Retry-After`（可重试），不是 500。索引内容是
  `ffprobe` 输出的纯函数，两次结果相同，冲突只发生在并发窗口内，重试即成功。
- 被否决：为每个 `uid` / 每个分片键各做一把跨副本锁。它把「N 次多余计算」换成
  「N 次锁往返 ＋ 租约续期 ＋ 锁泄漏清理」，而正确性换不到（幂等本来就不需要锁）。

### D4 缓存作用域 = 缓存文件的真实作用域：`artifact` 加 `owner`，`path` 改存相对 `cache_dir`

修「背景」第 4 条那个真实缺陷的最小改法，也是两套拓扑共用一套 schema 的关键。

```sql
-- 迁移 0005（PG 版；编号按 S13 落地后的 runner 规则续号）
ALTER TABLE artifact ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE artifact DROP CONSTRAINT artifact_pkey;
ALTER TABLE artifact ADD CONSTRAINT artifact_pkey PRIMARY KEY (kind, key, owner);
DROP INDEX idx_artifact_kind_last_used;
CREATE INDEX idx_artifact_kind_owner_last_used ON artifact (kind, owner, last_used_at);
```

- `owner = ''` 表示**全局**：缓存文件在共享存储上（拓扑 T1），所有副本看到同一份。
- `owner = <instance_id>` 表示**副本私有**：文件在本机盘（拓扑 T2，默认）。
- `path` 存 `seg/ab/abcd….ts` 这样的**相对**路径，读时 `filepath.Join(cache_dir, path)`。
  今天存的是拼了 `cache_dir` 的路径（`internal/cache/layout.go:53`）→ 换 `cache_dir`
  或换挂载点后行里的路径立刻失效；相对路径把「目录在哪」变成运行时定位，而不是数据身份。
- 语义随之明确：`cache_max_mb` 从「全局上限」变为**每个 owner 一份上限**
  （T2 下 = 每副本）。这条要进 `docs/DEV-STANDARDS.md` 的配置表。
- `cache_scope` 本身**走 env**（`DRAMA_HUB_CACHE_SCOPE`），不进 `settings`：它和 `instance_id`
  一样是部署身份——必须整集群一起改，而 `settings` 天生会被「只重启了一个副本」抢先读到新值，
  造成「一部分行 `owner=''`、一部分 `owner=<id>`」的混用。理由同 D6。

被否决：只加 `owner` 不改 `path`（换目录 / 换挂载点仍要搬迁行）；只改 `path` 不加
`owner`（本地盘多副本仍然互删行，缺陷没修）。

### D5 调度循环每个副本都跑，靠租约互相排斥；**不选主**

- 每副本都跑 `scheduler.Run`：多副本时「谁跑下一轮」不需要事先决定——租约先到先得，
  副本挂了下一次 poll（≤ 60 s）别人自然接上。少一个 leader 选举机制、少一个调度开关。
- 一轮的开始是抢租约（`lease` 表，`expires_at = now() + refresh_timeout_sec + 60s`）；
  一轮的结束是**带 fencing 的条件写回**：

```sql
INSERT INTO source_state (name, cursor, last_run_at, last_ok_at, last_error)
VALUES ($5, $1, $2, $3, $4)
ON CONFLICT (name) DO UPDATE
   SET cursor = excluded.cursor, last_run_at = excluded.last_run_at,
       last_ok_at = excluded.last_ok_at, last_error = excluded.last_error
 WHERE EXISTS (SELECT 1 FROM lease
                WHERE name = $5 AND holder = $6 AND expires_at > now())
RETURNING name;
```

  `sql.ErrNoRows` → 租约已被夺走 → **本轮立即中止，不写游标、不写 `last_run_at`**，日志记
  `reason=lease_lost`。用 `INSERT … ON CONFLICT … WHERE … RETURNING` 而不是裸 `UPDATE`：
  它顺手覆盖「`source_state` 还没有这一行」的情形，不必再区分「行不存在」与「租约丢了」。
  把「我是否仍持有锁」压进一条语句，不需要额外往返，也就不需要给 `source_state` 加列。
- **时间基准用 PG 的 `now()`，不用副本本地时钟**（`lease` 的所有比较都在 SQL 里做）。
  副本时钟漂移 ±几分钟在本设计里完全无害。
- **不做续租**：一轮本来就被 `refresh_timeout_sec`（默认 900 s）的 ctx 卡死，租约 TTL 取
  `timeout + 60 s` 就够，不会出现「租约过期而轮还在跑」。代价：副本在轮中间被 kill 时，
  该源最多空等 `timeout + 60 s + poll`（≈ 17 min）才有下一个副本接手——对「30 小时起步的
  首次全量」这个量级可接受。
- 被否决的替代：
  - **PG advisory lock（`pg_try_advisory_lock`）**。会话级锁在连接断开时自动释放，确实不留
    陈旧锁；但它要求整轮（最长 900 s）独占一条连接且不能被连接池回收，持有者只能翻
    `pg_locks` 看，也无法把「是否仍持有」写进业务 SQL 做 fencing。租约表多约 20 行 SQL，
    换来可观测、可 fencing、与写回同一条语句 → 选租约表。advisory lock 留给「短事务互斥」，
    本版没有这种需求。
  - **只让一个副本跑调度**（env 开关或选主）。多一个配置项、多一种「开关忘了改」的故障
    形态；租约已经解决同一件事。

### D6 副本身份走 env，**不进 settings**

`instance_id` 必须每副本不同，而 `settings` 是全局共享的一张表（PG 之后天然共享）——
把副本身份写进 settings 必然互相覆盖。**MUST** 用 `DRAMA_HUB_INSTANCE_ID`（缺省取
hostname）。要求：同一副本重启后 id 不变，否则旧行的 `owner` 成为孤儿（不丢数据，
只占着 `SUM(bytes)`，由 `cache_lru_days` 老化清掉）。

### D7 锁 / 状态不可用时**拒绝新生成**，而不是所有副本都去生成（fail-closed）

租约表不可达（PG 抖动）时：同步轮放弃（记 warn）；播放面未缓存分片返回
`503 + Retry-After`。命中判断要读 `artifact` 行（在 PG 里），PG 不可达时连都命中不了
→ 播放面整体 503。这是**接受**的后果（见 D8 与故障矩阵），不做「无 DB 也能播」的降级：
那需要本地索引，等于给缓存造第二份真值。

### D8 健康检查拆成存活 / 就绪，新增 `/readyz`

`/healthz` 保持现状（只证明进程能接请求，`cmd/drama-hub/main.go:238`），供容器 liveness。
新增 `GET /readyz`：PG `Ping`（1 s 超时）＋ `cache_dir` 可写（T1 下再加共享存储可写）
→ 200，任一失败 → 503，供 LB / 编排的 readiness。**不做**「就绪失败就退出进程」——
一次 PG 抖动会连锁重启整个集群。

## 拓扑：两套，一套代码，由 env `DRAMA_HUB_CACHE_SCOPE` 选择

| | **T2 副本亲和本地缓存**（默认，`cache_scope=per-replica`） | **T1 共享存储**（`cache_scope=shared`） |
|---|---|---|
| 缓存文件 | 每副本自己的盘 | 一份 NFS/JuiceFS 挂到所有副本同一路径 |
| `artifact.owner` | `<instance_id>` | `''` |
| 跨副本共享的状态 | 只有源级租约 | 源级租约 ＋ 淘汰租约 |
| 命中率 | 每副本各自算；同一集被 N 个副本各缓存一份 → 占用最坏 N× | 全局一份 → 命中率最高、容量最省 |
| 上游 / CDN 压力 | 同一分片最坏被切 N 次 | 1 次 |
| 新增基建 | 无 | NFS / 对象存储网关 ＋ 挂载管理 ＋ 一条新故障域 |
| 单点故障 | 无（一副本盘坏只影响它） | 共享存储挂 → 所有副本新分片生成失败 |
| 读延迟 | 本地盘（最快） | 网络文件系统（多一跳；百 KB 级分片仍远快于现切） |
| 适合 | 2–3 副本、同机 / 同机架、盘够 | 副本多、盘紧、或要求「一份缓存」 |

**默认取 T2**：不需要任何新基建，故障半径最小，且「幂等替代锁」已经把这套拓扑的
共享面压到只剩一类锁。T1 的价值在副本数上来（4+）之后才体现，届时只改一个 settings
值 ＋ 挂一次盘。

## 被否决的方案（汇总）

| 方案 | 否决理由 |
|---|---|
| Redis 承担锁 ＋ 状态 | D1 的对比表：每项 PG 都有更短实现；多一个中间件、多一个故障域、多一条降级路径 |
| PG advisory lock 做源锁 | D5：900 s 独占连接、不可观测、无法 fencing；租约表约 20 行 SQL 就够 |
| 跨副本分片生成锁 | D3：幂等已保证正确性，锁只省一次 ffmpeg，却引入锁往返与泄漏清理 |
| 持久化「生成中」状态 | D3：同上，且要 TTL 与陈旧清理 |
| 全局共享 `artifact` 行 ＋ 各副本本地盘（今天的 schema） | 背景第 4 条：B 删 A 的行、互删互切。这是**必须修**的缺陷，不是可选优化 |
| 缓存全上对象存储 ＋ 无状态化改造 | 要重写 `internal/cache` 读路径（S3 Range 语义、网关、本地二级缓存），并让 ffmpeg 从对象存储取源；收益只有「缓存池统一」，而 T1 的挂载方案已经给了同一个收益 |
| 选主（只有一个副本跑调度 / 淘汰） | D5：多一个配置项与一种故障形态；租约先到先得等价 |
| PG 不可用时的「只读播放」降级 | D7：命中判断要读 `artifact` 行，降级需要本地索引 = 第二份真值；按硬约束 9 不为极低概率场景加固 |

## 后果

- 多副本的**同步速度不变**（单源单飞 ＋ 上游礼貌度是瓶颈）；变快的是播放与元数据面。
- 缓存占用最坏 N×（T2）；`cache_max_mb` 语义变成「每个 owner（副本）一份」。
- 首次播放同一集的同一片可能被多个副本各切一次；CDN 侧最坏 N 倍读取。
- 滚动升级期间副本可能跑不同镜像 → 同一 `codec_sig` 下 ffmpeg 版本不同 → 同一键的字节
  可能略有差异（rename 原子，读到的是完整文件之一，不影响播放）。因此 **MUST** 所有副本
  以同一镜像 tag 运行；滚动升级期接受短暂混版。
- PG 成为一等依赖：PG 挂 = 播放、WebDAV、管理面全部 503。这是「唯一真值源」的代价，
  写进故障矩阵。
- 两处文档口径要改：`docs/SCHEDULER-MODEL.md`「不做多实例」、`docs/CACHE-MODEL.md`
  「不做跨进程锁（单实例）」。
- 宪法级改写（**需 lxz 批准**）：
  - `AGENTS.md` 硬约束 4 的「单二进制 ＋ SQLite」在 S13（PG）已需改写；本 ADR 追加
    「同一镜像可多副本，缓存默认副本私有」。其中「**没有外部缓存服务**」**仍然成立**
    ——因为我们不引入 Redis。
  - `docs/SCHEDULER-MODEL.md`「2.0 明确不做：分布式调度 / 多实例」→ 改为「多副本靠
    PG 租约单飞，见 ADR-0008」。

## 需要同步修改的文档（实现落地时一起改；本 ADR 只定方向）

| 文件 | 要改的段落 |
|---|---|
| `docs/MULTI-REPLICA.md` | 新增：本 ADR 的参考实现口径（前置条件、键设计、故障矩阵、容量、运维） |
| `docs/CACHE-MODEL.md` | 「并发」表加跨副本口径；「淘汰」加 owner 分域；`artifact` 键与路径语义 |
| `docs/SCHEDULER-MODEL.md` | 「并发」「单飞」「2.0 明确不做」三处按 D5 改写 |
| `docs/DATA-MODEL.md` | 新增迁移 `0005`（`artifact.owner` / 主键 / 索引 / 相对路径） |
| `docs/DEV-STANDARDS.md` | 配置表：`cache_max_mb` 语义改为每 owner、`cache_dir` 加「每副本独立卷」约束、`stream_secret` 加「全副本一致」约束；env 引导表新增 `DRAMA_HUB_INSTANCE_ID` / `DRAMA_HUB_CACHE_SCOPE` |
| `README.md` | 「文档」索引加 `docs/MULTI-REPLICA.md`；部署一节加多副本前提 |
| `AGENTS.md` | 硬约束 4 与目录说明（待 lxz 批准） |

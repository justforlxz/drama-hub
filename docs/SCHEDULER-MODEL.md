# 调度模型

状态：定稿（2026-10-30，MKACG-604 / stage 1）
前置：`docs/PLUGIN-CONTRACT.md`（插件契约）、`docs/L1-CORPUS-SCOPE.md`（全量有多大）

## 一句话

> **一个进程内单例调度器，按源跑「轮」；一轮 = 增量枚举 + 新剧灌库 + 存量轮转一批；
> 间隔、批量、超时全部从 `settings` 现读，改完下一轮即按新值算，不重启。**

## 为什么必须分成「枚举」与「轮转」两件事

实测（`docs/L1-CORPUS-SCOPE.md` 取证 E）：全量首次灌库 = 608,372 部 × 1 请求 ≈ **38 小时**
（4 并发）；而 sitemap 增量枚举只有 **30 个请求 ≈ 30 秒**。

如果把「刷一遍全部剧」当成一个周期动作，1 小时的间隔永远不可能被满足（一轮 38 小时），
「间隔可配」就变成一句空话。所以一轮拆成三段，各有各的量级：

| 段 | 做什么 | 一次的上游成本 | 每轮都跑 |
|---|---|---|---|
| 1. 增量枚举 | 从 `source_state.cursor` 起推进 `Enumerate`，得到源侧全部作品标识 | 红果 ≈ 30 请求 | 是 |
| 2. 新剧灌库 | 标识在库里不存在的，逐部 `Work` 并入库 | 新增部数 × 1 请求 | 是（首次就是全量 38 h） |
| 3. 存量轮转 | `sync_at` 最旧的 `refresh_batch` 部，逐部 `Work` 并 upsert | `refresh_batch` × 1 请求 | 是 |

- 首次全量不特殊：它就是「第 2 段一次有 60 万新增」，因此会连跑几十小时。
  一轮跑超了 `next_due`，下一轮立刻开始（见「下次时间」），这是有意的。
- 稳态下每轮 ≈ 30 s 枚举 + `refresh_batch` 部刷新（默认 1000 部 ≈ 4 min），
  远小于 1 小时的间隔，剩下时间在等。
- 存量轮转的全库周期 = `ceil(剧数 / refresh_batch) × 间隔`。
  默认值下 = `ceil(608372/1000) × 1h ≈ 25 天`。这个公式写进状态页，用户能自己算。

## 配置

全部走 `settings`（硬约束 5）。`<源>` = `Plugin.Name()`。

| key | 类型 | 默认 | 取值范围 | 作用 |
|---|---|---|---|---|
| `refresh_interval_sec` | int | `3600` | 300–86400 | 全局刷新间隔 |
| `plugin.<源>.refresh_interval_sec` | int | 空 = 继承全局 | 300–86400 | 源级覆盖 |
| `refresh_batch` | int | `1000` | 0–10000 | 每轮存量刷新的部数；`0` = 关闭存量轮转（只剩枚举 + 新增 + 手动） |
| `refresh_timeout_sec` | int | `900` | 60–86400 | 单轮预算（`context.WithTimeout`） |
| `plugin.<源>.request_timeout_sec` | int | `30` | 5–300 | 单次出站请求超时（插件用） |
| `plugin.<源>.enabled` | bool | 内置源 `true` | — | 是否参与调度 |

### 粒度：全局 + 按源，**不做按剧**

- **全局**：所有源共用的默认间隔。
- **按源**：`plugin.<源>.refresh_interval_sec` 覆盖全局（第二个源想 15 分钟刷一次就写这一个键）。
- **按剧：MUST NOT。** 理由：60 万部剧各带一个计划，等于把调度表变成 60 万行的状态机，
  而它能表达的需求（「这部剧更急」）已被「手动触发 + `sync_at` 最旧优先」覆盖。
  加它只增加状态与测试面，不增加能力。

### 间隔范围为什么是 300–86400

- 下限 300 s：一轮的固定成本（枚举 30 请求 + 至少 1 部刷新）约 1 分钟，
  再密就是在打上游，没有意义。
- 上限 86400 s：一天一轮，再长等于「关了自动刷新还假装开着」。
- 越界不静默纠正：`Settings.Set` 返回 `ErrInvalidSetting` → HTTP 400（沿用现有行为）。

## 「间隔从 1h 改成 15m，不重启即生效」的实现路径

选**配置热读**，不选「重建调度器」：

1. `Settings` 现有实现每次读都查库、不缓存（`internal/store/settings.go`），
   所以 `refresh_interval_sec` 的下一次读取一定是新值。**不需要新增任何机制。**
2. 调度循环的定时器等待时间取 `min(下一个到期时间, now + 60s)`。
   60 s 是「配置变更能被多快感知」的上界，也是唯一的轮询：一次 `settings` 主键查询。
3. 每次醒来都重新读一遍 `enabled` / `refresh_interval_sec` / `plugin.<源>.*`，
   然后重算所有源的 `next_due`。改值 → 最多 60 s 后按新间隔执行。

被否决的做法：`Settings.Set` 往 channel 里发通知，调度器 `select` 上它。
代价是 `internal/store` 要持有一个观察者注册表（可变全局状态、要处理 `Set` 与
`Delete` 的一致性、要给测试注入），换来的只是把「60 s 内生效」变成「立刻生效」。
60 s 的延迟对「改刷新间隔」这个场景没有可观察的差别，不值得为它加一层观察者机制。

可验证：`refresh_interval_sec` 设 300，观察连续 3 次执行的 `source_state.last_run_at`
时间戳（间隔 ≈ 300 s）；过程中改成 900，第 4 次起间隔变成 ≈ 900 s，进程未重启。
下限 300 s 就是为了让这条验证在 15 分钟内跑完，不需要为测试放宽取值范围。

## 循环结构

```
Startup（装配根，一次）
  1. 对每个启用的源：SELECT … FROM source_state WHERE name=? ；无行则 INSERT 一行空状态
  2. 计算 next_due（见下）
  3. go scheduler.Loop(ctx)

Loop（单 goroutine）
  for {
      wait = min(最早的 next_due - now, 60s)      // 最小 0
      select { <-time.After(wait) ; <-ctx.Done() → return }
      重新读取所有配置，重算 enabled 集合与各源 next_due
      for 每个 due 的源:
          if !sourceLock.TryLock() { continue }   // 正在跑 → 本轮跳过，记 skipped
          go runRound(source)                     // 结束后 Unlock
  }

runRound(source)                                  // 单飞，见下
  ctx, cancel := context.WithTimeout(ctx, refresh_timeout_sec)
  defer cancel()
  defer sourceLock.Unlock()
  1. 增量枚举：从 DB 读 cursor → Enumerate 循环 → 直到 Next==nil 或达到本轮预算
  2. 新剧灌库：差集里的每部 Work() → 入库（并发见下）
  3. 存量轮转：sync_at 最旧的 refresh_batch 部 → Work() → 入库
  4. 写回 source_state：cursor / last_run_at / last_ok_at / last_error
```

`runRound` 是唯一的写库入口，也是唯一的错误汇聚点。它在自己的 goroutine 里，
panic 必须被 `recover` 成一轮失败（状态页可见），不能拖垮进程。

## 并发

| 层 | 取值 | 可配 |
|---|---|---|
| 源之间 | 并行（每个源一个 round） | — |
| 同一源 | **单飞**：同一源任意时刻至多一个 round | 不可配 |
| 一部剧内部 | 串行（一次 `Work` 就是一次请求） | — |
| 剧与剧之间 | `min(4, max(1, NumCPU/2))` 个 worker | 不可配 |
| SQLite 写入 | 一部剧一个事务；同一时刻最多 4 个写者 | — |

worker 数不复用 `ffmpeg` 那个常量（`internal/segment` 的闸），但取值规则相同
（`min(4, max(1, NumCPU/2))`）：4 并发实测 4.4 req/s，8 并发对上游的收益递减而
403/429 的概率上升（实测上游已经在 `/seo-static.xml` 上偶发 503，见取证 E）。
**不做成配置项**：没有用户会为「上游礼貌度」调参，而多一个可调项就多一条要测的边界。

`busy_timeout=5000`（`internal/store` DSN 已设）覆盖 4 个写者的争用；
单个事务只写 1 部剧 + 它的集 + 重建该剧的 `emby_item`，最长的是 252 集那部。

## 超时

| 层 | 谁负责 | 值 |
|---|---|---|
| 单次出站请求 | 插件（`http.Client.Timeout`） | `plugin.<源>.request_timeout_sec`，默认 30 s |
| 单次插件调用 | 框架 | 在请求超时之上再包一层 `ctx`，取 `request_timeout_sec + 5s` 兜底 |
| 一轮 | 框架 | `refresh_timeout_sec`，默认 900 s |

超时归 `ErrUpstream`（可重试）。`ctx` 被取消（进程退出、用户手动取消）时直接向上传播，
不写 `last_error`。

**一轮超时不清游标**：游标只在成功推进枚举后写回。一轮被砍掉时，已灌库的剧留下
（各自是完整事务），未跑到的下一轮重来，游标指向最后成功推进的位置。

## 失败隔离

四条，每条都有可观察的结果：

1. **源之间隔离。** 一个源 panic / 超时 / 抛错，其它源的 round 照常。
   实现上每个 round 独立 goroutine + `recover`。
2. **剧之间隔离。** 一部剧失败只跳过这一部，不中止本轮（`ErrSchema` 连续 20 次例外，
   见 `docs/PLUGIN-CONTRACT.md` 错误语义表）。
3. **库不变。** 失败的剧不写库；写入边界是「一部剧一个事务」。
   因此「刷新失败 → 已有数据零删除」是结构保证，不是靠人工小心。
4. **可观察。** `source_state.last_error` 记最近一次失败的文本与时间；
   状态页显示「上次刷新时间 / 下次计划时间 / 上次失败原因」。

失败也推进 `next_due`（按 `last_run_at + interval` 算），不做指数退避：
源挂了的时候，每个间隔试一次是可接受的礼貌度；退避需要额外状态（失败计数、退避时长表），
而它能带来的唯一好处是「少打几次挂掉的上游」，在单源单用户的场景换不到什么。

## 幂等

| 动作 | 幂等身份 | 重跑结果 |
|---|---|---|
| 剧目入库 | `drama(provider, provider_id)` upsert | 计数不变；`sync_at` 被本次刷新时间覆盖（这是刷新的证据，不是副作用） |
| 集入库 | `episode(drama_id, ep_no)` upsert | 计数不变；`vid` 被覆盖为同值 |
| 派生表 | `RebuildEmbyItems(dramaID)` 整体替换 | 逐行相同 |
| 枚举推进 | `source_state.cursor` 单调前进 | 同一游标起点重跑得到同一批新增（差集运算本身幂等） |
| 手动触发 | 单飞锁 | 撞车时第二次返回 409，不重复执行 |

**刷新只增改，不删。** 源里消失的剧/集不会被删除（`ErrNotFound` 只记日志），
理由与后果见 `docs/DATA-MODEL.md`「增删改语义」。删剧只走管理 API 的显式删除。

## 手动触发

| 端点 | 方法 | 语义 | 状态码 |
|---|---|---|---|
| `/api/refresh` | POST `{"source":"web"}` | 立即跑一轮该源，不等 `next_due` | 202 已启动 / 404 无此源或未启用 / 409 该源正在跑 |
| `/api/dramas` | POST `{"series_id":…}` | 保留为单剧补录入口（1.0 已有），走同一套 `Work` + 入库 | 201 / 502（不变） |
| `/api/status` | GET | 扩展为含 `sources[]`：`name` / `enabled` / `running` / `last_run_at` / `last_ok_at` / `next_due_at`（现算）/ `last_error` | 200 |

手动触发与定时触发**共用同一把单飞锁**：先拿到锁的跑，另一个拿到 409 或被本轮跳过。
这就是「同一源同一时刻不并发跑两次」的全部实现——没有队列、没有优先级、没有重入。

## 下次时间（重启可算）

```
next_due = last_run_at + interval          // last_run_at 在库里，interval 现读 settings
          若 last_run_at 为空 → next_due = now（首次启动立刻跑一轮）
```

库里**不存** `next_due`：它每次用到时从 `last_run_at + 当前 interval` 现算。
改间隔后重启、或改间隔后不重启（60 s 内重算），结果一致。
存一份 `next_due_at` 等于给同一个数造第二个真值，还要额外约定「不一致时听谁的」——
不存就没有这个问题。状态页与 `/api/status` 的 `next_due_at` 字段是**请求时算出来的**，
不是从库里读的。

## 2.0 明确不做

- cron 表达式、按星期/按天的时间表：一个整数间隔覆盖全部需求。
- 分布式调度 / 多实例：单二进制单实例（硬约束 4）。
- 优先级队列、按剧间隔、动态背压：见「粒度」。
- 失败重试队列：失败由下一轮自然重试。

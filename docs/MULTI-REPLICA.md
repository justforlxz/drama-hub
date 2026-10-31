# 多副本部署与共享状态

状态：定稿（2026-10-31 / MKACG-619）。本文件是 `docs/adr/0008-multi-replica-state-sharing.md`
的参考实现口径：前置条件、状态归属、键设计、配置、故障矩阵、容量推算、运维清单、测试要求。
**决策与理由在 ADR-0008，本文不复述。**

前置：MKACG-618（PG）先落地。今天 `main @ 0ea9520` 是 SQLite 单文件，跨机不可共享。

## 一句话

> **默认拓扑下多副本只共享一类状态：源级单飞锁（T1 额外多一把淘汰租约）。缓存文件留在各副本自己的盘上，
> 索引的作用域跟着文件的作用域走（`artifact.owner`），跨副本的一致性靠幂等而不是靠锁。**

## 范围

| 负载 | 多副本能分担 | 说明 |
|---|---|---|
| 播放切片（ffmpeg / ffprobe）、出流字节 | **能** | CPU 与带宽在本地，请求可分散 |
| WebDAV 列目录、`/video/{id}.m3u8` 渲染、管理面 | **能** | 无状态读 |
| 全量同步 | **不能** | 单源单飞，瓶颈是上游礼貌度（`/player` 444）；多副本只会更容易触发限流。要提速只能分源，本版不做 |
| 缓存淘汰 | 不需要 | 量小 |

**多副本不是同步提速手段。** 首次全量灌库仍是约 30.5 小时、按轮跑、被打断是常态
（`docs/SCHEDULER-MODEL.md`「全量灌库的真实成本」）。

## 前置条件清单（必须先落地）

| # | 条件 | 现状 | 验证方式 |
|---|---|---|---|
| 1 | **PG 落地**（MKACG-618）：`settings` / `artifact` / `source_state` / `segment` 共库 | 未落地（仍 SQLite） | 两个进程连同一 `DRAMA_HUB_DB_URL`，A 写 `settings` 一行 → B 读到同一行 |
| 2 | **必填 secret 有注入入口**（MKACG-616）：`stream_secret` / `webdav_user` / `webdav_pass` 由 env 引导写入共享库 | 未落地（`settings.Set` 全仓无调用方，空卷 `/webdav` 恒 401） | A 渲染出的 `.strm` 里的 `{id}`，拿 B 的地址请求 `/video/{id}.m3u8` → 200（说明两副本 secret 一致） |
| 3 | **`strm_prefix` 指向 LB / 对外地址** | 需配置 | `.strm` 内容里的主机是 LB，不是某个副本名 |
| 4 | **镜像按 tag 发布**（MKACG-615） | 未落地 | 两副本 `podman inspect` 的 image digest 相同 |
| 5 | **`/readyz` 就绪探针**（ADR-0008 D8） | 只有 `/healthz`（硬编码 ok） | 停 PG → `/readyz` 503、`/healthz` 200 |
| 6 | **`DRAMA_HUB_INSTANCE_ID` / `DRAMA_HUB_CACHE_SCOPE` 两个 env** | 不存在 | 两副本日志里 instance_id 不同；重启后不变 |
| 7 | **迁移 `0005`：`artifact.owner` + 相对路径 + `lease` 表** | 不存在 | 空库 → 最新；重复执行幂等；两副本写不同 owner 的行互不可见 |
| 8 | **连接池上限算过**：`副本数 × max_open_conns ≤ 80% × PG max_connections` | S13 定义 | `SELECT count(*) FROM pg_stat_activity` 在 N 副本满载时留 20% 余量 |
| 9 | **`cache_dir` 每副本独立卷**（运维） | — | `podman inspect` 显示两副本的该路径来自不同 volume；T2 下**禁止**把同一共享目录给两个副本 |
| 10 | **淘汰单写者**（T1）或**分域**（T2） | 现在是单实例无锁淘汰 | 见「测试要求」第 9 条 |

## 拓扑

### T2 副本亲和本地缓存（默认，`cache_scope=per-replica`）

```
                ┌──────────── LB / 反向代理 ────────────┐
                │  /webdav  /*  /video/{id}/*            │
                └───────┬───────────────────┬───────────┘
                        │                   │
             ┌──────────▼──────┐   ┌────────▼────────┐
             │ 副本 A           │   │ 副本 B           │
             │ cache_dir=A 盘   │   │ cache_dir=B 盘   │   ← 文件不共享
             │ ffmpeg 闸 4      │   │ ffmpeg 闸 4      │
             │ owner=A          │   │ owner=B          │
             └──────┬──────────┘   └───────┬─────────┘
                    │                      │
                    └──────────┬───────────┘
                               ▼
                  ┌────────────────────────┐
                  │ PG（唯一真值源）        │
                  │ settings / drama /     │
                  │ episode / segment /    │
                  │ artifact(owner) /      │
                  │ source_state / lease   │
                  └────────────────────────┘
```

- 跨副本共享的状态：**只有 `lease` 里的一行源级租约**。
- 每个副本自己切、自己存、自己淘汰；同一个分片两个副本各存一份（内容相同，键相同）。
- 一个副本的盘挂掉只影响它自己；LB 摘掉它即可继续服务。
- 不需要会话粘性。若要减少重复切片，可在 LB 上按 URL 做一致性哈希，属运维可选优化，
  本版不实现。

### T1 共享存储（`cache_scope=shared`）

```
        LB ──┬──────────────┬──────────────┐
             ▼              ▼              ▼
        副本 A          副本 B          副本 C
             └──────────────┼──────────────┘
                            ▼
                  ┌──────────────────┐        ┌──────────────┐
                  │ NFS / JuiceFS     │        │ PG           │
                  │ <cache_dir> 同一份 │        │ + lease      │
                  └──────────────────┘        └──────────────┘
```

- `artifact.owner = ''`：索引行全局共享，路径相对 `cache_dir`，所有副本解析到同一文件。
- 跨副本共享的状态：源级租约 ＋ 淘汰租约（`name='cache:evict'`，单写者）。
- 收益：命中率最高、容量最省、上游只切一次。代价：多一条故障域（共享存储挂 = 新分片生成失败）。

拓扑成本与失效对比见 ADR-0008「拓扑」表。

## 状态归属

### 判断依据（三轴，任一命中即按该列处理）

| 轴 | 问题 | 结论 |
|---|---|---|
| 一致性要求 | 是「谁先谁后决定结果」的写状态吗？ | 是 → PG（并需单写者或幂等） |
| 访问频率 | 每个请求都要读吗？ | 是 → 本地（内存/无锁）；写路径上一次主键查询可接受 |
| 故障半径 | 丢了会怎样？ | 只掉性能（可重建）→ 本地；丢正确性/进度且不可恢复 → PG |

### 逐项结论

| 状态 | 归属 | 一致性要求 | 丢了的后果 | 判断依据 |
|---|---|---|---|---|
| 源级单飞锁 | **PG `lease`**（`source:<源>`） | 必须单写 | N 副本同刷同源：上游 444 放大、游标互相覆盖 | 一致性 + 不可重建 |
| 游标 / `last_run_at` / `last_ok_at` / `last_error` | **PG `source_state`** | 单写 + fencing（ADR-0008 D5） | 重复枚举（差集幂等，只浪费上游）或进度回退 | 一致性 + 不可重建 |
| 关键帧索引（`segment` 表） | **PG** | 幂等整体替换 + `UNIQUE(episode_id,start_sec)` | 并发时一次 `503 + Retry-After`，重试即成功 | 一致性（派生数据，但重建成本 0.3–1 s/集） |
| 刷新计划 `next_due` | **不存**（`last_run_at + 现读 interval` 现算） | — | 无 | 现算即一致，存了就是第二份真值 |
| 分片生成锁 | **不共享**（保留进程内 single-flight） | 无需 | 同一分片多切一次（300–600 ms ＋ 一次上游读） | 幂等（内容寻址 + 原子 rename） |
| segment「生成中」状态 | **不存在**（只有完成态的 `segment` 行与 `artifact` 行） | — | 同上的多切一次 | 同上；引入它要 TTL 与陈旧清理 |
| `artifact` 元数据（分片/海报/索引） | **PG，带 `owner`** | 作用域必须等于文件作用域 | 今天：两副本互删行、反复重切（ADR-0008 背景第 4 条） | 一致性（它是文件的清单） |
| 缓存文件（`.ts` / 图 / `.json`） | **本地盘（T2）/ 共享盘（T1）** | 内容寻址、不可变、`tmp` + `rename` | 重切一次 | 故障半径（可重建） |
| 淘汰 | T2：每副本删自己的 `owner`；T1：`cache:evict` 租约单写者 | T1 需单写者 | T1 多写者 → 过度淘汰与抖动（不会坏数据） | 一致性（`SUM(bytes)` 需要唯一结论） |
| `settings`（含 `stream_secret`） | **PG** | 全副本读到同一份 | secret 不一致 → A 发出的 `{id}` 在 B 校验失败（播放全 503） | 一致性 |
| 副本身份 `instance_id` | **env（每副本不同）** | 每副本唯一、重启不变 | 旧行 `owner` 变孤儿（只占容量，按天数老化） | 必须每副本不同 → 不能放共享表 |
| 缓存拓扑 `cache_scope` | **env（整集群同值）** | 整集群一致 | 混用 → 一部分行 `owner=''`、一部分 `owner=<id>` | 部署身份，不是运行参数 |
| ffmpeg 并发闸 | 本地（进程资源） | 无需 | 无 | 故障半径 |
| single-flight 表 | 本地（内存） | 无需 | 无 | 故障半径 |
| m3u8 文本 | **不缓存、不共享**（`segment` 表纯函数） | — | 无 | 访问频率（< 1 ms） |
| 日志 / 指标 | 本地 | 本版不做聚合 | 无 | 故障半径 |

## 键与库表设计

### `lease`（新增，迁移 `0005`）

一个通用租约表，本版只有两种 `name`：`source:<源名>` 与 `cache:evict`（仅 T1）。

```sql
CREATE TABLE lease (
    name        TEXT        PRIMARY KEY,
    holder      TEXT        NOT NULL,   -- DRAMA_HUB_INSTANCE_ID
    expires_at  timestamptz NOT NULL,   -- 一律由 PG 的 now() 算，不取副本本地时钟
    acquired_at timestamptz NOT NULL DEFAULT now()
);
```

**抢租约（一条语句，允许接管已过期的租约）：**

```sql
INSERT INTO lease (name, holder, expires_at, acquired_at)
VALUES ($1, $2, now() + $3::interval, now())
ON CONFLICT (name) DO UPDATE
   SET holder      = excluded.holder,
       expires_at  = excluded.expires_at,
       acquired_at = now()
 WHERE lease.expires_at <= now()
RETURNING holder;
```

- 返回一行（`holder` = 自己）→ 抢到。
- `sql.ErrNoRows`（`QueryRow.Scan`）→ 别人持有且未过期 → 本轮跳过，记 `skipped`。

**释放：** `DELETE FROM lease WHERE name = $1 AND holder = $2`。
一轮成功或失败**都必须**显式释放——不释放会让其它副本白等到 TTL
（`refresh_timeout_sec + 60s`，默认 960 s）。进程被 kill 时由过期兜底。

**fencing：** 写回 `source_state` 用一条带守卫的 `INSERT … ON CONFLICT DO UPDATE … WHERE EXISTS
(SELECT 1 FROM lease WHERE name = $5 AND holder = $6 AND expires_at > now()) RETURNING name`
（ADR-0008 D5）。`sql.ErrNoRows` → 中止本轮，不写任何字段。

### `artifact`（改造，迁移 `0005`）

```sql
ALTER TABLE artifact ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE artifact DROP CONSTRAINT artifact_pkey;
ALTER TABLE artifact ADD CONSTRAINT artifact_pkey PRIMARY KEY (kind, key, owner);
DROP INDEX idx_artifact_kind_last_used;
CREATE INDEX idx_artifact_kind_owner_last_used ON artifact (kind, owner, last_used_at);
```

- `owner = ''`：全局（T1）。
- `owner = <instance_id>`：副本私有（T2）。
- 读出/写入一律按 `(kind, key, owner)` 三元组；`owner` 由一个构造期注入的取值函数提供
  （`cache_scope` 决定它返回 `''` 还是 `instance_id`），**MUST NOT** 在 `internal/cache`
  里散落 `if scope == …` 判断。
- `path` 改为**相对 `cache_dir`** 的路径（`seg/ab/<hash>.ts`）。读时
  `filepath.Join(cache_dir, path)`。
- `SUM(bytes)` / 淘汰查询按 `owner` 过滤：`cache_max_mb` 是**每个 owner 一份**上限。
- 淘汰只有**一套**代码：作用域由构造期注入的 `owner` 取值函数 ＋ `singleWriter bool` 决定
  （T2：按 `owner` 过滤、不抢淘汰租约；T1：全表、先抢 `cache:evict` 租约）。**MUST NOT**
  在淘汰循环里读 `cache_scope`、也 MUST NOT 写两份淘汰逻辑。
- 校验和冲突：`PutArtifact` 的 `ON CONFLICT (kind, key)` 改为 `(kind, key, owner)`；
  `TouchArtifact` / `DeleteArtifact` / `ListArtifactsOldest` / `ListExpiredArtifacts` /
  `ArtifactByKey` 全部带 `owner`。

> **顺带发现（给实现 issue）**：`artifact(kind='index')` 现在只有测试在写——真实的关键帧索引在
> `segment` 表（`internal/segment/cutter.go` 的 `ensureSegments` 直接 `ReplaceSegments`），
> 而 `docs/CACHE-MODEL.md` 把它写成「缓存在 `artifact(kind='index')`」。多副本下这个 kind 的
> `owner` 语义必然错（索引是全局派生数据）。实现 `0005` 时必须选一条：删掉这个 kind，或把它
> 明确成「本副本私有的派生缓存」。**MUST NOT** 给全局派生数据配 `owner`。

### 分片内容寻址键在跨副本时仍然安全吗（结论：安全）

`SegmentKey = sha256(uid \x1f seq \x1f codec_sig)`，三个输入的来源逐一核对：

| 输入 | 来源 | 跨副本一致？ |
|---|---|---|
| `uid` | `e<episode.id>` / `m<merge_group.id>` | 是。`episode.id` / `merge_group.id` 在 PG 里全局唯一（`emby_item` 是派生表，不参与 uid） |
| `seq` | `segment` 表（按 `start_sec` 升序的片内序） | 是。索引全局一份；`ReplaceSegments` 整体替换，内容由 `planSegments` 纯函数给出 |
| `codec_sig` | `ffprobe` 的 `codec_name/profile/level/pix_fmt/codec_tag_string/time_base` | 是。同源同参数 → 同签名；**不含 ffmpeg 版本**（`internal/segment/cutter.go` 的 `probe`） |

两条约束：

1. **键里 MUST NOT 加副本 id**——加了就把同一份内容在共享存储下分裂成 N 份。
2. 滚动升级期间两副本跑不同镜像 → 同一 `codec_sig` 下 ffmpeg 版本不同 → 同一键的字节
   可能略有差异。`rename` 是原子的，读到的永远是完整文件之一，播放正确性不受影响；
   但**MUST** 以同一镜像 tag 运行，滚动升级期视为可接受的短暂混版。

## 配置项

两个新 env（**部署身份，不是运行参数**，与 `DRAMA_HUB_ADDR` 同类，不进 `settings`）：

| env | 默认 | 取值 | 生效 | 说明 |
|---|---|---|---|---|
| `DRAMA_HUB_INSTANCE_ID` | hostname | 非空、每副本唯一、重启不变 | 重启 | 租约 `holder`；`artifact.owner` 的取值 |
| `DRAMA_HUB_CACHE_SCOPE` | `per-replica` | `per-replica` / `shared` | 重启（**整集群必须同值**） | 决定 `owner` 语义与淘汰模式 |

为什么这两个不进 `settings`：`settings` 的语义是「所有副本共享的可调参数」，
而这两个值必须**每副本不同**（instance_id）或**必须整集群一起改**（cache_scope）。
放进共享表会出现「改了一个值、只重启了一个副本」→ 拓扑混用。env 的改法天然是
「重建容器」，整集群一并生效。

`settings` 里语义有变的既有键：

| key | 变化 | 说明 |
|---|---|---|
| `cache_dir` | 语义不变，约束新增 | T2 下所有副本读到同一个字符串，但**必须映射到各自的卷**；禁止两个副本共用一个物理目录（除非 `cache_scope=shared`） |
| `cache_max_mb` | **语义变更**：全局上限 → 每个 owner 一份 | T1 下仍等于全局（只有一个 owner）；T2 下等于每副本 |
| `cache_lru_days` | 不变 | 过期淘汰按行判断，跨 owner 无害 |
| `stream_secret` | 约束新增：全副本必须一致 | PG 共享后天然一致；不一致的症状是「A 的 strm 在 B 上 401/503」 |
| `refresh_timeout_sec` | 约束新增：决定租约 TTL | `TTL = refresh_timeout_sec + 60s`；改大 → 崩溃后等待变长 |

## 故障矩阵

| 事件 | 用户可见影响 | 系统行为 | 降级动作 | 恢复 |
|---|---|---|---|---|
| **PG 挂**（连不上/超时） | 播放、WebDAV、管理面全部 503；同步停 | 命中判断要读 `artifact` 行 → 逐请求失败；租约抢不到 → 同步轮放弃并 warn；`/readyz` 503 | 不做「无 DB 也能播」的降级（本地索引 = 第二份真值，ADR-0008 D7） | 连接池自动重连，无需重启；下一 poll 恢复调度 |
| **PG 慢**（> 1 s） | 播放首字延迟上升；每个分片请求至少 1 次主键查询 + 1 次 touch | `/readyz` 1 s 超时 → 摘流 | 摘流后由其它副本服务 | 同上 |
| **共享存储挂**（仅 T1） | 新分片 502/503；已缓存分片的读取也失败（读文件报错，无法降级） | 写盘失败 → 不写 `artifact` 行；读失败 → 502 | 切回 T2 需整集群重启（env） | 挂载恢复后自愈，无需重启 |
| **本地盘满/坏**（T2） | 该副本新分片 502/503；其它副本不受影响 | 写盘失败 → 不写行；`/readyz` 写探测失败 → 摘流 | 摘流该副本 | 扩盘或换卷后 `readyz` 自动回 200 |
| **副本在轮中间被 kill** | 该源最多空等 `TTL + poll`（≈ 17 min） | 租约未释放 → 到期后别的副本接手；游标停在最后一次成功推进处 | 无 | 到期自动接管 |
| **副本在切片/写盘中间被 kill** | 下一次请求重切 | `tmp/` 残留半截文件（1 h 后清扫），内容目录里不会出现半截文件（`rename` 原子），`artifact` 无行 | 无 | 自动 |
| **脑裂**（PG 网络分区） | 少数派副本全部请求 503 | 锁与状态都在 PG 单库；连不上 PG 的副本既抢不到租约也写不进状态 → **无法双写同一源**。不存在 Redis Cluster 式双主 | 无 | 网络恢复后自动加入 |
| **租约行被误删 / 误改** | 可能出现两个副本同时跑一轮 | 数据无损（剧目 upsert 幂等、游标单调），只是上游请求翻倍 | 无 | 自然恢复（下一轮重新抢） |
| **副本本地时钟漂移** | 无 | 租约比较全在 SQL 里用 PG 的 `now()` | 无 | — |
| **滚动升级（镜像混版）** | 同一分片的字节可能不同（版本差异） | 各自 `rename` 原子生效；同一键可能被覆盖一次 | 以同一 tag 运行；升级节奏由人控制 | 滚完即一致 |
| **`instance_id` 变更**（改了 env 或换 hostname） | 无 | 旧 `owner` 的行变孤儿：`SUM(bytes)` 仍计入该 owner，按 `cache_lru_days` 老化删除 | 需要立刻回收见「旧缓存与缩容的回收」 | 自动 |
| **Redis 挂** | **不适用**——本版不部署 Redis（ADR-0008 D1） | — | 若将来按 D1 的再评估条件引入：Redis 不可用 → 退化为进程内 single-flight，未缓存分片 `503 + Retry-After`，绝不放任 N 个副本同时回源（fail-closed） | — |

## 容量推算

单副本基线（`docs/CACHE-MODEL.md`「磁盘预算」的实测数字）：

| 量 | 数值 | 依据 |
|---|---|---|
| 一片 `.ts` | ≈ 650 KB | 实测 651,433 B / 10.07 s（`docs/L1-CORPUS-SCOPE.md` 取证 D） |
| 一集（按 3 分钟） | ≈ 12 MB（18 片） | 10 s/片 |
| 一部剧（40 集均值） | ≈ 480 MB（若整部看到底） | `docs/L1-CORPUS-SCOPE.md` 均值 46.8 集 |

多副本推算（N = 副本数，默认 `cache_max_mb = 8192`）：

| 维度 | T2（默认） | T1（共享存储） |
|---|---|---|
| 缓存总盘 | **8 GB × N**（最坏全重复）；单副本仍受 8 GB 约束 → 每副本 ≈ 650 集 ≈ 16 部剧 | **8 GB 一份** |
| 命中率 | 每副本独立；LB 无 sticky 时同一集被 N 个副本各存一份，实际占用视访问分布 | 全局最高 |
| 上游 CDN 读取 | 同一分片最坏 N 次（每次切片一次 range 读） | 1 次 |
| 上游主站（限流对象） | **不放大**：同步单飞，同一时刻只有一个副本在刷源 | 同左 |
| PG 写 | 「每分片请求 1 次 touch `UPDATE`」× 总请求数（不放大，请求本身被分摊） | 同左 |
| PG 读 | 每分片请求 1 次主键查询；每 `m3u8` 请求 1 次 `segment` 查询 | 同左 |
| PG 连接 | `N × max_open_conns`，须 ≤ 80% × PG `max_connections` | 同左 |
| CPU | 每副本 `min(4, max(1, NumCPU/2))` 个 ffmpeg/ffprobe → 集群最坏 `4N` 个 | 同左 |
| 内存 | 每在途分片 2–5 MB（`internal/cache/cache.go` 注释上限；实测片 650 KB）→ 单副本 10 并发 ≈ 20–50 MB | 同左 |
| 调度空转 | 每副本每 60 s 一次 `settings` 读取 + 每源一次 `source_state` 读 + 一次租约抢 | 同左 |

结论：2–3 副本时 T2 的磁盘代价（24 GB）与运维代价（不需要新基建）都低于 T1；
副本数到 4 以上、或盘成为约束时，切 T1 只改一个 env + 挂一次盘。

## 旧缓存与缩容的回收

| 情形 | 会发生什么 | 运维动作 |
|---|---|---|
| 改 `cache_dir` | `path` 是相对路径 → 旧目录不再被任何行引用；命中失败 → `hit()` 自愈删行 → 在新目录重切 | 确认旧目录无进程在用后 `rm -rf` **旧目录**；不做迁移工具 |
| 缩容（删掉一个副本） | 它的 `owner` 行留在 PG，指向已不存在的文件；命中不可能再发生 → 按 `cache_lru_days` 老化删除（文件不存在也删行成功） | 想立刻回收：先删该 owner 的文件，再 `DELETE FROM artifact WHERE owner = '<旧 id>'`（行是文件清单，顺序不能反） |
| `instance_id` 变更 | 同上 | 同上 |
| 切 `cache_scope`（T2 ↔ T1） | 旧 scope 的行对新 scope 不可见，成为孤儿；新 scope 冷启动回源 | 整集群重启；旧行按上一条回收 |

## 测试要求（给实现 issue，必须真跑）

1. **源级互斥**：A 持租约、`runRound` 注入阻塞；B 的 `dispatchDue` 跳过该源；假上游计数只 +1 轮。
2. **fencing**：A 跑一轮中把 `lease.expires_at` 改到过去 → A 的守卫写回返回 `sql.ErrNoRows`、
   日志含 `reason=lease_lost`、`source_state.cursor` 不变。
3. **到期接管**：A 抢到租约后直接杀掉（不释放）→ 把 `expires_at` 推到过去 → B 抢到并完成一轮。
4. **显式释放**：一轮成功结束后 `lease` 无该行 → B 立刻可抢，不必等 TTL。
5. **owner 隔离**（T2）：两副本共用一个 PG、各自 `cache_dir`；A 切好的分片不让 B 命中；
   两边 `artifact` 各一行；杀 A 后 B 仍能服务同一分片（自己切）。
6. **共享路径**（T1）：两副本同一 `cache_dir`、`owner=''` → 同一分片只切一次，第二次命中，行只有一行。
7. **相对路径**：`artifact.path` 不含 `cache_dir`；把 `cache_dir` 指向新目录 → 旧行命中失败 → 自愈删行 → 新目录重切。
8. **`/readyz`**：停 PG → 503、`/healthz` 仍 200；恢复后 readiness 自动 200（进程不重启）。
9. **淘汰分域**：owner=a 超过 `cache_max_mb` → 只删 a 的行与文件，b 的字节数不变。
10. **索引并发**：两个 goroutine 同时 `ensureSegments` 同一集 → 一个成功，另一个得到
    `503 + Retry-After`（冲突映射），最终 `segment` 只有一套行。
11. **时间基准**：把某副本本地时钟调偏 ±10 min，租约抢/放/接管结果不变（全部用 PG `now()`）。

不测：跨集群、对象存储网关、LB 配置、K8s 编排。

## 本版不做

- **不引入 Redis**（含 Sentinel / Cluster）。再评估条件见 ADR-0008 D1。
- 不做 K8s operator / HPA / StatefulSet 编排清单（部署形态由运维决定，本服务只要求 env + 卷）。
- 不做跨集群、跨地域、多活写；不做读写分离。
- 不做同步提速（分源并发调度、双活灌库）。
- 不做无状态化改造（缓存不上对象存储、不重写 `internal/cache` 读路径）。
- 不做按剧/按用户配额、全局热门淘汰、跨副本缓存分发与预取。
- 不做 LB 会话粘性或按 URL 哈希（运维可选，不进代码）。
- 不做 `artifact` 行迁移工具、不做旧 `cache_dir` 自动清理。
- 不做「PG 挂了还能播」的本地索引降级。
- 不做租约续期、公平队列、优先级（争用已是「一个源、≥300 s 一次」的量级）。

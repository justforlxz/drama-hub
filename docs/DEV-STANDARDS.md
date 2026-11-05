# 开发规范

适用于 `drama-hub` 全部 Go 代码。与 `AGENTS.md` 的硬约束冲突时，以 `AGENTS.md` 为准。

## 依赖与技术选型

| 项 | 选择 | 理由 |
|---|---|---|
| Go | `go 1.27`（开发机 go1.27.2） | 与 CI 一致 |
| 模块 | `github.com/justforlxz/drama-hub` | |
| 数据库 | PostgreSQL 16；驱动 `github.com/jackc/pgx/v5` 的 stdlib 适配层 | 单后端、多写者，ADR-0009 |
| WebDAV | `golang.org/x/net/webdav` | 只需要 FileSystem 接口 |
| HTTP | 标准库 `net/http` + 1.22 路由模式 | 不引框架 |
| 日志 | `log/slog`（JSON handler） | |
| 模板/前端 | `html/template` + 原生 JS，`embed` 进二进制 | 无构建步骤 |
| 进程 | `ffmpeg` / `ffprobe` 外部依赖 | 不内嵌二进制 |

**不引入**：ORM、Web 框架、依赖注入容器、前端框架、第二个数据库驱动、任何需要 cgo 的库。

新增任何第三方依赖，先在 `docs/adr/` 写一条 ADR 说明「为什么标准库不够」，否则打回。

## 错误处理

- 所有错误带上下文：`fmt.Errorf("加载剧集 %s: %w", id, err)`。
- 哨兵错误定义在包内并导出：`var ErrNotFound = errors.New(...)`；调用方用 `errors.Is` / `errors.As` 判断，禁止比较错误字符串。
- 业务错误不强求堆栈；跨层返回时只包装一次语义，避免 `"失败: 失败: 失败"`。
- `panic` 只允许出现在 `main` 的启动装配阶段；请求路径里任何 panic 视为缺陷（recover 中间件只做兜底 + 记录，不吞掉不记录）。
- 出站 HTTP 一律 `http.Client{Timeout: ...}` 或 `ctx` 超时；没有超时的出站调用视为缺陷。
- 出站到源侧一律带浏览器身份：UA / `Accept` / `Accept-Language` / `Referer`（`Referer`
  取源站根）。真值只有一份——插件里的常量；`ffmpeg` / `ffprobe` 不走 `http.Client`，
  由装配根用同一份常量拼 `-headers` 注入 `internal/segment`（`Cutter`），不在
  segment 里另写一个 UA。实测与理由见 MKACG-624、`docs/PLUGIN-CONTRACT.md` 硬性要求 8。
- 播放面的出站身份**按通道两份**（MKACG-638）：`ffmpeg` / `ffprobe` 取 App 通道的直链时
  **不带 `Referer`**（媒体 CDN 对带站内 `Referer` 的请求回 403，UA 保留无害），网页通道维持
  UA + 源站根 `Referer`。用哪一份由同一条通道判据挑（本次解析带不带 `Stream.Key`），
  两份都在装配根派生。写「一份身份全通道通用」的代码就是把一条通道打成 502。

### HTTP 边界：错误类别与统一响应

`internal/httpx` 定义**传输层错误类别**，边界包（`web` / `stream` / `webdav`）在返回前用
`fmt.Errorf("%w: 详情", httpx.ErrXxx)` 打标；domain 包保留自己的包内哨兵，**不** import
`internal/httpx`。映射由 `httpx.StatusCode`（`errors.Is` 判定，支持 `%w` 包装，未命中 → 500）：

| 哨兵 | 状态码 | 响应体 `code` |
|---|---|---|
| `httpx.ErrNotFound` | 404 | `not_found` |
| `httpx.ErrInvalid` | 400 | `invalid` |
| `httpx.ErrUnauthorized` | 401 | `unauthorized` |
| `httpx.ErrUpstream` | 502 | `upstream` |
| `httpx.ErrUnavailable` | 503 | `unavailable` |
| 其它错误 | 500 | `internal` |

边界 handler 用 `httpx.WriteError(w, err)` 统一写出：`Content-Type:
application/json; charset=utf-8`，body 固定为 `{"code":"<kind>","message":"<err.Error()>"}`。
`WriteError` **不写日志**（每请求一条访问日志归 `httpx.RequestLogger`，否则失败请求会出现
两条日志），错误详情只进响应体 —— 因此**错误文本里不得带 token / 密码**。

## 上下文与并发

- `context.Context` 作为第一个参数贯穿到所有 I/O（DB、HTTP、exec）。
- 每个 goroutine 必须有明确的 owner 与退出条件；不接受没有 `cancel` 的长期 goroutine。
- ffmpeg/ffprobe 调用统一过 `internal/segment` 的并发闸：`min(4, max(1, NumCPU/2))`，固定值不可调（见 `internal/segment` 的闸注释）。超过上限的请求排队，不丢弃。
- 共享可变状态用 `sync.Mutex` 或 channel 二选一，不混用；禁止包级可变变量。

## 数据库

- 迁移文件 `internal/store/migrations/NNNN_<verb>_<subject>.sql`，只向前，不改历史文件。启动时自动执行到最新；版本真值在 `schema_migrations`（不是数据库的 user_version），并发保护用会话级 `pg_advisory_lock`（锁与迁移共用一条连接，多副本同时启动只有一个真正执行）。迁移失败即返回错误，`main` 记日志后退出——不提供半迁移服务。
- 所有 SQL 只在 `internal/store` 出现；其它包通过方法访问。占位符是 PostgreSQL 的 `$n`（`IN (…)` 的序号由 `placeholders(from, n)` 算偏移），值一律走绑定，不拼进 SQL 文本。
- 连接串是必填项 `DRAMA_HUB_DB_URL`；连接池参数走 env（`DRAMA_HUB_DB_MAX_OPEN_CONNS` / `DRAMA_HUB_DB_MAX_IDLE_CONNS` / `DRAMA_HUB_DB_CONN_MAX_LIFETIME_SEC`），非法值启动即报错，不静默退回默认。
- 对外集号（Emby 看到的一集）是**运行时派生值**，没有派生表：库里只有原始 `episode`，`store.ListEmbyItems(dramaID)` 现读 `merge_chunk_size`、按 `ep_no` 顺序切块得出对外剧集（`deriveEmbyItems`，ADR-0014）。所以改 N 后下一次读取就是新分块，同步完成与首播回填都不碰任何派生数据；播放单元由 `store.PlaybackEpisodes` 用同一套规则判定，不存在的形态一律 `ErrNotFound`。
- 合并分块只在 `internal/store/merge.go`：`chunkEpisodes` 是纯函数（按列表连续切片），`merge_chunk_size` 每次派生现读 `settings`（ADR-0010 / 0014）。没有派生表、没有 `merge_group` / `merge_item` 表，也没有人工编排入口。
- 时间统一存 UTC 的 RFC3339 文本（列类型是不透明 `TEXT`，`空串 = 还没发生过`）；展示时区转换在 Web 层做。只有 `schema_migrations.applied_at` 是 `TIMESTAMPTZ`（runner 自己的时间）。
- store 用哨兵错误表达结果：查询/删除无行返回 `ErrNotFound`，唯一/主键约束冲突（SQLSTATE `23505`）由 `wrapWriteErr` 统一映射为 `ErrConflict`（均可 `errors.Is` 判断），不向调用方泄漏裸 `*pgconn.PgError`。
- `drama` / `episode` 的写入一律 upsert：`drama` 以 `(provider, provider_id)`、`episode` 以 `(drama_id, ep_no)` 为幂等身份，冲突时只更新可变业务列，身份列与入参 `id` 被忽略，返回行 id。其中 `episode.duration_sec` 只在插件给了非零值时覆盖，否则保留库里已有的值 —— 上游批量拿不到时长（恒 0），首播建索引回填的实测值不能被下一轮刷新清零。
- `drama → episode` 与 `episode → segment` 都是 `ON DELETE CASCADE`（PostgreSQL 外键默认强制，不需要连接参数）；服务不提供删剧入口，该约束只保证库内引用完整性。
- `segment` 表是「每集的分片索引」，由 `internal/segment` 索引任务整体重建：`ReplaceSegments` 一事务内先删后插（重复索引幂等、失败整体回滚），`ListSegments` 按 `start_sec` 升序（即片内序）；无行返回空切片而非 `ErrNotFound`（「还没索引」是正常状态，由分片端点按 503 处理）。
- 跨行写入用 `Store.InTx`：同一事务内执行、失败整体回滚。事务内 `Store` 复用同一批读写方法；取数/网络请求必须在事务之外完成（长事务持行锁，会挡住同剧的其它写者）。
- **写事务的锁序固定为「drama → 子表」**：任何按剧写入子表的事务，第一条写语句之前先 `lockDrama`（`SELECT … FOR UPDATE`，`internal/store/lock.go`），再动 `episode`。两条事务以相反顺序请求同两行即 40P01 死锁（一方被中止）：同步事务先锁 `drama`（`UpsertDrama`）再逐集锁 `episode`，所以首播回填必须先锁 `drama` 再 `UPDATE episode`。新增按剧写入的入口必须遵守，并用「外部持锁 + `FOR UPDATE NOWAIT` 探针」的测试钉住（`internal/store/crud_test.go` 的 `TestBackfillLocksDramaBeforeEpisode`）。

## 配置

> 2.0 的全局 key 已全部落地：`cache_dir` 随 S3（MKACG-608），`cache_max_mb` /
> `cache_lru_days` 随 S7（MKACG-611，范围见 `docs/CACHE-MODEL.md`「淘汰」），刷新
> 调度的三项（`refresh_interval_sec` / `refresh_batch` / `refresh_timeout_sec`）与
> `plugin.<源>.refresh_interval_sec` 随 S4（MKACG-606，范围见
> `docs/SCHEDULER-MODEL.md`「配置」）。
>
> 2.0 明确不做**整库**缓存预热（MKACG-603 的「不在范围」表），1.0 预留的 `seg_prefetch` /
> `pin_days` 两个死键已删除（MKACG-616）：它们从没有消费者，留着只会让人以为改了有用。
> 库里若还有这两行，`Settings` 会按 `ErrUnknownSetting` 忽略它们，不需要数据迁移。
> 播放触发的**直链**预取（MKACG-681 / ADR-0017）不是分片预热：它只申请直链、落
> `episode_stream`，不生成任何分片字节；**分片**预热由 MKACG-682 / ADR-0018 引入，
> 参数是 `segment_prefetch_blocks`（默认 1，0 = 关闭）。

**生效时机逐键标注在配置表里**，只有四种取值（MKACG-617 的生效性矩阵是实测真值来源）：

- **即时**：每次使用都查库（`Settings` 本身不缓存，硬约束 5）。请求路径现读
  `strm_prefix` / `stream_secret` / `webdav_user` / `webdav_pass`，
  缓存每次读写现读 `cache_dir`，调度器每轮现读 `plugin.<源>.enabled` /
  `refresh_batch` / `refresh_timeout_sec`；`merge_chunk_size` 在每次派生对外剧集时现读
  （`ListEmbyItems` / 播放单元 `PlaybackEpisodes`），改完下一次读取就重排对外集号、
  旧 id 随即 404（ADR-0014）。
- **≤60 秒**：调度器每 60 秒醒一次，用 `refresh_interval_sec`（或
  `plugin.<源>.refresh_interval_sec` 覆盖）算下一轮是否到期。改值后状态页立刻按新值算
  「下次计划」，实际触发最多晚 60 秒。
- **≤5 分钟**：缓存淘汰循环固定 5 分钟一轮，`cache_max_mb` / `cache_lru_days` 在下一轮
  淘汰才生效。
- **重启后**：`plugin.<源>.base_url` 与 `plugin.<源>.request_timeout_sec` 是构造期注入的
  不可变配置（`*http.Client` 的 Timeout 在这里定型，见 `docs/PLUGIN-CONTRACT.md` 硬性
  要求 2）。状态页对这两项同时给「settings 现值」（`base_url` / `request_timeout_sec`）
  与「启动时注入、正在生效的值」（`*_effective`），两者不一致时 `restart_required=true`；
  页面直接写「重启后生效」，不再出现「显示新值、跑的还是旧值」。

运行时配置只有一张表 `settings(key TEXT PRIMARY KEY, value_json TEXT, updated_at TEXT)`。

启动期只读这几个环境变量：`DRAMA_HUB_DB_URL` / `DRAMA_HUB_DB_MAX_OPEN_CONNS` / `DRAMA_HUB_DB_MAX_IDLE_CONNS` / `DRAMA_HUB_DB_CONN_MAX_LIFETIME_SEC` / `DRAMA_HUB_ADDR` / `DRAMA_HUB_LOG_LEVEL`
（进程自身），`DRAMA_HUB_STREAM_SECRET` / `DRAMA_HUB_CACHE_DIR`（空卷首次引导，见下），
`DRAMA_HUB_ADMIN_TOKEN`（后台写操作令牌，见「管理 API」的写操作鉴权）。其余全部走 settings。

**首次启动的 env 引导（MKACG-616）**：下面两个 env 只在 settings 里**还没有该键时**写入
一次（`Settings.SetIfAbsent`），已有值一律不覆盖 —— 重启不会把手改过的 settings 覆盖回
env；env 缺省或值非法（secret 不足 32 字节）时只记一条 warn、不写坏值、不 panic（看板上
显示「未设置」就是这条原因）。后台改配置走 `PATCH /api/settings`（MKACG-622），但那要
服务先起来、还要有令牌；空卷上这几项只能靠 env 种下，否则服务起不来（密钥缺了 `/video`
恒 500），也就没有后台可改。

| env | settings 键 | 说明 |
|---|---|---|
| `DRAMA_HUB_STREAM_SECRET` | `stream_secret` | 空卷没有它 `/video` 恒 500 |
| `DRAMA_HUB_CACHE_DIR` | `cache_dir` | 镜像里默认 `/data/cache`（`WORKDIR /data`，相对默认值会落成 `/data/data/cache`）；裸机默认仍是 `./data/cache` |

**WebDAV 账号密码不走 env**（lxz 2026-10-31 定）：`webdav_user` / `webdav_pass` 是后台配置项，
由 `PATCH /api/settings` 或 `/settings` 页的表单写入（MKACG-622）；env 再种一份就会出现两个
真值，所以这两个 env 不存在，设了也会按「未知变量」报出来。

拼错或已废弃的 `DRAMA_HUB_*` 变量会打一条 warn，连同**支持的键**一起列出（只报变量名、
不报值）：静默忽略是「配了却没生效」的常见来源，见 `cmd/drama-hub/bootstrap.go` 的
`warnUnknownEnv`。

**`.strm` 的绝对地址（B1，MKACG-616）**：`strm_prefix` 为空时，`.strm` 里的 URL 由**请求的
`Host` 推导**成 `{scheme}://{Host}/video/{id}.m3u8`（`scheme` 取 `X-Forwarded-Proto`，没有就按
连接是否 TLS）。选这条路而不是「列为必填」的理由：验收要求默认安装下 `.strm` 里的 URL
直接 `curl` 就能 200；必填路径在「没配」时把播放能力整段关掉，而 Host 推导在直连、反代
（`X-Forwarded-Proto`）、以及 rclone 用任意地址挂载时都成立。配置了 `strm_prefix` 时以它为准
（对外地址与访问地址不同时用这个）。

| key | 默认 | 取值范围 | 生效时机 | 作用 |
|---|---|---|---|---|
| `strm_prefix` | `""` | 带 host 的绝对 URL 或空 | 即时（每次渲染 `.strm`） | `.strm` 里 `/video/{id}.m3u8` 前面的**对外地址前缀**（本服务自己的公开地址；反向代理/自定义端口时填，尾 `/` 写入时归一掉）；空 = 按请求 `Host` 推导绝对地址（见上「`.strm` 的绝对地址」）。不指向 rclone/openlist |
| `stream_secret` | 必填 | ≥ 32 字节 | 即时（每请求） | 出流 HMAC 签名密钥（见 ADR-0003）；换了之后所有旧 URL 失效 |
| `stream_url_ttl_sec` | `900` | 60–86400 | 即时（下一次解析直链） | 直链的默认有效期（秒）：插件给了 `Stream.ExpiresAt` 时以上游为准，没给就用「解析时刻 + 本值」写进 `episode_stream.expires_at`。未过期的直链直接复用、零外呼；过期或 CDN 拒绝（401/403/404）才重解析（ADR-0016）。默认 900 s 的取值理由见 ADR-0016「决策」 |
| `merge_chunk_size` | `10` | 1–500 | 即时（每次派生对外剧集） | 每 N 集并成 1 条对外剧集（ADR-0010 / 0014）。库里只存原始集，列举与播放每次现读本值重算，改完下一次读取即生效、不用重启也不用同步；改 N 会重排全库对外集号，旧合并 uid 随即 404（分片缓存靠 LRU 过期，不主动清） |
| `play_session_ttl_sec` | `120` | 30–3600 | 即时（每请求现读） | 「正在播放」会话的闲置回收阈值（秒）：某个 (客户端, 播放单元) 超过该秒数没有新的分片请求，就从 `/api/playback` 消失。会话只存内存、不落库，重启清空；同步轮次不计入（MKACG-683） |
| `prefetch_concurrency` | `1` | 0–4 | 下一个预取任务开始时 | 整剧直链预取的并发上限：同时在途的批数（一批 = 一个合并块），默认 1 = 串行；`0` = 关闭预取。一次任务开始时读一次，改值由下一次播放触发的任务生效（ADR-0017） |
| `prefetch_retry_sec` | `900` | 60–86400 | 下一个预取任务开始时 | 某集预取失败后的固定退避（秒）：`retry_after = 失败时刻 + 本值`，退避期内的集不再被预取；不做指数退避。前台播放该集不受影响、仍现解析（ADR-0017） |
| `segment_prefetch_blocks` | `1` | 0–10 | 下一次播放列表触发时（每个任务开始时读一次） | 分片预热提前量：除当前块剩余成员外，再预热后面几个合并块；`0` = 关闭。预热与前台共用 ffmpeg 闸且前台优先，生成的分片在容量吃紧时先淘汰（MKACG-682 / ADR-0018） |
| `webdav_user` / `webdav_pass` | 必填 | 非空字符串 | 即时（每请求） | WebDAV Basic 认证 |
| `webdav_page_size` | `1000` | 1–100000 | 即时（每次目录页请求） | WebDAV 集合 GET 目录页的单页条数：超出部分不渲染，页面上写「共 M 项，已显示前 N」。分类层有 21.8 万项目录，整页铺开会生成百 MB 级 HTML（MKACG-671；语义见 `docs/DIRECTORY-CONTRACT.md`「集合 GET」） |
| `cache_dir` | `./data/cache`（镜像 `/data/cache`） | 非空路径 | 即时（每次读写） | 本地缓存根目录：海报 `cover/`、分片 `seg/`，临时文件 `tmp/`（`docs/CACHE-MODEL.md`）；三个目录在启动时预先建出 |
| `cache_max_mb` | `8192` | 0–1048576 | ≤5 分钟（下一轮淘汰） | 分片缓存上限（MB）；`0` = 不按大小淘汰 |
| `cache_lru_days` | `30` | 1–365 | ≤5 分钟（下一轮淘汰） | 超过 N 天未命中的缓存条目（所有 kind 都算）删除 |
| `cover_timeout_sec` | `5` | 1–60 | 即时（每次海报下载） | 海报（`artifact(kind='cover')`）的单次出站超时。**不跟插件 `base_url` / `request_timeout_sec` 走**：封面 URL 是上游页面里的三方 CDN 绝对地址（实测 `p3-novel.byteimg.com`），`base_url` 改不到它，而页面的 30 s 超时会让一张卡住的图拖住整部剧的同步。取不到只记一条 warn，该剧不算同步失败（MKACG-620） |
| `plugin.<源>.enabled` | `true` | 布尔 | 即时（每轮调度现读） | 是否参与**刷新调度**（缺失按 `true`）；手动同步（`POST /api/dramas`）与播放（`/video`）不受影响 |
| `plugin.<源>.base_url` | `""` | 带 host 的绝对 URL 或空 | **重启后**（构造期注入） | 源地址；空 = 用插件内置默认（红果 `https://hongguoduanju.com`） |
| `plugin.<源>.request_timeout_sec` | `8` | 5–300 | **重启后**（构造期注入） | 单次出站请求超时（一次 GET 内部最多试 3 次，见 `docs/PLUGIN-CONTRACT.md`「出站重试」） |
| `refresh_interval_sec` | `3600` | 300–86400 | ≤60 秒（调度器醒来） | 全局刷新间隔，一轮到期 = `last_run_at` + 本值 |
| `plugin.<源>.refresh_interval_sec` | 空 = 继承全局 | 300–86400 | ≤60 秒（调度器醒来） | 源级间隔覆盖（用 `Settings.OptionalInt` 读，无行即「继承」） |
| `refresh_batch` | `1000` | 0–10000 | 即时（每轮调度现读） | 每轮存量刷新的部数；`0` = 关闭存量轮转 |
| `refresh_timeout_sec` | `900` | 60–86400 | 即时（每轮调度现读） | 单轮预算（`context.WithTimeout`） |
| `jellyfin_url` | `""` | 带 host 的绝对 URL 或空 | 即时（每次触发现读） | Jellyfin 基址（如 `http://jellyfin:8096`）；空 = 关闭媒体库维护 |
| `jellyfin_api_key` | 无行（= 空） | 空或非空字符串 | 即时 | Jellyfin API key（**只写不读**：PATCH 回 `null`，状态页只给「已设置」）。空 = 关闭 |
| `jellyfin_library_root` | `""` | 容器内绝对路径（非 `/`） | 即时 | **Jellyfin 容器内**看到的挂载根（本部署 `/data/video/dramahub`）；空 = 关闭。填成宿主路径或本服务的挂载点会建出空库 |
| `jellyfin_library_prefix` | `短剧 - ` | 非空字符串 | 即时 | 库名前缀：`name = <前缀><分类目录名>`，同时是「哪些库归本服务管」的第一条判据。不接受空串（空前缀会让所有库都算自管） |
| `jellyfin_timeout_sec` | `10` | 1–60 | 即时 | Jellyfin 单次出站超时；`http.Client` 不设全局 Timeout，超时靠每次请求的 ctx |
| `hongguo_cookie` | 无行（= 空） | 空或任意非空字符串 | 即时（每次取流现读） | 红果站点登录态 Cookie（**只写不读**：PATCH 回 `null`，状态页不给值；不进日志与错误文本）。**只作最后兜底**：App 通道与匿名站点通道都不通时才带它再要一次，播放不依赖它。空 = 匿名；配了会解锁该账号有权限的集。凭据失效时锁定集回 403 `protected_playback_unavailable`，出流面据此报「需要登录态」而不是 502（MKACG-676） |

`<源>` 是 `Plugin.Name()`（红果是 `web`），键的格式与校验在 `internal/store`
（`PluginSettingKey` + `specFor` 的前缀模式），调用方不自己拼字符串。

`strm_prefix` 的尾 `/`（B2）：`{strm_prefix}/video/…` 是字符串拼接，库里存着
`http://host/stream/` 会产出 `//video/`（MKACG-617 实测）。**写入时归一化**（`parseSetting`
对 URL 类键 `TrimRight`，状态页显示的就是归一后的值），**渲染时再去一次**（存量手改的行
也能拼对）。归一化在校验之前，所以校验看的是削过的值：`http://` 只剩 `http:`、
`http:///path` 的 host 为空，两条都会被 `IsAbs` 放过而拼出 `http:/video/…`，因此
URL 类键还要求 **host 非空**（MKACG-634）；空串仍是合法值（按请求 `Host` 推导）。
非法值照旧报错，不猜。

写接口的校验只有一份：`PATCH /api/settings` 走 `Settings.SetMany` → 同一条 `parseSetting`
（`specFor` 的范围与类型）。越界 / 类型不符 → 400 且整批不落库，错误文本原样带回（页面
直接显示「必须在 0–1048576 之间」这类原因）。上表的「取值范围」就是接口的边界，页面不
另写一份校验。

`cache_max_mb` 的下界是 `0`（= 不按大小淘汰），所以 `0` 是合法值、返回 200；能返回 400 的是
`-1` 与 `1048577`。

缓存淘汰由本服务自己执行（ADR-0007）：`internal/cache` 的淘汰循环每 5 分钟一轮，
先按 `cache_lru_days` 删过期，再按 `cache_max_mb` 删最久未用的分片（`docs/CACHE-MODEL.md`
「淘汰」）。淘汰间隔是固定值，不做成可调项。

**海报出站（MKACG-620）**：`internal/cache` 下载封面时按 `cover_timeout_sec` 现读超时
（默认 5 s，可调 1–60 s），不用插件那份出站超时。理由是地址与语义都不同：插件请求打的是
`{base_url}/player/{id}`，封面打的是上游页面里写死的三方 CDN 绝对地址，换 `base_url`
不会改变封面请求的目标；把「页面等 30 s」套到百 KB 的图上又回到「一张图拖住整部剧」。
封面取不回来（离线、CDN 不可达、超时）时同步照常成功：剧与集入库、`artifact(kind='cover')`
留空、日志一条 warn，下一轮自然重试。
ffmpeg 并发上限固定为 `min(4, max(1, NumCPU/2))`，不做成可调项（见 `internal/segment` 的闸注释）。

新增参数必须同 PR 更新上表，并在 `settings` 里给默认值 + 范围校验。写非法值返回 400，不静默纠正。

**App 通道的两个 host 不是参数，所以上表不新增行**（MKACG-626 D2，MKACG-628/630 落地）：
`hongguo.Config` 的 `DeviceRegisterURL`（匿名设备注册）与 `AppVideoURL`（App 取流）都是
构造期字段，默认值内置、测试注入假上游，与 `BaseURL` 同一类——改它们要重启，不是热读。
理由是 App 侧协议事实（路径与查询串里的版本号是逆向常量），没有用户会调；多一个键就多
一条「配了却不生效」的来源。代价：上游换域名发版。

## 播放面（`/video`）

契约真值：`docs/PLAYBACK-CONTRACT.md`（S8 / MKACG-612 落地）。本节只写实现口径。
两个口子由 `internal/segment` 提供 handler、装配根注册：

| 端点 | 方法 | 说明 |
|---|---|---|
| `/video/{id}.m3u8` | GET / HEAD | 播放列表；单集与合并集**同一路径形态** |
| `/video/{id}/{seq}.m4s` | GET / HEAD | 整集 fMP4 分片：查本地缓存 → 未命中整集转封装、落缓存、再回 |

- `/dav/`、`/s/{token}/*` 与 `/s/*/master.m3u8` **已退役、不留兼容层**：`.strm` 是按
  请求渲染的，下一次读取就是新内容，没有存量文件需要兼容。
- `internal/stream`（rank 20）只剩两件事：`ID` / `ParseID`（对外 id 的 HMAC 签名）与
  `Resolver`（按 `episode_stream` 复用或解析直链，返回 `stream.Direct{URL, Key}`；
  `Key` 是插件给的内容密钥，原样透传给播放面，见 `docs/PLUGIN-CONTRACT.md`
  「加密切片与 Key」）。直链与 key 按 `expires_at` 复用、CDN 拒绝时作废重取
  （ADR-0016）。它不碰 HTTP —— 状态码映射在 `internal/segment`。

### id 规则

```
id  = uid + "-" + hex(HMAC-SHA256(stream_secret, uid))[:12]
uid ∈ { "e<episode.id>", "m<dramaID>_<epFrom>_<epTo>" }
```

- 生产方是 vfs 的 `.strm` 渲染（`cmd` 注入 `stream.ID`），校验方是 `/video`
  （`stream.ParseID`）：两侧同一个实现，不各写一份 HMAC 拼接。
- 单集 `uid` 只依赖 `episode.id`；合并集 `uid` 只依赖 `(dramaID, 块内集号区间)`，不含
  自增主键。两者在「集集合与 N 不变」的前提下都不随同步、重启、改目录而漂 →
  URL 不漂、分片缓存不废；换 `stream_secret` 后所有 URL 失效（ADR-0003 的既定后果）。
  secret 每请求读一次，不缓存（硬约束 5）。
- 分隔符是 `_` 不是 `-`：播放 id 是 `<uid>-<签名>`，`stream.ParseID` 按首个 `-` 切；
  uid 里带 `-` 会把「uid 不存在」的 404 顶成「签名不对」的 401。
- **播放单元的合法性由当前派生规则判定**（`store.PlaybackEpisodes`）：`e<id>` 只在
  它当前自成一块（块内 1 集）时有效，`m<…>` 只在它的集号区间与当前切块完全一致时有效；
  改 `merge_chunk_size` 后某集被并进块、或块的成员变了，旧 uid 立即 404 —— 这是契约
  「不飘移」第 3 条，也是 401（签名不对）与 404（播放单元不存在）的分界。
- 状态码矩阵里没有 302、没有过期时间、没有 token 表。

### 时长探测（按需）

`ensureDuration` 在**第一次播放某个 uid** 时同步 `ffprobe` 一次，只问
`format=duration`（ADR-0011 删掉了关键帧与编码签名，探测不再做关键帧预扫）：

- **唯一真值**：`episode.duration_sec`。playlist 的 `#EXTINF`、`#EXT-X-TARGETDURATION`、
  `.nfo` 的 `<runtime>` 全部由它派生，没有第二份时长。
- **只探 0**：`duration_sec` 已非 0 的集直接用它（零外呼）。插件恒给 0，所以「首播多一次
  ffprobe」只发生在从没播过的集上。
- **按需、不预建**：库里有 2847 万集，全库 ffprobe 不可完成，代码里没有任何批量
  预建入口（只在请求路径上按需探测）。
- **30 s 硬上限**：整次补齐（`ensureDeadline`）走一个 `context.WithTimeout`，超时回
  503 + `Retry-After: 5`（`httpx.ErrUnavailable`），不出半截时间轴。
- **失败不写**：探测失败不写任何东西（`duration_sec` 保持 0、也没有 `segment` 行）。
- **回填**：探测到的源时长回填 `episode.duration_sec`
  （`store.BackfillEpisodeDuration`：只填 0、不覆盖非零值，非正时长视为探测失败）。
  回填不碰派生数据：对外 `runtime_sec` 在列举时现算（单集取本集时长、合并集取组内之
  和），`.nfo` 的 `<runtime>` 取 `runtime_sec / 60`，所以回填后下一次读取就是真实值；
  没播过的集仍是 0，`.nfo` 照常渲染（`<runtime>0</runtime>`，Emby 视为未知）。
- **整数秒**：时长按整数秒写库（每集 ≤ 0.5 s 误差，合并 10 集最多累计 ~5 s 显示差），
  不为它加 float 列（第二份真值）。
- **顺带对齐通道行**：同一次解析出的通道与 `segment.channel` 不一致时覆盖它
  （`ensureChannel`）。分片端点复用这一行，所以命中缓存的分片请求不解析直链。

### 状态码矩阵

| 情形 | `/video/{id}.m3u8` | `/video/{id}/{seq}.m4s` |
|---|---|---|
| 正常 | 200 + `application/vnd.apple.mpegurl` | 200 + `video/mp4` + `Content-Length` + `Cache-Control` |
| id 签名不对 / 缺 `-` | 401（`httpx.ErrUnauthorized`） | 401 |
| 路径不是 `.m3u8` | 404 | — |
| `uid` 不是当前播放单元 / seq 越界 / 分片名非法 | 404（`httpx.ErrNotFound`） | 404 |
| 解析不到直链（`ErrNoStream` 等） | 502（`httpx.ErrUpstream`） | 502 |
| 时长探测超时 | 503 + `Retry-After: 5` | 503 + `Retry-After: 5` |
| secret 读失败 | 500（响应体与日志都不含 secret） | 500 |

- **直链按过期时间复用**（`stream.Resolver.Direct`，ADR-0016）：解析结果与
  `expires_at` 一起写进 `episode_stream`，未过期直接返回、零外呼；过期或 CDN 明确
  拒绝（401/403/404）才作废这一集并重解析一次。同一集的并发请求单飞。解析失败一律
  502，失败结果不写库。`Key`（加密片源的内容密钥，`nil` = 明文）与直链同生命周期：
  一起落库、一起失效；两者都 MUST NOT 进日志、错误文本与缓存键。
- **加密片源解密归 ffmpeg**（MKACG-626 契约 1/6，MKACG-629 落地）：`Key` 非空时整集转封装与时长
  探测的命令 MUST 把 `-decryption_key <hex>` 放在**源地址（`-i`）之前** —— 放到后面 ffmpeg
  只会把它当输出选项静默忽略。明文流的 argv 里不出现这个参数（空值会被 ffmpeg 判非法）。
- **探针解密自检**（MKACG-626 AC-8）：key 错时 ffprobe 常常**以 0 退出**、只往 stderr 吐
  `non-existing PPS …`，拿不到时长却也不算失败——那种「成功」会把 0 写进
  `episode.duration_sec`、把坏片写进缓存。所以 `Key` 非空时 stderr **必须为空**
  （实测正确 key 下 0 字节），非空即解密失败 → 502、不写时长、不写缓存。
  明文流不看 stderr（`-v error` 下好源也会吐无害告警）。
- playlist 是 `segment` 表的**纯函数视图**（`BuildPlaylist`）：不落盘、不缓存
  （理由见 `docs/CACHE-MODEL.md`「为什么 m3u8 文本不缓存」）。
- HEAD 与 GET 的 `Content-Length` 一致（硬约束 3）：playlist 手写长度；分片走
  `http.ServeContent`（Range 也由它处理，分片是完整不可变对象，不需要分段生成）。
- 分片字节先完整收进内存再生效（一集 12–40 MB）：ffmpeg 中途失败时客户端看到
  干净的 502，不是截断流。

### 分片缓存

`GET /video/{id}/{seq}.m4s` 的顺序是「查缓存 → 命中即回；未命中生成、落盘、再回」：

- 键 `sha256("e<episode.id>" \x1f channel)`（`cache.SegmentKey`），落
  `<cache_dir>/seg/<hash 前 2 位>/<hash>.m4s`；`artifact(kind='segment')` 是缓存行。
  `seq` **不进键**：同一集在单集 / 合并块 / 改 `merge_chunk_size` 后命中同一个文件（ADR-0011 D3）。
  通道（`web` / `app`）必须进键：同一集的两条通道字节流不同；判据是「本次解析有没有带
  `Key`」，内容密钥本身不进键。`segment.channel` 是它的持久形态。
- 并发同一个缓存键只生成一次（`Cache.GetOrGenerate` 的 single-flight）；
  **返回的 `[]byte` 是共享只读**，调用方不得就地改写。
- 每次**真实生成**打一条 `Info`（`msg="segment generated"`，带 `episode_id` / `channel` /
  `bytes`）——「第二次播放 ffmpeg 调用 0 次」在测试里就靠这条计数。
- **集边界时间戳在出流时平移**（ADR-0011「修正：集边界时间戳」）：缓存字节 0 基自包含，
  `ServeSegment` 取到字节后调 `shiftTimeline(body, start)`，把每个 `moof/traf/tfdt` 的
  `baseMediaDecodeTime` 加 `start × 该轨 timescale`（`start` = 本片在播放单元里的起点 =
  前面各集 `duration_sec` 之和，与 `#EXTINF` 同源）。`start == 0` 时零拷贝。
  不这么做，ffmpeg 系播放端（Emby 转码/DirectStream、mpv）会因集边界 PTS 回退丢帧
  （实测 3 集 325 帧只解 275 帧）。前置集 `duration_sec` 还没回填时**先补齐再平移**
  （`timelineStart`：解析一次直链 + `ffprobe` + 回填，与 playlist 同一份 `ensureDuration`），
  不能按 0 出流——那会把「每片从 0 起」原样放回去（检测现场 1550 包只解 800 帧）。补齐失败
  按 502/503 返回；只有「直接请求分片 / 首播并发」会走到这里，补齐后写库、后续零外呼。
  平移函数是纯函数（`internal/segment/mp4time.go`），结构不合规直接报错、不静默出流。
- 生成失败 → 502，**不写缓存**（失败结果绝不进缓存）；缓存自身故障按 500 报，
  不冒充上游问题。
- 淘汰归 `internal/cache`（每 5 分钟一轮，`cache_max_mb` / `cache_lru_days`，
  `docs/CACHE-MODEL.md`）。
- ffmpeg/ffprobe 一律带硬超时（`timeoutProbe=30s` / `timeoutRemux=300s`，整集要读完
  CDN 全程）；stderr 尾部只进日志，不落响应体；日志只记源 host，不落完整直链。并发闸
  `min(4, max(1, NumCPU/2))`，超出的请求排队不丢弃。
- **预热**：播放列表成功后按合并块预热分片（当前块剩余成员 + 后面
  `segment_prefetch_blocks` 个块，默认 1、0 = 关闭；ADR-0018）。与前台共用
  single-flight 与 ffmpeg 闸，前台优先（预热在跑时前台到达会取消它）；预热拿不到
  空槽位就跳过该集，从不排队。换单元 / 换剧 / 停机 / 前台静默 30 s 即取消。
  预热生成的分片带 `prefetched` 标记，容量吃紧时先淘汰。1.0 的两个占位键
  `seg_prefetch` / `pin_days` 仍是删除状态（MKACG-616）。

### 合并集

`uid = m<dramaID>_<epFrom>_<epTo>`：`unit` 交给 `store.PlaybackEpisodes` —— 它用
`ParseMergedUID` 反解出剧与集号区间，再按当前 N 切块、要求有一块的区间与 uid 完全一致
（连续列表切片保证区间里没有别块的集），`Timeline` 把它们排成连续 seq 空间（**第 k 片就是区间内第 k 集**）；playlist
在集边界插 `#EXT-X-DISCONTINUITY`。每片自带 `ftyp+moov`，所以集边界的时间戳重置由
DISCONTINUITY 表达，不再需要 `-output_ts_offset` 平移 PTS。分片端点只需要它那一片的
直链：通道行在就直接读，不在才解析一次。

## 日志

- `slog` 结构化字段，键用小写下划线：`drama_id`、`ep_no`、`uid`、`seq`、`duration_ms`、`upstream_status`。
- **禁止记录**：完整带签名的播放直链、token 明文、WebDAV 密码、解密 key（`plugin.Stream.Key`，
  也不许出现在错误文本与缓存键里）。直链与 key 虽然落库（ADR-0016），日志纪律不变：
  需要定位时记 `episode_id` + `channel` + `host` + 路径前 32 字符。
- 每个请求一条入口日志（方法、路径、状态、耗时），出站上游一条（源、状态、字节数；传输错误时该条额外带 `err` 底层原因，不含 URL）。

### 请求访问日志中间件

`httpx.RequestLogger(logger, next)` 在**装配根**（`cmd` 的 `newHandler`）挂**一次**，每请求
**恰一条** slog 记录，字段固定：

| 字段 | 类型 | 说明 |
|---|---|---|
| `method` | string | `r.Method` |
| `path` | string | `r.URL.Path` 原样（不含 query） |
| `status` | int | 响应状态码（未显式 `WriteHeader` 时 200）|
| `duration_ms` | int | 处理耗时（毫秒）|

级别按状态：5xx → Error，4xx → Warn，其余 Info。

**不再掩码路径**：2.0 之后 URL 里没有凭据段 —— `/video/{id}` 是签名 id（本身不是
密钥），WebDAV 凭据在 `Authorization` 头里。日志只记 `r.URL.Path`，天然不含 query，
`?token=…` 这类参数不会落盘；**禁止记录**完整带签名的上游直链（可能带短时凭据）。

中间件只允许挂一次：在 `web` / `stream` / `webdav` 内再挂一层会让同一请求产生两条日志。


## 插件与同步（internal/plugin、internal/sync）

播放源是插件。契约（接口签名、5 条哨兵错误、配置键、注册方式）以
`docs/PLUGIN-CONTRACT.md` 为唯一真值，本节只写实现口径。

| 包 | 职责 | 允许 import 的内部包 |
|---|---|---|
| `internal/plugin` | 契约：`Plugin` 接口、5 条哨兵、`Registry` | 无（只依赖标准库） |
| `internal/plugin/hongguo` | 第一个实现：红果官网 SSR，`Name() == "web"` | `internal/plugin` |
| `internal/sync` | 框架侧写库编排：`Syncer.SyncSeries` | `internal/plugin`、`internal/store` |

插件只做「上游字节 → 领域值」：不写库、不开事务、不保存状态（不存游标、不缓存直链、
不缓存解密 key —— key 随 `Resolve` 的返回值一次性交给框架，见 `docs/PLUGIN-CONTRACT.md`
「加密切片与 Key」）。
写库、事务在 `internal/sync`（只写 `drama` / `episode`）；刷新调度与游标在 `internal/scheduler`
（S4，MKACG-606）：本包只提供「按一个作品写一次库」这个幂等动作，调度器按源调它。

### 红果插件的上游成本

| 方法 | 上游 | 请求数 |
|---|---|---|
| `Work(ctx, ref)` | `GET {BaseURL}/player/{id}` | 1（剧目元数据 + 全部集） |
| `Resolve(ctx, ref, epNo)` | 详情页 `GET /player/{id}` 必发（集号 → `vid`）→ 网页通道 `GET /player/{id}/{vid}`；没拿到直链就走 App 通道（注册一次性身份 → 签名 → `POST` 取流）；再不行才 `POST /api/video/protected/play-info` 兜底 | 受保护剧 3（详情页 + **每次取流一次设备注册** + 取流；被拒重试时 4–5）；非受保护剧拿到明文直链 2、落到 App 3 |
| `Enumerate(ctx, cur)` | 目录索引：`index.xml` + 29 份子 sitemap + `seo-important.xml` + `/category` 34 页 | 64（每份子 sitemap 前重取一次 4 KB 索引）= 30s 级 |

`Resolve` 在网页通道是 2 个请求：契约入参只有集号，集号 → 源侧集标识（`vid`）只能从详情页取，
而插件不许保存状态；要压到 1 个必须改接口（把源侧集标识传进来）。
受保护剧（`protectedPlayback == true`）不发集页请求，直接走 App 通道（MKACG-676 路线调整）：
详情页 1 个 + 每次取流 1 次设备注册 + 1 个 `POST`（带请求签名）。**取流身份是一次性的**——
上游对同一条身份的第二次取流回 `200` + 空体，所以每集都新注册一条；被拒时再换一条身份 +
备用域名（`Config.AppVideoAltURL`）+ `Connection: close` 重试一次。App 通道不可用时才落到
站点播放通道（`POST /api/video/protected/play-info`，匿名只覆盖免费集）；站点回
`protected_playback_unavailable` 且配了 `hongguo_cookie` 时才带登录态要最后一次。站点只回
DASH-CENC（`play_info`）时按 `ErrUnsupported` 上报，不回落。
`Enumerate` 一页 = 一份上游文档（子 sitemap 整份返回，43,556 个 loc 一次读完），
三段串行：子 sitemap（29）→ `seo-important.xml` → `/category` 第 1..34 页。并集才是全集：
只用 sitemap 漏最新 53 部，只用 `/category` 只剩 800 部（`docs/L1-CORPUS-SCOPE.md`）。
游标 `v1:<段>:<位置>` 只记「哪一段、第几份/第几页」，可从任意游标续跑；认不出的游标按
`ErrSchema` 打回（静默从头开始会把「游标坏了」伪装成「枚举走完了」）。

- `BaseURL` 默认 `https://hongguoduanju.com`，配置键 `plugin.web.base_url`（空 = 用它）。
  它与 App 通道的三个 host 是**四个构造期缝**（`--network=none` 下都指向
  `httptest.Server`）：`Config.BaseURL`（官网页）、`Config.DeviceRegisterURL`（匿名设备注册，
  默认 `log5-applog-lf.fqnovel.com`）、`Config.AppVideoURL`（App 取流主域名，默认
  `api5-normal-sinfonlineb.fqnovel.com`）、`Config.AppVideoAltURL`（被拒后的备用域名，默认
  `api5-normal-hl.fqnovel.com`；注入了 `AppVideoURL` 的测试沿用同一个假上游）。四者都是
  构造期注入、默认值内置、**不进 settings**（MKACG-626 D2：上游换域名就发版）。`*http.Client`
  由装配根按 `plugin.web.request_timeout_sec` 构造（默认 8s）。
- 第四个注入缝是 `Config.Cookie func(ctx) string`（MKACG-676）：装配根把「现读
  `hongguo_cookie`」的闭包传进来，插件不 import `internal/store`；测试注入固定串或 `nil`
  （= 匿名）。它没有默认地址，所以除 `hongguo_cookie` 这一项外没有别的配置面。
- 响应体经 `io.LimitReader` 读，两档上限：页面（列表页 / 集页 / `/category`）4 MiB，sitemap 8 MiB
  （实测子 sitemap 每份 5,242,7xx 字节）；超限按 `ErrSchema`（页面异常放大，不是可重试的传输错误）。
- 解析：`_ROUTER_DATA` 后的 JSON 用标准库 `json.Decoder` 从 `{` 起解（不手写括号扫描）；JSON-LD 用 `regexp` 取出 `<script type="application/ld+json">` 块后按 `@type` 过滤。
- 校验（任一不满足即 `ErrSchema`）：`isSuccess` 不为 `false`；页面的 `series_id` 与 `seriesDetail.series_id` 都等于请求 id（被锁集会被上游 302 到别的集/别的剧页面）；`vid_list` 非空、元素全为数字、无重复。JSON-LD 块另做绑定校验：`url` 的 pathname 是 `/player/{id}` 或 `/player/{id}/{vid}`，且 `contentUrl` 的 pathname 含 `/{vid}`。
- `vid_list` 下标即集号（`ep_no = i+1`）；`episode.vid` = `vid_list[i]`。
- 哨兵映射（插件 HTTP 层，框架只用 `errors.Is` 判定）：429 / 403 / 444 → `ErrRejected`
  （444 = 上游丢连接；实测累计约 1 万次 `/player` 后源侧全量回 444，属限流，见
  `docs/L1-CORPUS-SCOPE.md`「取证 F」）；
  404 → `ErrNotFound`（集页 404 由 `Resolve` 翻成 `ErrNoStream`，见契约错误语义表）；
  其余非 2xx / 传输失败 / 读体失败 / 空响应 → `ErrUpstream`；结构不符 → `ErrSchema`；
  `VideoObject` 在但 `contentUrl` 缺 → `ErrNoStream`。`ctx` 被取消 / 超时直接返回 ctx 错误，
  不冒充源故障。插件不 import `internal/httpx`，状态码映射由边界包负责。
- 判定顺序 MUST 是**状态码先、响应体后**（MKACG-635）：404 + 空体曾因「空体 → 身份被挡」
  被判成 `ErrRejected`，多换一轮设备身份、多打一次上游。App 通道每一格：403/429/444 →
  `ErrRejected`（换一次身份，仍失败即 502）、404（带体或不带体）→ `ErrNoStream`、其余非
  200 → `ErrUpstream`、200 + 空体 → `ErrRejected`（换身份可自愈，MKACG-673）、
  200 + 非空但不是 JSON → `ErrSchema`、200 + `code != 0` → `ErrRejected`。
  网页通道同一顺序，200 + 空体按 `ErrUpstream`。站点播放通道也同一顺序：403 **先读体**，
  `{"error":"protected_playback_unavailable"}` → `ErrLoginRequired`，其余 403 → `ErrRejected`；
  404 → `ErrNotFound`、其余非 200 → `ErrUpstream`、200 + 空体 → `ErrSchema`（这个接口的
  被拒形态是 403，空体只可能是结构变了，MKACG-676）。
- 框架校验（`internal/sync`）：集表非空、`Episodes[i].No == i+1`、源侧集标识数量与集数一致，
  违反即 `ErrSchema` 且**不写库**（这是插件实现 bug，不是上游问题）。
- `episode.vid` 的来源不在契约的 `Episode` 里（只有集号 / 标题 / 时长）：框架用消费方定义的
  可选接口 `sync.WorkWithEpisodeKeys` —— 插件实现就一次调用同时拿到详情与每集源侧标识，
  不实现则 `episode.vid` 留空（不编造）。红果实现了它。

### 直链复用与失败自愈（ADR-0016，推翻 0004 的「不落库」）

**库里存的是带过期的短时凭据，不是永久链接。** `0004_drop_play_url.sql` 删掉
`episode.play_url` / `play_url_at` 防的是「1.0 把很久以前存的 URL 当作永久有效」；
`0010_episode_stream.sql` 存的每一行都必须带 `expires_at`，并有第二条失效路径
（CDN 401/403/404 → 作废 + 重解析一次）。两条路径任一命中都要重新向插件申请，
不存在「旧链接被当永久有效」的可能。

实测（2026-10-29，9 部剧逐一 ffprobe，**网页通道**）：列表页 `vid_list` 给全全部集，但单集页
SSR 的 `contentUrl` 只有当集为第 1 集时有值，且下载到的 MP4 **实际时长恒为 10.08 秒**
（JSON-LD 却声明整集时长）。受保护剧的完整片源走站点播放通道（MKACG-676，站点登录态），
App 通道退为兜底（MKACG-626/630）——同一集换通道就是另一条字节流，所以分片身份带通道段
（见「分片缓存」）。出流面把 `ErrLoginRequired` 映射为 403、`ErrUnsupported` 映射为 501，
其余解析失败仍是 502（`internal/segment` 的 `resolveFailure`）。三条通道都要走
`plugin.Resolve` 才拿得到直链：**解析结果按 `expires_at` 复用**，1.0 的
`/s/{token}/e/{uid}` 之所以恒 502，是因为它读的 `play_url` 列从来没人写过。

同步只保证「剧与集齐全、`vid` / `ep_no` 正确」；`/api/dramas` 也不再报「可播集数」：
那个数字在当前实现里恒为 0，删列只是让它别再骗人。

### 测试

假官网一律是 `httptest.Server`：`hongguo.Config.BaseURL` 指向它，不依赖真实外网。
`sync_at` 的时钟由测试注入（`sync.New` 之后设 `Syncer.now`），不依赖真实时间。
页面 fixture 放 `internal/plugin/hongguo/testdata/html/`，裁剪自真实页面并在文件头注明
来源、裁剪日期与覆盖场景；`internal/sync` 的入库一致性测试读同一份 fixture
（`../plugin/hongguo/testdata/html`），因为「迁移前后入库结果逐字段一致」必须用同一个页面。

## 刷新调度（internal/scheduler）

契约唯一真值 `docs/SCHEDULER-MODEL.md`（三段、并发取值、`next_due` 推导、错误与超时
口径）。本节只写实现口径。

| 文件 | 内容 |
|---|---|
| `scheduler.go` | 循环（睡 `min(最早 next_due, 60s)` → 重读配置 → 派发到期源）、`Trigger`（手动）、`Status`（状态页）、每源单飞锁 |
| `round.go` | 一轮三段（枚举 / 新剧灌库 / 存量轮转）+ `source_state` 写回 + 每轮一条结构化日志 |
| `schedule.go` | 纯函数：`nextDue`（`last_run_at` + interval）、`waitUntil`（夹在 `[0, 60s]`）、时间文本编解码、worker 数 |

- **配置热读**：`configFor` 每次醒来（以及每轮开始）现读 `enabled` /
  `refresh_interval_sec`（含源级覆盖）/ `refresh_batch` / `refresh_timeout_sec`；
  没有任何通知机制、不重建插件实例。读失败按默认值继续并 warn（配置错不该让调度停摆）。
- **单飞**：每源一把 `sync.Mutex`，手动与定时共用；拿不到即 409（手动）/ 跳过（定时）。
  源之间各一个 goroutine，互不影响。
- **游标**：枚举按页前推，走完写空串（下一轮从起点重来），被砍断写最后成功推进的位置；
  只有一轮收尾那次 `SaveState` 会落库，且用 `context.WithoutCancel`，超时也写。
- **next_due**：库里不存，永远由 `last_run_at + 现读 interval` 现算；失败也照此推进，
  没有退避表、没有失败计数。
- **错误分级**照 `docs/PLUGIN-CONTRACT.md` 错误语义表：`ErrNotFound` / `ErrNoStream`
  只跳过；`ErrUpstream` / 未知错误跳过并记 `last_error`（未知错误另计 `unknown_errors`）；
  `ErrRejected` 立刻中止本轮；`ErrSchema` 连续 20 次中止本轮。单部失败不影响「本轮完成」。
- **可观察**：每轮一条 `一轮结束` 日志，字段 `enumerate_ms` / `ingest_ms` / `rotate_ms` /
  `refs` / `ingested` / `rotated` / `failed` / `unknown_errors` / `err`；状态页读
  `Status`（`source_state` + 现读配置）。

## 代码组织

依赖方向单向：内部 import 仅允许 `rank(importer) > rank(importee)`；同级、反向、
未登记包一律失败，由 `scripts/check-deps.sh` 强制（`make lint` / `ci-lint.sh` 接入）。
新增包必须注册到 `scripts/check-deps.sh`。

| rank | 包 | 允许 import 的内部包 |
|---|---|---|
| 60 | `cmd/drama-hub` | 全部 internal |
| 50 | `internal/web` | 40 / 30 / 20 / 10 |
| 40 | `internal/webdav`、`internal/segment`、`internal/scheduler` | 30 / 20 / 10 |
| 30 | `internal/sync`、`internal/vfs`、`internal/httpx` | 20 / 10 |
| 20 | `internal/stream`、`internal/plugin/hongguo`、`internal/cache` | 10 |
| 10 | `internal/plugin`、`internal/store` | 无 |

`internal/httpx` 是 HTTP 边界的横切层：只依赖标准库、不 import 任何 internal 包；
与它同 rank 30 的 `sync` / `vfs` 互不 import —— 插件与同步编排不做 HTTP 状态分类。
`internal/plugin`（契约）只依赖标准库，与同为 rank 10 的 `store` 互不 import：
插件不认识库，库也不认识插件，两者只在 `internal/sync` 与装配根相遇。

- 一个文件一个关注点，超过 500 行先考虑拆分。
- 接口定义在消费方，只声明用到的方法；不预建「为将来准备」的接口。
- 构造函数注入依赖：`NewServer(store, settings, logger)`；不用全局单例、不用 `init()` 注册。
- 导出标识符必须有 doc 注释，说明契约与错误返回；私有函数只在「为什么」不显然时写一行注释。

## 虚拟文件树与渲染（internal/vfs）

树与文件内容全部按请求渲染，不落盘（AGENTS.md 硬约束 1）。`vfs.Tree` 只有两个入口：
`List(ctx, dir)` 列某个目录的直接子项、`Render(ctx, path)` 返回文件的完整字节。
形状、命名与冲突规则的真值是 `docs/DIRECTORY-CONTRACT.md`，本节写实现口径。

### 目录与命名（纯函数，`tree.go`）

```
短剧/                                  ← vfs 根（DAV 挂载点根映射到它，DAV 路径里没有这一段）
└── <分类>/                            ← drama.genre；缺失落兜底名「其他」
    └── <剧名> (<年份>)/
        ├── tvshow.nfo
        ├── poster.jpg              ← 仅当这部剧有可用封面（MKACG-650）
        └── Season 01/
            ├── <剧名> - S01E01 - <标签>.strm
            └── <剧名> - S01E01 - <标签>.nfo
```

- 顶层只有分类目录，没有 `全部/`：一部剧只出现在它自己那一个分类下。
- **分类与剧之间没有中间层**（MKACG-651 v5）：分片层已删除，`00`–`7f` 不出现在任何路径，
  也不存在 `store.ShardCount` / `ShardOf` / `drama.shard`。为什么删、代价与实测见
  `docs/DIRECTORY-CONTRACT.md`「单目录子项上限」——别再把它加回来。
- 兜底名「其他」同时覆盖 `genre` 为空与上游真的叫「其他」两种取值，它们并进同一目录。
- 同名冲突（同一个分类目录内基准名重复）：所有冲突成员改用
  `<基准名> [<provider>-<provider_id>]`，后缀是 `provider_id` 原文、不截断；不冲突的
  成员保持基准名。
- 剧目录 `<剧名> (<年份>)`：年份来自 `drama.year`；`year=0` 显式写成 `(<0>)`，不省略。
- 季目录恒为 `Season 01`（派生值的 `Season` 恒为 1）。
- 集号 `S%02dE%02d`：**对外集号取派生出的 `EpNo`**（硬约束 6），不是 `episode.ep_no`。
- `<标签>` = `embyLabel`：单集取源集标题、空则 `第<源集号>集`（不是对外集号：
  自动分块后第 3 块可能只装源第 21 集，标题应是「第21集」）；**合并集取
  `第 A-B 集`**（A/B 是块内最小/最大源集号，A==B 时退化为 `第 A 集`）。
- 目录名（含分类目录）与文件名里的 `/ \ : * ? " < > |` 一律替换为 `_`（Windows + WebDAV
  非法字符集）：分类名转义是 S8 / PL 2026-10-30 的裁决，见下「含路径分隔符的分类名」。
- 路径以 `/` 分隔、相对 vfs 根、无前导 `/`；`""` 与 `短剧` 都表示根。

**数据源是运行时派生的对外剧集**（`store.ListEmbyItems`，底层读 `episode`）：一个
合并块在树里只产出 1 条文件对，Emby 因此显示为 1 集、对外集号连续（硬约束 6）。改 N
后下一次列举就按新分块给出文件名，`drama` / `episode` 原始数据不动（ADR-0014）。

### 列举成本（一个目录一次查询）

`List(ctx, dir)` 只返回**一层**直接子项，每次常数条 SQL，与全库规模无关：

| `dir` | 子项 | 查询 |
|---|---|---|
| `""`（根） | 分类目录 | 1 次 `SELECT DISTINCT genre`（覆盖索引全扫，唯一 O(行数) 的一条） |
| `<分类>` | 剧目录 | 1–2 次 genre 存在性点查（`GenreExists`）+ 1 次 `WHERE genre IN (…) ORDER BY title` |
| `<剧目录>` | `tvshow.nfo`（+ 有封面时的 `poster.jpg`）+ `Season 01` | 1 次剧目点查 + 1 次封面行点查（目录名被 sanitize 时再加 1–2 次 genre 存在性点查） |
| `<剧目录>/Season 01` | 集文件对 | 同上 + 1 次 `ListEmbyItems` |

两类查询都走索引（`idx_drama_genre_title` / `idx_drama_title`），由
`internal/store` 的 `TestDirectoryQueryPlans` 用 `EXPLAIN ANALYZE` 钉住。

分类目录那次列举用**窄列**（`store.ListDramaDirsByGenre` → `store.DramaDir`：只取
`id/provider/provider_id/title/year/genre`）：它是唯一会一次物化整个分类的查询，
21.6 万子项时含 `plot`/`tags` 的 `[]Drama` 堆峰值 ≈100–130 MB，窄列 ≈51 MB
（MKACG-651 实测，数字与判据见契约文档）。命中目录名后再点查一次拿完整 `Drama` 去渲染。

**不存在「枚举整棵树」的路径**：旧实现每次请求 1 次 `ListDramas` + 每剧 1 次
`ListEmbyItems`，在 L1 规模（60 万部 × 46.8 集）下不可能完成。

两条实现口径（此处留档，改动前先读）：

- **分类目录名解析只用存在性点查**（`GenreExists`，`SELECT EXISTS(SELECT 1 … WHERE genre = ?)`，
  O(log n)）：目录名 → genre 取值集合是纯映射（`其他` → `{"", "其他"}`，其余就是目录名
  本身），不需要把行集取回来。不要换成 `SELECT DISTINCT genre` 再过滤（整条覆盖索引
  全扫，60 万行约 74ms），也不要换成 `DISTINCT genre WHERE genre IN (…)`（仍要扫该分类
  的全部索引行，183k 行约 44ms）。根目录是唯一必须拿到全部分类的地方，所以只有它付
  这一次全扫。
- 这条口径由 `internal/vfs` 的 `TestListDoesNotScanAllGenres` 用**数 store 方法调用**
  的方式钉住（不是数 SQL 文本）：根目录 1 次 `ListGenres`，其余层 0 次（除非目录名是
  转义形态，见下）。SQL 文本测试抓不住「点查写了但没接线」——上一轮 Review 就是这么
  抓到的。

含路径分隔符的分类名（上游脏 `tags[0]`，如 `剧情/古装`）**按目录名规则转义**
（`sanitizeName`：`剧情/古装` → `剧情_古装`，PL 2026-10-30 裁决）。不转义时它无法成为
一个路径段：强行拼进根目录会产出跨段节点，`x/net/webdav` 的 `walkFS` 给它 `Stat` 时
失败并中止，整份 `multistatus` 被截断（客户端拿到非法 XML，整个挂载点不可解析）；
而「跳过 + warn」会让这批剧在挂载点里静默不可达（QA 实测转义后可列可播）。

- 转义后同名的两个分类（`剧情/古装` 与 `剧情_古装`）并进同一个目录，记一条 debug。
- 目录名 → genre 取值集合因此不再总是「目录名本身」：`genresForDir` 先做点查
  （`GenreExists`），点查全落空时（转义形态）才退回一次 `ListGenres` 筛
  「转义后等于目录名」的分类 —— 这条路径只对脏分类与不存在的目录付代价。

剧目录名 → 剧目：先按 `WHERE title = ? AND year = ?`（`idx_drama_title`）点查，带冲突
后缀的形态按 `WHERE provider = ? AND provider_id = ?` 点查；命中后**仍比对「分类 +
目录名」**，不一致即 `ErrNotFound`（防止其他分类的同名剧被渲染到本路径下）。
标题含 `/ : * ?` 等字符时目录名是 sanitize 过的、点查必然不中，此时退到「本分类内比对」
（`WHERE genre IN (…) ORDER BY title`，索引区间查询，不是全表扫）。**代价**：这条回退
路径要扫整个分类，21.6 万部时实测 0.77–1.41 s/文件（`docs/DIRECTORY-CONTRACT.md`
「单目录子项上限」第 4 条）；这类脏标题在真实库里是少数，量大时升级路径是加派生列/
函数索引，**不要**恢复分片层。

### strm 形态

一行 URL + 尾 `\n`，前缀取 `settings.strm_prefix`：

```
{strm_prefix}/video/{id}.m3u8
```

- `id = uid + "-" + hex(HMAC-SHA256(stream_secret, uid))[:12]`（`stream.ID`），
  `uid` 取派生出的对外剧集 uid：单集 `e<episode.id>`、合并集 `m<dramaID>_<epFrom>_<epTo>`。
  **单集与合并集没有区别**，对外统一是 HLS 播放列表（`docs/PLAYBACK-CONTRACT.md`）。
- 签名算法在 `internal/stream`（rank 20）：vfs（rank 30）可以 import 它，但仍在装配根
  注入 `vfs.IDFunc`（`cmd` 传 `stream.ID`），让 vfs 只依赖「一个纯函数」这条缝。
- 播放面（`/video/{id}.m3u8` + `/video/{id}/{seq}.m4s`）见本节前面的「播放面」章。
- `strm_prefix` 与 `stream_secret` 每次渲染现读，改值即时生效（硬约束 5）。

### NFO 形态

`encoding/xml` 序列化，固定头 `<?xml version="1.0" encoding="UTF-8" standalone="yes" ?>` +
两空格缩进体 + 尾换行。XML 转义交给编码器。

| 文件 | 根元素 | 字段 | 说明 |
|---|---|---|---|
| 集 `.nfo` | `episodedetails` | `title` / `season` / `episode` / `runtime` / `plot` | 字段全部取自派生出的对外剧集：`episode` 是**对外集号**，`runtime` 单位是**分钟**（`runtime_sec / 60` 向下取整）；写秒会被 Emby 放大 60 倍。`plot` 优先单集简介，上游没有单集简介字段，当前用 `drama.plot` 兜底；为空则不写元素（MKACG-679） |
| `tvshow.nfo` | `tvshow` | `title` / `year` / `plot` / `genre` / `tag` / `thumb` | `plot` 来自 `drama.plot`（上游 `series_intro` 原文，不截断）；`genre` 多条，`drama.genre` 在前、`drama.tags` 其余项按原序去重追加；`tag` 多条，`drama.tags` 全量原文（保序，空串项丢弃）；`tags` 为空或非法 JSON 时 `genre`/`tag` 都不写；`thumb` 见下「poster.jpg」（MKACG-679） |

`aired` 不写：库里的时间只有采集时间，没有播出日期。

### poster.jpg（封面，MKACG-650）

剧目录下的 `poster.jpg` 是 Emby / Jellyfin 认的剧集封面名。它**仍然是虚拟文件**：字节取自
该剧的本地封面缓存（`artifact(kind='cover')` 指向的文件），磁盘上不会多出任何实体文件
（硬约束 1，磁盘只允许图片缓存与分片缓存）。

- **出现条件**：这部剧**曾经**本地化成功过（`artifact(kind='cover')` 有行）。判据只看行、
  不看文件，所以封面文件被手删、或被 `cache_lru_days` 淘汰（淘汰**不删行**，只清空
  `path`/`bytes`/`sha256`）之后节点仍在，下一次 GET / PROPFIND 才会触发按需回源。
  源未声明封面或同步时下载失败的剧**没有这一行**——树里就没有 `poster.jpg`，
  `tvshow.nfo` 也不写 `<thumb>`（不产出 0 字节文件、不写空元素）。
- **`tvshow.nfo` 的 thumb**：`<thumb aspect="poster">poster.jpg</thumb>`，值是**相对路径**
  （与 nfo 同目录），不写外链——外链会绕过本服务的缓存与淘汰，源图换址后指向错内容。
- **按需回源**：`Render` 时若本地文件不在，由注入的 `Coverer`（生产实现
  `cache.CoverCache.Poster`）回源一次并写回 artifact 行；同一封面的并发请求只回源一次
  （`cache.flightGroup`，与同步的本地化共用同一组）。取不回来就返回 `vfs.ErrNotFound`
  → WebDAV `404`，不返回 0 字节或半截文件，坏值不写缓存，下一次请求可再试。
- **列举不因封面失败而整体失败**：`internal/webdav` 的目录列举会**跳过**渲染成
  `vfs.ErrNotFound` 的子项。否则一张取不回来的封面会让整个剧目录的 `PROPFIND` 变成
  错误响应，把客户端的扫描停在这一层。
- 封面能力通过 `vfs.Coverer` 注入（接口定义在消费方）：vfs 不认识 HTTP、不读 artifact 表，
  回源/落盘/超时/single-flight 全在 `internal/cache`。`NewTree` 传 nil 等价于「这个库没有
  封面」（单测用它省掉缓存）。

### 长度

渲染结果的长度只有 `len(Render 的 []byte)` 一个真值，本包不提供独立的 `Size()` 或长度字段。
`internal/webdav` 的 `PROPFIND getcontentlength` 与播放面的 `Content-Length`
都取这个值（硬约束 3）。

## WebDAV（internal/webdav）

挂载点 `/webdav/`（`webdav.MountPath`），只读。协议实现委托 `golang.org/x/net/webdav`，
本包只提供两件事：`x/net/webdav.FileSystem` 适配（`fs.go`）与 Basic 认证（`handler.go`）。
路径与目录形状见 `docs/DIRECTORY-CONTRACT.md`。

### `Depth` 的三档取值与整树递归（MKACG-623 / 651 / 678）

`PROPFIND` 的 `Depth` 由协议库解析（`x/net/webdav` 的 `parseDepth`）：**只认 `0`、`1`、
`infinity` 三个字面量**，任何数值 ≥2 一律 `400 Bad Request`（协议库实现限制，不是本服务
的缺陷；不要为它加兼容层）。

**关键的一条（旧版本节写错过，2026-11-02 功能检测实测纠正）**：**省略 `Depth` 头等于
`infinity`**——RFC 4918 §9.1 就是这么规定的，`x/net/webdav` 的 `handlePropfind` 里
`depth := infiniteDepth` 正是缺头时的默认值。整树递归的实测代价是 369 MB / 116 s
（21.6 万剧的分类目录），所以 MKACG-678 起本服务**在认证之后、协议层之前直接拒绝这个形状**：

| 请求 | 行为 |
|---|---|
| `Depth: 0` | 只回资源自己（207） |
| `Depth: 1` | 回直接子项（207，**唯一应该用的形状**） |
| `Depth: infinity` / 省略 `Depth` 头 | `403` + `<D:error><D:propfind-finite-depth/></D:error>`（`application/xml`） |
| `Depth: 2` 及以上 | `400` |

拦截点在 `Handler.ServeHTTP` 里、认证与 308 之后、`servePropfind` / 协议层之前：请求不进入
响应缓存、不查树、不渲染，所以根目录也是一次 O(1) 返回。`infiniteDepth()` 把「显式 infinity」
与「缺头」当同一个形状处理——只拦前者会留下同一条挂死路径。决策与保留的实测数字见
ADR-0017 与 `docs/DIRECTORY-CONTRACT.md`「单目录子项上限」。

客户端与排障口径：

- rclone mount / Emby / Jellyfin **逐层 `Depth: 1`**，不受影响；`rclone lsf -R` 实测正常。
- 手工排障带 `-H 'Depth: 1'`：`curl -X PROPFIND <url>`（不带 `Depth`）现在直接拿到
  `403` + `propfind-finite-depth`，按提示补 `Depth: 1` 即可，不再等几分钟。
- 看到 `400` 先去查请求头（数值深度 ≥2），而不是查 vfs 树。
- 看到 `403` + `propfind-finite-depth` 是**协议的正常回复**，不是权限问题：改请求头，不要
  去查基本认证或 settings。
- `PROPFIND` 的响应缓存（ADR-0015）只缓存 `Depth: 0/1` 的 207；`403` 在进入缓存之前
  返回，不会把「拒绝」缓存成后续请求的结果。

### OPTIONS 的 Allow

`OPTIONS` 仍交协议层生成（`DAV: 1, 2`、`MS-Author-Via`），但 `x/net/webdav` 的集合分支只列
`OPTIONS, LOCK, DELETE, PROPPATCH, COPY, MOVE, UNLOCK, PROPFIND`，漏了本服务自
MKACG-671 起真的支持的 `GET, HEAD`（集合 GET/HEAD 出目录页）。`serveOptions` 在协议层返回后
补上这两个方法：`handleOptions` 只写响应头、不写状态码与正文（返回 `status 0`），响应在它
返回后仍未提交，所以这次改写是安全的；`TestOptionsAllowIncludesGetHead` 钉住「集合与文件都
含 GET/HEAD、`DAV` 头不变」。

### 路径映射

**DAV 根 = vfs 根目录**（`vfs.Root`）：`/` → `短剧`，`/爱情/剧名 (2024)/…` →
`短剧/爱情/剧名 (2024)/…`。DAV 路径里没有 `短剧` 这一段，根目录名来自 `vfs.Root`，
不在本包复制字面量。

因此 `rclone lsf --max-depth 4 dramahub:/` 的层级是 分类 → 剧 → Season 01 → strm/nfo
（`docs/ACCEPTANCE.md` P2）。Emby/Jellyfin 的库根**必须挂在分类目录上**
（每个分类一个 media folder），挂 DAV 根时分类目录会被当 Series ——
`docs/DIRECTORY-CONTRACT.md`「被推翻的做法」。

### 定位一个节点 = 列一次父目录

`Stat` / `OpenFile` 不枚举整棵树：把 DAV 路径拆成「父目录 + 末段」，调一次
`vfs.List(ctx, 父目录)`，再按末段匹配（快照里的 `listing.byName` 是「末段 → 节点」
索引，一层列举不会退化成 O(子项数²) 次比较）。因此一次 `Stat` 的成本是**一层列举**，
与库规模无关。

目录不存在时 `vfs.ErrNotFound` 被翻译成 `*fs.PathError{Err: fs.ErrNotExist}`：
**必须是 `*fs.PathError`**，因为协议层用 `os.IsNotExist` 分流，普通
`fmt.Errorf("%w")` 包装它认不出来，`PROPFIND` 会落到
`405 Method Not Allowed` 而不是 404。

### 目录打开是惰性的

`OpenFile` 打开一个目录时**不列子项**，`Readdir` 第一次调用才查。原因在协议层：
`x/net/webdav` 处理 `Depth: 1` 时会对**每个子项**再 `OpenFile` 一次（`prop.go`
的 `propnames`），惰性化之前「列举一层」因此变成「列举 N 层 + 渲染孙级文件」——
608k 部库实测 `/剧情` 13.30s、`/剧情/05` 858ms；惰性化之后同样的请求是
`List=2 / Render=0`。当前 `PROPFIND` 的查询次数由
`TestPropfindDepth1_QueryCountIndependentOfSize` 在 200 部与 12,000 部两种规模上
钉住（次数必须相同，且不渲染任何孙级文件）。

### 请求内快照

`x/net/webdav` 处理一个 `PROPFIND` 会对同一份数据反复调用 `Stat` / `OpenFile`（父目录一次、
每个子项再各一次）。逐次查库会让一次目录列举变成 O(目录项数) 次查询，因此一次请求内把
**已列过的目录**（键是目录，不是整棵树）与渲染结果挂在请求 ctx 上（`snapshot`），每请求
每层各算一次。**不跨请求**：没有 TTL、没有失效逻辑，数据改了下一个请求即见（硬约束 5 同理）。

### 长度

`getcontentlength` 与 GET 的 `Content-Length` 都取 `len(vfs.Render 的字节)`（硬约束 3），
没有第二条长度来源。`PROPFIND` 里目录不带 `getcontentlength`（协议层对 collection 不适用）。

### 认证

Basic，凭据取 `settings.webdav_user` / `webdav_pass`，**每请求现读**（硬约束 5，改值即时生效）。
缺任一项或凭据不符 → `401` + `WWW-Authenticate: Basic`，响应体是 `httpx` 统一错误体
（`code: "unauthorized"`）。比较用 `subtle.ConstantTimeCompare`。

### 只读

`Mkdir` / `RemoveAll` / `Rename` 与任何带写意图的 `OpenFile` 四大写入口都返回同一个
`errReadOnly`（`fs.go`，普通 error，不是 `fs.ErrPermission`），不落盘、不改库。

**状态码由协议层各 handler 各自映射，不是本服务能选的**，所以「写意图 → 405」的说法是错的。
`x/net/webdav@v0.62.0` 实测矩阵（MKACG-610 验收，17,558 部真库）：

| 方法 | 实测状态码 | 前置条件与映射来源 |
|---|---|---|
| `MKCOL` | 无 body（或空 body）`405`；带 body `415` | 带非空 body 时 `handleMkcol` 直接回 `UnsupportedMediaType`（协议层 TODO：不支持 MKCOL 带 body），根本不落到 FS；无 body 时 `Mkdir` 错误 → `MethodNotAllowed`（父目录在不在都一样） |
| `DELETE` | 路径存在（文件或目录）`405`，不存在 `404` | `handleDelete` 先 `Stat`（落空 → `NotFound`），`RemoveAll` 错误 → `MethodNotAllowed` |
| `PUT` | `404` | 已存在文件 / 不存在路径 / 根 / 带斜杠目录 / 父目录不存在，五种都 404：`handlePut` 只在 `OpenFile` 返回 not-exist 时给 409，而只读错误不是 not-exist |
| `MOVE` | `403` | 源存在与否都是 403（`moveFiles`：`Rename` 错误 → `Forbidden`） |
| `COPY` | 源存在 `403`，源不存在 `404` | `copyFiles` 先 `Stat` 源（落空 → 404），复制阶段失败 → `Forbidden` |
| `PROPPATCH` | 路径不存在 `404`；路径存在但 body 空/非法 `400`；路径存在 + 合法 `propertyupdate` `500` | `handleProppatch` 依次 `Stat` → `readProppatch` → `patch`；`patch` 的写失败没有状态码分支，直落 500 |

只读语义不靠状态码保证：四个写入口共享一个错误值，协议层怎么映射都改变不了「什么都没发生」。
因此 `TestReadOnly` 只断言「不是 2xx」（PUT / DELETE / MKCOL 三种），不钉具体码；
要钉 405 只能钉「无 body 的 `MKCOL`」与「已存在路径的 `DELETE`」。锁定用 `x/net/webdav.MemLS`
（协议层要求，不落盘）。

### 已知边界

- 修改时间恒为零值（`entry.ModTime`）：库里没有可靠的 mtime，`sync_at` 是采集时间。
  零值让 ETag 稳定在「内容长度」上，`http.ServeContent` 也不写 `Last-Modified`。
- 封面 `poster.jpg` 只在「这部剧有可用封面」时出现（MKACG-650），详见「虚拟文件树与渲染」的
  `poster.jpg` 小节：取不回来按不存在处理（404），目录列举跳过它，不返回 0 字节文件。
- **单目录成本随子项数线性增长，季层不摊薄**：`walkFS` 对一次 `PROPFIND` 的每个子项都要取
  props，所以一个 1000 集的季（2000 个子项）就是 2000 次渲染 + 一份大 XML。MKACG-610
  验收在 17,558 部真库上实测：该季 `Season 01` 的 `Depth: 1` = **1.8 MB / 25.9 s**（单请求）。
  分类层已不再摊薄（分片层删除，见 `docs/DIRECTORY-CONTRACT.md`「单目录子项上限」）；
  季层只能靠「单季集数量级本身有限」兜住（当前库实测最大单季
  1000 集，代码里没有任何集数上限常量），没有更强的摊薄手段——再切一层就改变了 Emby 认的季结构。

### 测试（两层，都不需要外部 rclone）

**单元**（`fs_unit_test.go`）：`fakeTree` 按目录给出子项与渲染字节，**不碰数据库、不起
HTTP**。覆盖：DAV 根 = vfs 根、长度只来自 `len(渲染字节)`、`os.IsNotExist` 认得的
`*fs.PathError` 包装、目录打开惰性（`OpenFile` 不查子项、`Readdir` 才查）、`Readdir`
直接子项且只查该目录、只读语义、请求内快照（同目录只 `List` 一次、同路径只渲染一次、
不跨请求复用）、列举/渲染错误透传。

**集成**（`webdav_test.go`）：真 PostgreSQL（独立 schema）+ 真 handler（`httptest`），验证协议契约。
客户端游览（等同 `rclone lsf --max-depth 4`、Emby 扫库的请求序列）由进程内的递归
PROPFIND 客户端完成，不依赖外部二进制、不起守护进程、不落盘：

- `TestPropfindDepth1_Layers`：`Depth: 1` 逐层只有该层节点（根只有分类、分类只有剧目录、
  剧目录是 `tvshow.nfo` + `Season 01`），且 DAV 路径里没有 `短剧` / `全部` / 两位十六进制段。
- `TestWalkTree_Complete`：max-depth 4 枚举出完整树（分类 → 剧 → Season 01 → strm/nfo），
  并对每个文件项断言 `getcontentlength == GET 实际 body 长度`。
- `TestWalkTree_DepthBounded`：遍历在预算内结束、深度不超过 max-depth。
- `TestPropfindDepthSemantics`：`Depth` 四种形状的真实语义——`0` 只回自己、`1` 只回直接
  子项、**省略头与 `infinity` 都会递归到孙级文件**、数值 `2` 回 `400`。这条盯的是文档
  口径（曾经写错成「不存在一次请求拉多层」），不是新的代码行为。
- `TestPropfindNotFoundIs404`：不存在的路径在**线缆上**必须是 404（不是 405）。
- `TestPropfindDepth1_QueryCountIndependentOfSize`：200 部与 12,000 部两种规模的
  `Depth:1` 查询次数必须相同（每层一次），且不渲染任何孙级文件。

消费方接口：`webdav.Tree`（`List(ctx, dir)` + `Render`）定义在 `fs.go`（接口在消费方，
DEV-STANDARDS「代码组织」），生产实现是 `*vfs.Tree`，单测注入 `fakeTree`。

**超时是硬要求**（`webdav_test.go` 顶部常量）：`requestTimeout = 5s`（单请求，
`http.Client.Timeout` + `context.WithTimeout` 双保险）、`walkTimeout = 30s`（整棵遍历）。
超时立即 `t.Fatalf` 而不是挂住——手工验证时 `rclone mount --daemon` 不退出的坑
不能再出现在测试路径上。

真机 `rclone lsf` / Emby 扫描仍在 `docs/ACCEPTANCE.md` 的 P2 手工清单里，
是验收层（人工、一次性），不是 `make test` 的组成部分。

## 管理界面（internal/web）

挂载点 `/`（装配根注册）。管理界面与管理 API 是同一个 handler：完整路径原样传入
本包（ServeMux 不剥前缀），未登记路径由本包 mux 返回 404 —— 「未知路径 → 404」
的语义不变，只是兜底者从顶层 mux 换成 web 的 mux。`/webdav/`、`/s/` 的 pattern 比
`/` 更具体，不受影响。

### 页面

页面集合 = 四个页面 + 一个对象详情页，同一个 `base.html` + 各自模板；`pages.go` 的
`pages` 与 `dramaPage` 是唯一答案，未登记路径（含旧的 `/status`）一律 404。

| 路径 | 页面 | 能干的事 |
|---|---|---|
| `/` | 概览（`data-page=overview`） | 健康带：摘要条（剧/集/对外集计数与最近同步时间）；每个播放源一张卡片：状态徽标（已启用/已停用/正在刷新）、上次运行/上次成功/下次计划（`title` 给完整 ISO）、仅非空时出现的上次错误、停用原因与「重启后生效」提示、页脚「立即刷新」；Jellyfin 卡片：状态徽标（未配置/已启用/正在同步/上次失败 + 缺哪几项）、上次同步时间、664 的进度与结果行、孤儿/冲突/被拒分类（空则整段不出）、页脚「同步媒体库」；「正在播放」卡片：当前几条流与剧名/集块、进度、已出字节、客户端，空态写「当前没有播放」，4 秒轮询且仅页面可见时发（MKACG-683）。补录入口已迁到剧库页（MKACG-675） |
| `/library` | 剧库（`data-page=library`） | 工具条：搜索（剧名 / 上游 id）、分类下拉、状态筛选（无封面 / 集数不齐）、视图切换（列表 / 海报墙，默认列表）、补录开关；**台账**九列（封面 / 标题 / 年份 / 分类 / 集数 / 上游 id / 上次同步 / Jellyfin 归属 / 操作），ARIA `table` 语义 + CSS Grid，≤640px 按行塌成卡片；海报墙是同一数据的第二种渲染（`loading=lazy`、2:3 占位）；前端 50 条/页；`<details>` 补录表单（`POST /api/dramas`，201 新建 / 200 只更新） |
| `/library/{id}` | 剧详情（`data-page=drama`） | 封面、标题/年份/分类、简介、WebDAV 路径（可复制）、Jellyfin 归属、单剧刷新（`POST /api/dramas` 传 `series_id`）、按当前 `merge_chunk_size` 分块的集列表：每块「第 A-B 集」+ 成员集与时长 + 播放地址 `…/video/<签名 id>.m3u8` 与复制按钮；id 非法或剧不存在 → 404（显式路由，不是 catch-all） |
| `/categories` | 分类（`data-page=categories`） | 分类名、剧数 / 集数、Jellyfin 库状态、启用开关（一行一个分类）；关掉的分类在下一轮同步时删除对应的自管库，剧与文件不动（MKACG-668） |
| `/settings` | 设置（`data-page=settings`） | 单表单、单「保存」，五组按人话排序：访问（WebDAV 用户/密码、`strm_prefix`）、同步与刷新（`refresh_interval_sec`）、合并与对外集号（`merge_chunk_size`）、缓存（`cache_max_mb` / `cache_lru_days`）、Jellyfin（地址 / API key / 媒体库根 / 库名前缀 / 单次超时）；每项带生效时机徽标与 settings key 提示，只提交改动过的项；底部可折叠「只读运行参数」（`cache_dir` / `refresh_batch` / `refresh_timeout_sec` / 密钥与凭据是否已设置） |

剧目的查看入口是 `/library` → `/library/{id}`（MKACG-675）；删除仍没有入口（对外集号
由运行时派生并按 `merge_chunk_size` 自动切块，ADR-0014），补录走剧库页的 `<details>`
或 `POST /api/dramas`。

设计系统（MKACG-667）：

- **单一 token 层**在 `:root`：颜色角色 `--bg` / `--surface` / `--ink` / `--ink-2` /
  `--ink-3` / `--line` / `--line-strong` / `--accent` / `--accent-ink` / `--ok` / `--warn` /
  `--danger` + 三档状态底 `--ok-bg` / `--warn-bg` / `--danger-bg`；间距 `--s1..--s7` =
  4/8/12/16/24/32/48；字号 11/12.5/14/17/22；圆角 2（徽标、输入）/ 6（卡片）。
  深色只在 `@media (prefers-color-scheme: dark)` 里覆盖颜色 token，不写第二套规则。
- **所有页面的 `<table>` 计数为 0**：播放源与媒体库用卡片，键值用 `dl` —— 删表就是移动端
  策略，不做 data-label 表格转卡片。剧库台账是唯一的表格形态，但用的是 ARIA `table` /
  `row` / `cell` 语义 + CSS Grid（ADR-0017），不是 `<table>` 元素：≥880px 是九列，≤640px
  同一份 DOM 按行塌成卡片。窄屏单列、触控目标 ≥44px、长值 `overflow-wrap: anywhere`；
  ≥880px 概览与设置项两列。断点只有 `max-width: 640px` 与 `min-width: 880px` 两个。
- **复用一组状态件**：`.badge[data-tone]` 状态徽标、`.badge[data-timing]` 生效时机
  （`immediate` / `refresh` / `evict` / `restart`）、`.alert` 错误与警告、
  `.state[data-state=loading|empty|error]` 三态。不引入图标库、字体文件、动画关键帧、
  主题切换开关或任何构建步骤（ADR-0002）。

模板 `templates/*.html` 与 `static/app.css`、`static/app.js`、`static/paging.js` 用
`//go:embed` 进二进制：界面没有构建步骤、没有 `package.json`（ADR-0002）。`paging.js`
只放剧库的纯逻辑（筛选 / 分页 / 台账单元格），不碰 DOM —— 浏览器里挂
`window.DramaHubPaging`，node 里可 `require`，`TestPagingContract` 用同一份源码做表驱动
单测（e2e 镜像装 nodejs 就是为了它，运行时镜像不需要）。页面 JS 只调下面这些 API，渲染结果
与 API 返回一一对应；页面响应同样给出精确 `Content-Length`（渲染成字节 → 再写响应）。

### 管理 API

| 端点 | 方法 | 说明 |
|---|---|---|
| `/api/dramas` | GET | 剧目列表：drama 行（含 `genre` / `plot` / 布尔 `has_cover`）+ `episodes` / `emby_items`（对外集数，运行时派生）计数 |
| `/api/dramas` | POST | 新增剧目：`{"series_id":"<上游剧 id 或 /player/ 链接>"}`，同步入库后返回整份列表：**库里新建了行 → 201，这部剧已存在（这次只更新）→ 200** |
| `/api/dramas/{id}/episodes` | GET | 剧详情页的集分块：`{"id","merge_chunk_size","webdav_path","chunks":[{"ep_no","from","to","count","play_id","play_url","episodes":[{"ep_no","title","duration_sec"}]}]}`。块边界与对外集号只走 `store.ListEmbyItems`（内部 `deriveEmbyItems`），播放 id 只由后端签名；id 非法 400、剧不存在 404 |
| `/api/dramas/{id}/cover` | GET | 海报字节：命中 → 200 + `image/*` + 精确 `Content-Length` + `Cache-Control`；没有海报或上游失败 → 404（不 500）。与 WebDAV 的 `poster.jpg` 共用同一个 CoverCache |
| `/api/refresh` | POST | 手动跑一轮：`{"source":"web"}` → 202 已启动 / 404 无此源或未启用 / 409 该源正在跑（与定时共用单飞锁） |
| `/api/status` | GET | 同步计数 + 运行配置（`strm_prefix` / `cache_dir` / `merge_chunk_size` / `cache_max_mb` / `cache_lru_days` / `refresh_interval_sec` / `refresh_batch` / `refresh_timeout_sec` 现读；密钥只报 `*_set`；`admin_token_set` 报写操作能不能用）+ 各播放源状态（含调度字段 `running` / `last_run_at` / `last_ok_at` / `last_error` / `interval_sec` / `next_due_at`；构造期项旁的 `base_url_effective` / `request_timeout_sec_effective` 是启动时注入、正在生效的值，`restart_required` 标出「改了还没重启」）+ `jellyfin` 块 = 媒体库维护现状（`configured` / `missing[]` / `url` / `library_root` / `library_prefix` / `api_key_set` / `timeout_sec` / `running` / `last_run_at` / `last_ok_at` / `last_error` / `created` / `unchanged` / `removed` / `duration_ms` / `uncertain` / `failed[]` / `delete_failed[]` / `disabled[]` / `orphans[]` / `conflicts[]`，运行中另有 `started_at` / `phase` / `phase_label` / `processed` / `total` 进度字段；**api key 永不出现在响应里**，只给 `api_key_set`） |
| `/api/settings` | PATCH | 按 key 批量写配置：`{"settings":{"<key>":"<原始值>",…}}` → 200 + **归一化后的值**（`strm_prefix` 去尾 `/`；`webdav_pass` / `stream_secret` 回 `null`，不回显）。未知键 / 非法值 → 400 且**整批一行不写**；`{"settings":{}}` → 400。值一律写成字符串（数字/布尔也写成 `"4096"` / `"false"`），校验只有服务端那一份。写操作要令牌，见下「写操作鉴权」 |
| `/api/jellyfin/libraries` | POST | 手动跑一轮媒体库同步（一分类一库，ADR-0012）：无请求体参数 → 202 `{"started":true}` 已派出 / 409 上一轮还在跑（与同步轮末钩子共用单飞锁）/ 400 三项配置没配齐（body 点名缺哪个键）。删不删库由分类开关决定（MKACG-668）：这一轮读到哪个分类关着就删它对应的自管库，没有 `prune` 参数；结果不在响应里，看下面的 GET |
| `/api/jellyfin/libraries` | GET | 一轮媒体库同步的现状与进度（MKACG-664），页面在派发后**每秒轮询它**直到 `running:false`：运行中给 `started_at` / `phase` / `phase_label`（`list` 列现有库 / `categories` 列分类 / `diff` 比对 / `create` 建库 / `delete` 删已关闭分类的库 / `scan` 扫描）/ `processed` / `total`；收尾后给 `created` / `unchanged`（已存在且未改动）/ `removed` / `duration_ms` / `delete_failed[]` / `last_error`。与 `/api/status` 的 `jellyfin` 块同一份视图，但**不读 drama / episode 表**，所以轮询用它 |
| `/api/jellyfin/categories` | GET | 全部分类 + 开关状态（MKACG-668）：`{"categories":[{"name":"都市","enabled":true},…]}`。分类口径与建库同源（vfs 根目录），开关来自 `jellyfin_category_switch` 表（行不存在 = 启用）；未配置 Jellyfin 时也可读 |
| `/api/jellyfin/categories` | PATCH | 开关一个分类：`{"name":"都市","enabled":false}` → 200 回显同值 / 400 `name` 为空、缺 `enabled` 或 body 非法。**只写库，不碰 Jellyfin**：删除发生在下一轮同步（周期或「同步媒体库」）里 |

- **「增剧目」的唯一入口是数据源同步**：剧目与集的真值在上游，手工插空剧只会得到一条
  永远没有集的记录。`series_id` 只接受纯数字或 `/player/<id>` 链接 —— 归一后的值会拼进
  上游路径，放宽字符集等于把路径注入带进来。
- **201 / 200 按「库里有没有新行」分**（MKACG-623）：重复 `POST` 同一个 `series_id` 时
  `store.UpsertDrama` 只更新那一行、不新增，此时回 `201 Created` 等于对调用方说假话。
  判断先查一次 `DramaByProviderID(syncer.Name(), sid)`，`store.ErrNotFound` 即新建；
  判断留在本包，不下沉到 store（那是要整体重写的层）。
- **同步失败统一 502**（数据源在远端，失败几乎总是上游/页面结构问题），错误原因进响应体；
  provider 的错误文本不含直链与签名。
- **业务规则不复制到本包**：分块规则（连续切片、单集/合并集判定、uid 形态）全部在
  `internal/store/merge.go`；settings 的非法值走 `store.ErrInvalidSetting`，由
  `mapSettingErr` → 400。
- **计数不做 N+1**：`store.ListDramaStats` 一条聚合查询取全库（标量子查询，不用多表
  JOIN，避免行数相乘）。

### 写操作鉴权（MKACG-622）

`/api/` 的**读**操作维持无鉴权（单用户本机管理面；Emby 与页面都靠 GET 浏览），**写**操作
（非 `GET` / `HEAD` / `OPTIONS` 即算写，含 `PUT`）一律要令牌 —— 在此之前 `POST /api/dramas` 能触发同步，
加写配置端点会同时放大这个风险。

| 项 | 口径 |
|---|---|
| 令牌来源 | env `DRAMA_HUB_ADMIN_TOKEN`（**不是** settings 键：它保护的就是「改 settings」这个动作，放进 settings 会变成改配置不需要令牌） |
| 传法 | 请求头 `X-Admin-Token: <token>` |
| 未设令牌 | 所有写端点 **401（fail closed）**：没配 ≠ 不设防，否则新部署会悄悄裸奔。页面显示「未设置 `DRAMA_HUB_ADMIN_TOKEN`，后台改配置不可用」并禁用按钮；`/api/status` 的 `admin_token_set` 报有没有配（不报值） |
| 令牌不符 / 缺失 | 401，`message` 说明缺哪个请求头 |
| 范围 | 按「方法 + `/api/` 前缀」判定（`Server.ServeHTTP` 里的闸门）：新增写端点默认受保护，不需要逐个登记 |
| 不受影响 | `GET /api/*`、单页界面、`/webdav/`（Basic 认证）、`/video/`（HMAC 签名）各按自己的口径 |
| 页面怎么带 | 顶栏只有「写操作 · 未解锁」按钮，点开原生 `<dialog id=token-dialog>` 填 `#admin-token`（MKACG-675）；确认后存 `sessionStorage`（关掉标签页就没了），`app.js` 对非 GET 请求自动加这个头；任一写请求 401 会自动打开弹窗并显示服务端原因。令牌不进任何页面响应、不进日志 |

### Jellyfin 媒体库（ADR-0012）

一分类一库：`internal/jellyfin` 按「虚拟文件树根目录列出的分类目录名」建库
（`name = <jellyfin_library_prefix><分类>`、`collectionType = tvshows`、
`paths = <jellyfin_library_root>/<分类>`）。理由是 Jellyfin 只把库根的直接子目录认作剧集；
库根挂在挂载根上时分类会变成 Series、剧目录退化成普通目录（ADR-0012 的实测数据）。

- **自管集合 = 三条同时成立**：名字带前缀、`Locations` 只有一个、那个位置正好是 root 的
  直接子目录。判定在 `jellyfin.Diff`（纯函数，表驱动测试覆盖三类「不算自管」）。
- **开关即删除开关**（MKACG-668）：页面一行一个分类的开关，默认全开；关掉的分类不建库，
  已有的自管库在下一轮同步里删（`Plan.Delete`），拨开关本身不发请求。删除失败进
  `delete_failed[]` 与 `last_error`、下一轮重试；目标库已不存在算这一项完成（幂等）。
  分类从树里消失且没有开关行，仍然只进 `orphans[]`（概览卡片 + 日志），不删。
- **触发**：手动端点 + `scheduler.runRound` 的轮末钩子（`afterRound`，`saveState` 之后、
  一轮一次）。两者共用一把单飞锁；`Start` 只派 goroutine（`context.WithoutCancel`），
  不延长同步轮的寿命。
- **整轮预算随请求数走**：`jellyfin_timeout_sec × (待发请求数 + 1)`（`roundBudget`）。
  固定成 `2 × timeout` 时，58 个分类的部署点一次只建 15 个就整轮 `context deadline
  exceeded`，要连点 3–4 次才收敛 —— 这是默认配置下的必现故障，不是极端情况。
- **单点失败不中止整轮**：建库超时（`uncertain`，Jellyfin 侧结果未知）与 4xx 被拒
  （`failed`，实测 12.1 库路径不存在回 400 `The specified path does not exist`）都只记账
  跳过，继续建其余分类；只有连不上 / 5xx / 认证失败这类系统性错误才整轮中止。一个坏分类
  不该让另外几十个都建不出来。
- **失败隔离**：Jellyfin 的任何失败只写内存里的 `last_error`，不写 `source_state`、不影响
  `/healthz`、WebDAV 与播放。
- **扫描**：`created > 0 || removed > 0 || uncertain > 0` 才在末尾调一次
  `POST /Library/Refresh`；0 变化时一个写请求都不发（幂等的可验证点）。
- **状态只在内存**：`created` / `uncertain` / `failed` / `last_*` / 孤儿库不落库，重启后
  回到「未运行」。唯一的落库状态是分类开关（`jellyfin_category_switch`，MKACG-668）：
  关掉的分类必须跨重启保持，否则重启后下一轮同步会把库悄悄重建。
- **与 MKACG-651 的合入顺序**：库内「剧目录成为 Series」以 651（去掉分片层）为前提 ——
  分片层还在时库根的直接子目录是分片目录，Jellyfin 会把 `00` 认成 Series、真剧目录仍是
  普通目录（12.1 实测复现）。654 对 `internal/vfs` 的增量只有 `categories.go` 一个文件，
  刻意不与 651 大改的 `tree.go` 同文件，两条分支因此可任意顺序干净合入。
- **分类口径只有一处**：`vfs.Tree.CategoryDirs` 复用根目录列举（转义 + 「其他」兜底），
  `internal/jellyfin` 不 import `vfs`、不读 `drama.genre`。

### 缓存面（ADR-0007）

**缓存由本服务自己负责**（`docs/adr/0007-own-hls-and-local-cache.md` 推翻 ADR-0004 的划界）：
海报与分片落 `<cache_dir>` 的两个子目录（分片是「每集一片」的 `.m4s`），按 `cache_max_mb` 与
`cache_lru_days` 淘汰，键、布局、并发与失效规则的真值是 `docs/CACHE-MODEL.md`，参数
默认值与取值范围见本节「配置」表。淘汰循环在装配根启动（`cache.RunEviction`，启动即
一轮、之后每 5 分钟一轮），与请求路径无关，间隔是固定值、不做成可调项。

过期淘汰对三种 kind **不是同一套动作**：分片与索引「删文件 + 删行」，海报「删文件、留行」
（行的 `path`/`bytes`/`sha256` 清空，语义见 `docs/CACHE-MODEL.md`「淘汰」第 3 步）。海报
那条例外就是 `poster.jpg` 的存在性判据：行在 = 剧目录里有这张虚拟文件；删了行请求根本到不了
`Render`，「淘汰后按需回源」不成立（MKACG-650）。

`strm_prefix` 与缓存**无关**：它只是 `/video/{id}.m3u8` 的对外地址前缀。播放字节一律
由本服务的 `/video/…` 产出并经本服务的缓存，rclone 只剩 WebDAV 只读挂载，**不再需要
`--vfs-cache-mode full`**（它的缓存以前是给播放用的）。

`/api/status` 不再有 `cache` 段（MKACG-616 删除）：那份 rclone 建议参数（`mode` /
`max_size` / `max_age` / `flags` / `note`）不参与本服务任何逻辑，是本节的已知偏差
（MKACG-610 记录、MKACG-617 复核），留着等于让页面显示不生效的配置。缓存相关配置
（`cache_dir` / `cache_max_mb` / `cache_lru_days`）在响应的 `config` 段里，都是现读值。

## 提交

`<类型>(<范围>): <祈使句>`，正文说明「为什么」而不是「改了什么」。范围用包名（`stream`、`vfs`、`store`）。格式化与重命名单独提交。

## 运行环境（容器，验收依据）

- **验收一律跑容器**：`make test`（`--network=none`）、`make run`；全量门禁是 `./scripts/ci.sh`。本机 `make build` / `make lint` 只用于快速迭代，不是验收依据 —— 开发机 ffmpeg 与容器 ffmpeg 版本不同，产物会不一致。
- **ffmpeg 版本由镜像决定**：alpine + `apk add ffmpeg` = 6.1.2。改动 `Containerfile` 或 `env.lock` 属于版本变更，必须同时更新 `env.lock` 并跑 `make env-check`。
- **测试禁止访问外网**：`make test` / `scripts/ci.sh` 用 `--network=none` 强制。假上游一律用容器内 `httptest`。
- 新引入的外部进程（rclone 等）必须有**固定 digest 的镜像**，不能依赖本机 PATH。
- 分片转封装在容器内调用 `ffmpeg`，路径统一解析为 `ffmpeg`（在 PATH），不写死绝对路径。
- `docs/TEST-STANDARDS.md` 的「禁止真实外网/真实时间」在容器里同样生效，容器不改变测试编写规则。

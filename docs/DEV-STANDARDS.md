# 开发规范

适用于 `drama-hub` 全部 Go 代码。与 `AGENTS.md` 的硬约束冲突时，以 `AGENTS.md` 为准。

## 依赖与技术选型

| 项 | 选择 | 理由 |
|---|---|---|
| Go | `go 1.27`（开发机 go1.27.2） | 与 CI 一致 |
| 模块 | `github.com/justforlxz/drama-hub` | |
| 数据库 | `modernc.org/sqlite`（纯 Go） | 免 cgo，交叉编译单二进制 |
| WebDAV | `golang.org/x/net/webdav` | 只需要 FileSystem 接口 |
| HTTP | 标准库 `net/http` + 1.22 路由模式 | 不引框架 |
| 日志 | `log/slog`（JSON handler） | |
| 模板/前端 | `html/template` + 原生 JS，`embed` 进二进制 | 无构建步骤 |
| 进程 | `ffmpeg` / `ffprobe` 外部依赖 | 不内嵌二进制 |

**不引入**：ORM、Web 框架、依赖注入容器、前端框架、第二个 SQLite 驱动、任何需要 cgo 的库。

新增任何第三方依赖，先在 `docs/adr/` 写一条 ADR 说明「为什么标准库不够」，否则打回。

## 错误处理

- 所有错误带上下文：`fmt.Errorf("加载剧集 %s: %w", id, err)`。
- 哨兵错误定义在包内并导出：`var ErrNotFound = errors.New(...)`；调用方用 `errors.Is` / `errors.As` 判断，禁止比较错误字符串。
- 业务错误不强求堆栈；跨层返回时只包装一次语义，避免 `"失败: 失败: 失败"`。
- `panic` 只允许出现在 `main` 的启动装配阶段；请求路径里任何 panic 视为缺陷（recover 中间件只做兜底 + 记录，不吞掉不记录）。
- 出站 HTTP 一律 `http.Client{Timeout: ...}` 或 `ctx` 超时；没有超时的出站调用视为缺陷。

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

- 迁移文件 `internal/store/migrations/NNNN_<verb>_<subject>.sql`，只向前，不改历史文件。启动时自动执行到最新。
- 所有 SQL 只在 `internal/store` 出现；其它包通过方法访问。SQL 字符串拼接一律用参数占位。
- 连接参数固定：`journal_mode=WAL`、`busy_timeout=5000`、`foreign_keys=ON`。
- 派生的 `emby_item` 表由 `store.RebuildEmbyItems(dramaID)` 幂等重建，调用点只有同步完成（`internal/sync` 的 `Syncer.SyncSeries`）、合并变更（`CreateMergeGroup` / `DeleteMergeGroup`）与首播回填时长（`store.BackfillEpisodeDuration`，事务内直接调 `rebuildEmbyItems`）三处。重建是「整体替换」：先删该剧全部行再按源集号顺序插入，对外集号从 1 起连续；单集占 1 个集号、合并组也只占 1 个。
- 合并编排只在 `internal/store/merge.go`：`merge_group`（组）+ `merge_item`（组内成员，`ord` 即时间轴顺序）+ `emby_item`（派生）。`CreateMergeGroup` 校验成员至少 2 集、同剧、不重复、不与已有组重叠（违反返回 `ErrInvalid`，不落库）。
- 时间统一存 UTC 的 RFC3339 文本；展示时区转换在 Web 层做。
- store 用哨兵错误表达结果：查询/删除无行返回 `ErrNotFound`，唯一/主键约束冲突由 `wrapWriteErr` 统一映射为 `ErrConflict`（均可 `errors.Is` 判断），不向调用方泄漏裸 `*sqlite.Error`。
- `drama` / `episode` 的写入一律 upsert：`drama` 以 `(provider, provider_id)`、`episode` 以 `(drama_id, ep_no)` 为幂等身份，冲突时只更新可变业务列，身份列与入参 `id` 被忽略，返回行 id。其中 `episode.duration_sec` 只在插件给了非零值时覆盖，否则保留库里已有的值 —— 上游批量拿不到时长（恒 0），首播建索引回填的实测值不能被下一轮刷新清零。
- 删剧走 `drama → episode` 的 `ON DELETE CASCADE` 连带删集（依赖 `foreign_keys=ON`，由 `Open` 的 DSN 施加），不影响其它剧目；`segment` 经 `episode` 级联一并删除。
- `segment` 表是「每集的分片索引」，由 `internal/segment` 索引任务整体重建：`ReplaceSegments` 一事务内先删后插（重复索引幂等、失败整体回滚），`ListSegments` 按 `start_sec` 升序（即片内序）；无行返回空切片而非 `ErrNotFound`（「还没索引」是正常状态，由分片端点按 503 处理）。
- 跨行写入用 `Store.InTx`：同一事务内执行、失败整体回滚。事务内 `Store` 复用同一批读写方法；取数/网络请求必须在事务之外完成（SQLite 单写者，长事务持锁）。

## 配置

> 2.0 的全局 key 已全部落地：`cache_dir` 随 S3（MKACG-608），`cache_max_mb` /
> `cache_lru_days` 随 S7（MKACG-611，范围见 `docs/CACHE-MODEL.md`「淘汰」），刷新
> 调度的三项（`refresh_interval_sec` / `refresh_batch` / `refresh_timeout_sec`）与
> `plugin.<源>.refresh_interval_sec` 随 S4（MKACG-606，范围见
> `docs/SCHEDULER-MODEL.md`「配置」）。
>
> 2.0 明确不做缓存预热（MKACG-603 的「不在范围」表），1.0 预留的 `seg_prefetch` /
> `pin_days` 两个死键已删除（MKACG-616）：它们从没有消费者，留着只会让人以为改了有用。
> 库里若还有这两行，`Settings` 会按 `ErrUnknownSetting` 忽略它们，不需要数据迁移。

**生效方式只有两类，配置表逐键标注**：

- **现读**：每次使用都查库（`Settings` 本身不缓存，硬约束 5）——调度器每次醒来重读
  `refresh_interval_sec` / `refresh_batch` / `refresh_timeout_sec` /
  `plugin.<源>.enabled` / `plugin.<源>.refresh_interval_sec`（改完最多 60 秒按新值算
  下一轮），请求路径现读 `strm_prefix` / `stream_secret` / `webdav_user` /
  `webdav_pass` / `seg_target_sec`，缓存每次读写现读 `cache_dir`、每轮淘汰现读
  `cache_max_mb` / `cache_lru_days`（淘汰循环每 5 分钟一轮）。都不需要重启。
- **需重启**：`plugin.<源>.base_url` 与 `plugin.<源>.request_timeout_sec` 是构造期注入的
  不可变配置（`*http.Client` 的 Timeout 在这里定型，见 `docs/PLUGIN-CONTRACT.md` 硬性
  要求 2）。状态页对这两项同时给「settings 现值」（`base_url` / `request_timeout_sec`）
  与「启动时注入、正在生效的值」（`*_effective`），两者不一致时 `restart_required=true`；
  页面直接写「重启后生效」，不再出现「显示新值、跑的还是旧值」。

运行时配置只有一张表 `settings(key TEXT PRIMARY KEY, value_json TEXT, updated_at TEXT)`。

启动引导只读环境变量三项：`DRAMA_HUB_DB`、`DRAMA_HUB_ADDR`、`DRAMA_HUB_LOG_LEVEL`。
其余全部走 settings。

**首次启动的 env 引导（MKACG-616）**：下面四个 env 只在 settings 里**还没有该键时**写入
一次（`Settings.SetIfAbsent`），已有值一律不覆盖 —— 重启不会把手改过的 settings 覆盖回
env；env 缺省或值非法（secret 不足 32 字节、用户/密码为空）时只记一条 warn、不写坏值、
不 panic（看板上显示「未设置」就是这条原因）。2.0 没有 `PUT /api/settings`，这是空卷上
唯一能种出必填项的入口。

| env | settings 键 | 说明 |
|---|---|---|
| `DRAMA_HUB_STREAM_SECRET` | `stream_secret` | 空卷没有它 `/video` 恒 500 |
| `DRAMA_HUB_CACHE_DIR` | `cache_dir` | 镜像里默认 `/data/cache`（`WORKDIR /data`，相对默认值会落成 `/data/data/cache`）；裸机默认仍是 `./data/cache` |

**WebDAV 账号密码不走 env**（lxz 2026-10-31 定）：`webdav_user` / `webdav_pass` 是后台配置项，
由 MKACG-622 的配置写入接口管理；env 再种一份就会出现两个真值，所以这两个 env 不存在，
设了也会按「未知变量」报出来。

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
| `strm_prefix` | `""` | URL 或空 | 现读（每次渲染 `.strm`） | `.strm` 里 `/video/{id}.m3u8` 前面的**对外地址前缀**（本服务自己的公开地址；反向代理/自定义端口时填，尾 `/` 写入时归一掉）；空 = 按请求 `Host` 推导绝对地址（见上「`.strm` 的绝对地址」）。不指向 rclone/openlist |
| `stream_secret` | 必填 | ≥ 32 字节 | 现读（每请求） | 出流 HMAC 签名密钥（见 ADR-0003）；换了之后所有旧 URL 失效 |
| `seg_target_sec` | `10` | 4–30 | 现读（每次建索引） | 分片目标时长；**只对还没建索引的 uid 生效**——已建索引的 uid 不重算，改了要等它下一次重建（`segment` 表被整体替换时） |
| `webdav_user` / `webdav_pass` | 必填 | — | 现读（每请求） | WebDAV Basic 认证 |
| `cache_dir` | `./data/cache`（镜像 `/data/cache`） | 非空路径 | 现读（每次读写） | 本地缓存根目录：海报 `cover/`、分片 `seg/`、索引 `index/`，临时文件 `tmp/`（`docs/CACHE-MODEL.md`）；四个目录在启动时预先建出 |
| `cache_max_mb` | `8192` | 0–1048576 | 现读（每轮淘汰） | 分片缓存上限（MB）；`0` = 不按大小淘汰 |
| `cache_lru_days` | `30` | 1–365 | 现读（每轮淘汰） | 超过 N 天未命中的缓存条目（三种 kind 都算）删除 |
| `plugin.<源>.enabled` | `true` | — | 现读（每轮调度） | 是否参与**刷新调度**（缺失按 `true`）；手动同步（`POST /api/dramas`）与播放（`/video`）不受影响 |
| `plugin.<源>.base_url` | `""` | URL 或空 | **需重启**（构造期注入） | 源地址；空 = 用插件内置默认（红果 `https://hongguoduanju.com`） |
| `plugin.<源>.request_timeout_sec` | `30` | 5–300 | **需重启**（构造期注入） | 单次出站请求超时 |
| `refresh_interval_sec` | `3600` | 300–86400 | 现读（每轮调度） | 全局刷新间隔，一轮到期 = `last_run_at` + 本值 |
| `plugin.<源>.refresh_interval_sec` | 空 = 继承全局 | 300–86400 | 现读（每轮调度） | 源级间隔覆盖（用 `Settings.OptionalInt` 读，无行即「继承」） |
| `refresh_batch` | `1000` | 0–10000 | 现读（每轮调度） | 每轮存量刷新的部数；`0` = 关闭存量轮转 |
| `refresh_timeout_sec` | `900` | 60–86400 | 现读（每轮调度） | 单轮预算（`context.WithTimeout`） |

`<源>` 是 `Plugin.Name()`（红果是 `web`），键的格式与校验在 `internal/store`
（`PluginSettingKey` + `specFor` 的前缀模式），调用方不自己拼字符串。

`strm_prefix` 的尾 `/`（B2）：`{strm_prefix}/video/…` 是字符串拼接，库里存着
`http://host/stream/` 会产出 `//video/`（MKACG-617 实测）。**写入时归一化**（`parseSetting`
对 URL 类键 `TrimRight`，状态页显示的就是归一后的值），**渲染时再去一次**（存量手改的行
也能拼对）；非法值照旧报错，不猜。

`plugin.<源>.request_timeout_sec` 的 5–300 范围校验已进注册表，但 2.0 没有 settings
写接口，所以非法值目前只能由代码侧（`Settings.Set` / `Settings.SetIfAbsent`）拒绝，
没有 HTTP 400 的入口；写接口属后续阶段。

缓存淘汰由本服务自己执行（ADR-0007）：`internal/cache` 的淘汰循环每 5 分钟一轮，
先按 `cache_lru_days` 删过期，再按 `cache_max_mb` 删最久未用的分片（`docs/CACHE-MODEL.md`
「淘汰」）。淘汰间隔是固定值，不做成可调项。
ffmpeg 并发上限固定为 `min(4, max(1, NumCPU/2))`，不做成可调项（见 `internal/segment` 的闸注释）。

新增参数必须同 PR 更新上表，并在 `settings` 里给默认值 + 范围校验。写非法值返回 400，不静默纠正。

## 播放面（`/video`）

契约真值：`docs/PLAYBACK-CONTRACT.md`（S8 / MKACG-612 落地）。本节只写实现口径。
两个口子由 `internal/segment` 提供 handler、装配根注册：

| 端点 | 方法 | 说明 |
|---|---|---|
| `/video/{id}.m3u8` | GET / HEAD | 播放列表；单集与合并集**同一路径形态** |
| `/video/{id}/{seq}.ts` | GET / HEAD | MPEG-TS 分片：查本地缓存 → 未命中现切、落缓存、再回 |

- `/dav/`、`/s/{token}/*` 与 `/s/*/master.m3u8` **已退役、不留兼容层**：`.strm` 是按
  请求渲染的，下一次读取就是新内容，没有存量文件需要兼容。
- `internal/stream`（rank 20）只剩两件事：`ID` / `ParseID`（对外 id 的 HMAC 签名）与
  `Resolver`（请求时向插件解析直链）。它不碰 HTTP —— 状态码映射在
  `internal/segment`。

### id 规则

```
id  = uid + "-" + hex(HMAC-SHA256(stream_secret, uid))[:12]
uid ∈ { "e<episode.id>", "m<merge_group.id>" }
```

- 生产方是 vfs 的 `.strm` 渲染（`cmd` 注入 `stream.ID`），校验方是 `/video`
  （`stream.ParseID`）：两侧同一个实现，不各写一份 HMAC 拼接。
- `uid` 只依赖 `episode.id` / `merge_group.id`，两者在库的整个生命周期里不变 →
  URL 不随同步、重建派生表、重启、改目录而漂；换 `stream_secret` 后所有 URL 失效
  （ADR-0003 的既定后果）。secret 每请求读一次，不缓存（硬约束 5）。
- **播放单元的合法性由 `emby_item` 派生表判定**（`store.EmbyItemByUID`）：`e<id>` 只在
  它是一条现存单集时有效；某集被并进合并组后，它的 `e<id>` 立即 404 —— 这是契约
  「不飘移」第 3 条，也是 401（签名不对）与 404（播放单元不存在）的分界。
- 状态码矩阵里没有 302、没有过期时间、没有 token 表。

### 关键帧索引（按需建立）

`ensureSegments` 在**第一次播放某个 uid** 时同步 `ffprobe -skip_frame nokey` 预扫源，
把切点按 `seg_target_sec` 对齐到关键帧后整体写入 `segment` 表：

- **切点规则**：首片从 0 起；之后每个切点取第一个 `>= 当前切点 + seg_target_sec`
  的关键帧。切点都落在关键帧上，`-c copy` 切片内部才不会漂移（实测「目标 10 s
  实际 10.66 s」就是这条规则）。
- **总时长**：Σ `dur` == 源时长（±一帧）。`Timeline` 把有序的各集分片拼成连续 seq
  空间，`seq → (集号, 集内秒, 绝对起点)` 供合并集 O(1) 定位。
- **按需、不预建**：库里有 2847 万集，全库 ffprobe 不可完成，代码里没有任何批量
  预建入口（索引只在请求路径上按需建立）。
- **30 s 硬上限**：整次建立（`indexDeadline`）走一个 `context.WithTimeout`，超时回
  503 + `Retry-After: 5`（`httpx.ErrUnavailable`），不出半截时间轴。
- **幂等与失败**：`ReplaceSegments` 一事务内先删后插；探测失败不写任何行（库里保留
  旧索引或保持为空）。
- **时长回填**：建索引时 `ffprobe` 已经拿到源时长，因此顺手回填 `episode.duration_sec`
  （`store.BackfillEpisodeDuration`：只填 0、不覆盖非零值，非正时长视为探测失败），并在
  同一事务内重建该剧 `emby_item` —— `runtime_sec` 单集取本集时长、合并集取组内之和，
  只有一个派生实现。`.nfo` 的 `<runtime>` 取 `runtime_sec / 60`，回填后当次即为真实值；
  没建过索引的集仍是 0，`.nfo` 照常渲染（`<runtime>0</runtime>`，Emby 视为未知）。
  回填排在写索引之前：若反序，索引写成功而回填失败时下次请求会命中已有索引直接返回，
  时长就再也补不上。

### 状态码矩阵

| 情形 | `/video/{id}.m3u8` | `/video/{id}/{seq}.ts` |
|---|---|---|
| 正常 | 200 + `application/vnd.apple.mpegurl` | 200 + `video/mp2t` + `Content-Length` + `Cache-Control` |
| id 签名不对 / 缺 `-` | 401（`httpx.ErrUnauthorized`） | 401 |
| 路径不是 `.m3u8` | 404 | — |
| `uid` 不在派生表 / seq 越界 / 分片名非法 | 404（`httpx.ErrNotFound`） | 404 |
| 解析不到直链（`ErrNoStream` 等） | 502（`httpx.ErrUpstream`） | 502 |
| 索引建立超时 | 503 + `Retry-After: 5` | 503 + `Retry-After: 5` |
| secret 读失败 | 500（响应体与日志都不含 secret） | 500 |

- **直链只在请求时解析**（`stream.Resolver.DirectURL`）：不落库（0004 已删
  `episode.play_url` / `play_url_at`）、不缓存、不跨请求复用 —— 上游换链后下一次请求
  自然拿到新链。解析失败一律 502，且不写库、不写缓存。
- playlist 是 `segment` 表的**纯函数视图**（`BuildPlaylist`）：不落盘、不缓存
  （理由见 `docs/CACHE-MODEL.md`「为什么 m3u8 文本不缓存」）。
- HEAD 与 GET 的 `Content-Length` 一致（硬约束 3）：playlist 手写长度；分片走
  `http.ServeContent`（Range 也由它处理，分片是完整不可变对象，不需要分段生成）。
- 分片字节先完整收进内存再生效（单片 10 s 约 2–5 MB）：ffmpeg 中途失败时客户端看到
  干净的 502，不是截断流。

### 分片缓存

`GET /video/{id}/{seq}.ts` 的顺序是「查缓存 → 命中即回；未命中生成、落盘、再回」：

- 键 `sha256(uid \x1f seq \x1f codec_sig)`（`cache.SegmentKey`），落
  `<cache_dir>/seg/<hash 前 2 位>/<hash>.ts`；`artifact(kind='segment')` 是索引行。
- 并发同一个 `(id, seq)` 只生成一次（`Cache.GetOrGenerate` 的 single-flight）；
  **返回的 `[]byte` 是共享只读**，调用方不得就地改写。
- 生成失败 → 502，**不写缓存**（失败结果绝不进缓存）；缓存自身故障按 500 报，
  不冒充上游问题。
- 淘汰归 `internal/cache`（每 5 分钟一轮，`cache_max_mb` / `cache_lru_days`，
  `docs/CACHE-MODEL.md`）。
- ffmpeg/ffprobe 一律带硬超时（`timeoutProbe=30s` / `timeoutCut=60s`）；stderr 尾部
  只进日志，不落响应体；日志只记源 host，不落完整直链。并发闸
  `min(4, max(1, NumCPU/2))`，超出的请求排队不丢弃。
- **没有预取**：2.0 不做缓存预热（MKACG-603「不在范围」），1.0 的两个占位键
  `seg_prefetch` / `pin_days` 已删除（MKACG-616），代码里没有预取循环、没有整集预热。

### 合并集

`uid = m<group_id>`：`Timeline` 按 `merge_item.ord` 把成员分片拼成连续 seq 空间，
playlist 在集边界插 `#EXT-X-DISCONTINUITY`；切片的 `-output_ts_offset` 用绝对起点保证
跨集 DTS 连续（否则播放器在集边界因时间戳回退丢帧）。分片端点只解析它要切的那一集，
不把整组都解析一遍。

## 日志

- `slog` 结构化字段，键用小写下划线：`drama_id`、`ep_no`、`uid`、`seq`、`duration_ms`、`upstream_status`。
- **禁止记录**：完整带签名的播放直链、token 明文、WebDAV 密码。需要定位时记 `vid` + `host` + 路径前 32 字符。
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

插件只做「上游字节 → 领域值」：不写库、不开事务、不保存状态（不存游标、不缓存直链）。
写库、事务、`emby_item` 重建在 `internal/sync`；刷新调度与游标在 `internal/scheduler`
（S4，MKACG-606）：本包只提供「按一个作品写一次库」这个幂等动作，调度器按源调它。

### 红果插件的上游成本

| 方法 | 上游 | 请求数 |
|---|---|---|
| `Work(ctx, ref)` | `GET {BaseURL}/player/{id}` | 1（剧目元数据 + 全部集） |
| `Resolve(ctx, ref, epNo)` | `GET /player/{id}` → `GET /player/{id}/{vid}` | 2 |
| `Enumerate(ctx, cur)` | 目录索引：`index.xml` + 29 份子 sitemap + `seo-important.xml` + `/category` 34 页 | 64（每份子 sitemap 前重取一次 4 KB 索引）= 30s 级 |

`Resolve` 是 2 个请求：契约入参只有集号，集号 → 源侧集标识（`vid`）只能从详情页取，
而插件不许保存状态；要压到 1 个必须改接口（把源侧集标识传进来）。`docs/PLUGIN-CONTRACT.md`
能力矩阵写的「红果：1 个请求 / 集」与 `Resolve` 签名不自洽，实现按签名走。
`Enumerate` 一页 = 一份上游文档（子 sitemap 整份返回，43,556 个 loc 一次读完），
三段串行：子 sitemap（29）→ `seo-important.xml` → `/category` 第 1..34 页。并集才是全集：
只用 sitemap 漏最新 53 部，只用 `/category` 只剩 800 部（`docs/L1-CORPUS-SCOPE.md`）。
游标 `v1:<段>:<位置>` 只记「哪一段、第几份/第几页」，可从任意游标续跑；认不出的游标按
`ErrSchema` 打回（静默从头开始会把「游标坏了」伪装成「枚举走完了」）。

- `BaseURL` 默认 `https://hongguoduanju.com`，配置键 `plugin.web.base_url`（空 = 用它）。
  它是唯一为测试开的缝（`--network=none` 下指向 `httptest.Server`）。`*http.Client`
  由装配根按 `plugin.web.request_timeout_sec` 构造（默认 30s）。
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
- 框架校验（`internal/sync`）：集表非空、`Episodes[i].No == i+1`、源侧集标识数量与集数一致，
  违反即 `ErrSchema` 且**不写库**（这是插件实现 bug，不是上游问题）。
- `episode.vid` 的来源不在契约的 `Episode` 里（只有集号 / 标题 / 时长）：框架用消费方定义的
  可选接口 `sync.WorkWithEpisodeKeys` —— 插件实现就一次调用同时拿到详情与每集源侧标识，
  不实现则 `episode.vid` 留空（不编造）。红果实现了它。

### 直链不落库的契约（0004 之后）

**库里没有直链列。** `0004_drop_play_url.sql` 删掉了 `episode.play_url` /
`play_url_at`：直链是请求时向插件解析的短时凭据，持久化等于把过期链接写进库。

实测（2026-10-29，9 部剧逐一 ffprobe）：列表页 `vid_list` 给全全部集，但单集页 SSR 的
`contentUrl` 只有当集为第 1 集时有值，且下载到的 MP4 **实际时长恒为 10.08 秒**（JSON-LD
却声明整集时长）。1.0 因此从不写这两列，读它们的出流端点（`/s/{token}/e/{uid}`）在真实
库上恒 502 —— 那条断链在 S8 直接接在 `/video` 上：**每次播放请求现调
`plugin.Resolve`**（`internal/stream` 的 `Resolver`），失败一律 502、不写库不缓存。

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
    └── <分片>/                        ← fnv32a(provider_id) % 128 的两位十六进制（00–7f）
        └── <剧名> (<年份>)/
            ├── tvshow.nfo
            └── Season 01/
                ├── <剧名> - S01E01 - <标签>.strm
                └── <剧名> - S01E01 - <标签>.nfo
```

- 顶层只有分类目录，没有 `全部/`：一部剧只出现在它自己那一个分类下。
- 分片是纯函数（用 `provider_id` 而非标题，剧改名、重扫都不换分片），只为把每个目录的
  子项数摊到 PROPFIND 能列出来的量级。`N = store.ShardCount`（128）是代码常量，
  不是 schema：改 N 只是重算 `drama.shard` 这一列的数据（`TestShardRecountKeepsSchema`）。
- 兜底名「其他」同时覆盖 `genre` 为空与上游真的叫「其他」两种取值，它们并进同一目录。
- 同名冲突（同一个分片目录内基准名重复）：所有冲突成员改用
  `<基准名> [<provider>-<provider_id>]`，后缀是 `provider_id` 原文、不截断；不冲突的
  成员保持基准名。
- 剧目录 `<剧名> (<年份>)`：年份来自 `drama.year`；`year=0` 显式写成 `(<0>)`，不省略。
- 季目录恒为 `Season 01`（`emby_item.season` 恒为 1）。
- 集号 `S%02dE%02d`：**对外集号取 `emby_item.ep_no`**（硬约束 6），不是 `episode.ep_no`。
- `<标签>` = `embyLabel`：单集取源集标题、空则 `第<对外集号>集`；**合并集取
  `第 A-B 集`**（A/B 是组内最小/最大源集号，A==B 时退化为 `第 A 集`）。
- 目录名（含分类目录）与文件名里的 `/ \ : * ? " < > |` 一律替换为 `_`（Windows + WebDAV
  非法字符集）：分类名转义是 S8 / PL 2026-10-30 的裁决，见下「含路径分隔符的分类名」。
- 路径以 `/` 分隔、相对 vfs 根、无前导 `/`；`""` 与 `短剧` 都表示根。

**数据源是 `emby_item` 派生表**（`store.ListEmbyItems`），不是 `episode`：一个合并组
在树里只产出 1 条文件对，Emby 因此显示为 1 集、对外集号连续（硬约束 6）。合并调整
只重建派生表，`drama` / `episode` 原始数据不动。

### 列举成本（一个目录一次查询）

`List(ctx, dir)` 只返回**一层**直接子项，每次常数条 SQL，与全库规模无关：

| `dir` | 子项 | 查询 |
|---|---|---|
| `""`（根） | 分类目录 | 1 次 `SELECT DISTINCT genre`（覆盖索引全扫，唯一 O(行数) 的一条） |
| `<分类>` | 分片目录 `00`–`7f` | 1–2 次 genre 存在性点查（`GenreExists`），分片集合是代码常量、不查库 |
| `<分类>/<分片>` | 剧目录 | 1–2 次 genre 存在性点查 + 1 次 `WHERE genre IN (…) AND shard = ? ORDER BY title` |
| `<剧目录>` | `tvshow.nfo` + `Season 01` | 1 次剧目点查（目录名被 sanitize 时再加 1–2 次 genre 存在性点查） |
| `<剧目录>/Season 01` | 集文件对 | 同上 + 1 次 `ListEmbyItems` |

两类查询都走索引（`idx_drama_genre_shard_title` / `idx_drama_title`），由
`internal/store` 的 `TestDirectoryQueryPlans` 用 `EXPLAIN QUERY PLAN` 钉住。
**不存在「枚举整棵树」的路径**：旧实现每次请求 1 次 `ListDramas` + 每剧 1 次
`ListEmbyItems`，在 L1 规模（60 万部 × 46.8 集）下不可能完成。

两个实现选择（契约允许、此处留档）：

- **分片目录恒回 `00`–`7f` 全部 128 个**（契约原文「分类目录 1 次 `DISTINCT shard`
  （或直接回 `00`–`7f`）」取了后者）。`DISTINCT shard` 要扫该分类的全部索引行
  （183k 行约 20–40ms，随分类规模线性增长），而分片集合本来就是
  `store.ShardCount`；空分片列出来是空目录，列它不发生查询。
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
后缀的形态按 `WHERE provider = ? AND provider_id = ?` 点查；命中后**仍比对「分类 + 分片 +
目录名」**，不一致即 `ErrNotFound`（防止其他分片/分类的同名剧被渲染到本路径下）。
标题含 `/ : * ?` 等字符时目录名是 sanitize 过的、点查必然不中，此时退到「本分片内比对」
（`WHERE genre IN (…) AND shard = ?`，索引区间查询，不是全表扫）。

### strm 形态

一行 URL + 尾 `\n`，前缀取 `settings.strm_prefix`：

```
{strm_prefix}/video/{id}.m3u8
```

- `id = uid + "-" + hex(HMAC-SHA256(stream_secret, uid))[:12]`（`stream.ID`），
  `uid` 取 `emby_item.uid`：单集 `e<episode.id>`、合并集 `m<merge_group.id>`。
  **单集与合并集没有区别**，对外统一是 HLS 播放列表（`docs/PLAYBACK-CONTRACT.md`）。
- 签名算法在 `internal/stream`（rank 20）：vfs（rank 30）可以 import 它，但仍在装配根
  注入 `vfs.IDFunc`（`cmd` 传 `stream.ID`），让 vfs 只依赖「一个纯函数」这条缝。
- 播放面（`/video/{id}.m3u8` + `/video/{id}/{seq}.ts`）见本节前面的「播放面」章。
- `strm_prefix` 与 `stream_secret` 每次渲染现读，改值即时生效（硬约束 5）。

### NFO 形态

`encoding/xml` 序列化，固定头 `<?xml version="1.0" encoding="UTF-8" standalone="yes" ?>` +
两空格缩进体 + 尾换行。XML 转义交给编码器。

| 文件 | 根元素 | 字段 | 说明 |
|---|---|---|---|
| 集 `.nfo` | `episodedetails` | `title` / `season` / `episode` / `runtime` | 字段全部取自 `emby_item`：`episode` 是**对外集号**，`runtime` 单位是**分钟**（`runtime_sec / 60` 向下取整）；写秒会被 Emby 放大 60 倍 |
| `tvshow.nfo` | `tvshow` | `title` / `year` / `plot` | `plot` 来自 `drama.plot` |

`aired` 不写：库里的时间只有采集时间，没有播出日期。

### 长度

渲染结果的长度只有 `len(Render 的 []byte)` 一个真值，本包不提供独立的 `Size()` 或长度字段。
`internal/webdav` 的 `PROPFIND getcontentlength` 与播放面的 `Content-Length`
都取这个值（硬约束 3）。

## WebDAV（internal/webdav）

挂载点 `/webdav/`（`webdav.MountPath`），只读。协议实现委托 `golang.org/x/net/webdav`，
本包只提供两件事：`x/net/webdav.FileSystem` 适配（`fs.go`）与 Basic 认证（`handler.go`）。
路径与目录形状见 `docs/DIRECTORY-CONTRACT.md`。

### 路径映射

**DAV 根 = vfs 根目录**（`vfs.Root`）：`/` → `短剧`，`/爱情/0a/剧名 (2024)/…` →
`短剧/爱情/0a/剧名 (2024)/…`。DAV 路径里没有 `短剧` 这一段，根目录名来自 `vfs.Root`，
不在本包复制字面量。

因此 `rclone lsf --max-depth 5 dramahub:/` 的层级是 分类 → 分片 → 剧 → Season 01 →
strm/nfo（`docs/ACCEPTANCE.md` P2）：Emby 库路径 `rclone mount dramahub:/ /mnt/dramahub`
→ `/mnt/dramahub`（一个「剧集」库，库根 `=` DAV 根）。

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
- 不做封面 `poster.jpg`（属 artifact 缓存，单独任务）。
- **单目录成本随子项数线性增长，季层不摊薄**：`walkFS` 对一次 `PROPFIND` 的每个子项都要取
  props，所以一个 1000 集的季（2000 个子项）就是 2000 次渲染 + 一份大 XML。MKACG-610
  验收在 17,558 部真库上实测：该季 `Season 01` 的 `Depth: 1` = **1.8 MB / 25.9 s**（单请求）。
  分片摊薄的是分类层（20 万级）；季层只能靠「单季集数量级本身有限」兜住（当前库实测最大单季
  1000 集，代码里没有任何集数上限常量），没有更强的摊薄手段——再切一层就改变了 Emby 认的季结构。

### 测试（两层，都不需要外部 rclone）

**单元**（`fs_unit_test.go`）：`fakeTree` 按目录给出子项与渲染字节，**不碰 SQLite、不起
HTTP**。覆盖：DAV 根 = vfs 根、长度只来自 `len(渲染字节)`、`os.IsNotExist` 认得的
`*fs.PathError` 包装、目录打开惰性（`OpenFile` 不查子项、`Readdir` 才查）、`Readdir`
直接子项且只查该目录、只读语义、请求内快照（同目录只 `List` 一次、同路径只渲染一次、
不跨请求复用）、列举/渲染错误透传。

**集成**（`webdav_test.go`）：真 SQLite + 真 handler（`httptest`），验证协议契约。
客户端游览（等同 `rclone lsf --max-depth 5`、Emby 扫库的请求序列）由进程内的递归
PROPFIND 客户端完成，不依赖外部二进制、不起守护进程、不落盘：

- `TestPropfindDepth1_Layers`：`Depth: 1` 逐层只有该层节点（根只有分类、分类只有分片、
  分片只有剧目录、剧目录是 `tvshow.nfo` + `Season 01`），且 DAV 路径里没有 `短剧` / `全部`。
- `TestWalkTree_Complete`：max-depth 5 枚举出完整树（分类 → 分片 → 剧 → Season 01 →
  strm/nfo），并对每个文件项断言 `getcontentlength == GET 实际 body 长度`。
- `TestWalkTree_DepthBounded`：遍历在预算内结束、深度不超过 max-depth。
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

挂载点 `/`（装配根注册）。三页界面与管理 API 是同一个 handler：完整路径原样传入
本包（ServeMux 不剥前缀），未登记路径由本包 mux 返回 404 —— 「未知路径 → 404」
的语义不变，只是兜底者从顶层 mux 换成 web 的 mux。`/webdav/`、`/s/` 的 pattern 比
`/` 更具体，不受影响。

### 页面

| 路径 | 页面 | 能干的事 |
|---|---|---|
| `/` | 剧目 | 按上游剧 id 或 `/player/<id>` 链接同步入库；删剧目（级联删集、合并组、对外集号） |
| `/merges` | 合并编排 | 选剧 → 按想要的播放顺序点集 → 建组（点击顺序即 `ord`）→ 删组 |
| `/status` | 同步与缓存 | 同步计数与最近同步时间、各播放源状态（是否启用、正在刷新、上次运行/上次成功/上次错误/下次计划、生效的 `base_url` 与超时、停用原因）、手动「立即刷新」、运行配置现状（密钥只报「是否已设置」）、缓存面（`mode` / `prefix` / `max_size` / `max_age` / `flags`）——**这几个字段仍是 ADR-0004 遗留**，与缓存实现相反，见「缓存面」的已知偏差 |

模板 `templates/*.html` 与 `static/app.css`、`static/app.js` 用 `//go:embed` 进二进制：
界面没有构建步骤、没有 `package.json`（ADR-0002）。页面 JS 只调下面这些 API，渲染结果
与 API 返回一一对应；页面响应同样给出精确 `Content-Length`（渲染成字节 → 再写响应）。

### 管理 API

| 端点 | 方法 | 说明 |
|---|---|---|
| `/api/dramas` | GET | 剧目列表：drama 行 + `episodes` / `playable_episodes` / `emby_items` / `merge_groups` 计数 |
| `/api/dramas` | POST | 新增剧目：`{"series_id":"<上游剧 id 或 /player/ 链接>"}`，同步入库后返回 201 + 整份列表 |
| `/api/dramas/{id}` | DELETE | 删剧目（级联删集与合并组），204；不存在 404 |
| `/api/dramas/{id}/episodes` | GET | 该剧全部集（`ep_no` 升序）；只回库里的字段（直链不落库，没有 `has_play_url`） |
| `/api/dramas/{id}/merge-groups` | GET | 该剧合并组与成员（`ord` 即时间轴顺序） |
| `/api/dramas/{id}/merge-groups` | POST | 建组：`{"label":"…","episode_ids":[…]}`，数组顺序即 `ord`，201 + 整份列表 |
| `/api/merge-groups/{id}` | DELETE | 删组（同一事务内重建派生表），204 |
| `/api/refresh` | POST | 手动跑一轮：`{"source":"web"}` → 202 已启动 / 404 无此源或未启用 / 409 该源正在跑（与定时共用单飞锁） |
| `/api/status` | GET | 同步计数 + 运行配置（`strm_prefix` / `seg_target_sec` / `cache_dir` / `cache_max_mb` / `cache_lru_days` 现读；密钥只报 `*_set`）+ 各播放源状态（含调度字段 `running` / `last_run_at` / `last_ok_at` / `last_error` / `interval_sec` / `next_due_at`；构造期项旁的 `base_url_effective` / `request_timeout_sec_effective` 是启动时注入、正在生效的值，`restart_required` 标出「改了还没重启」） |

- **「增剧目」的唯一入口是数据源同步**：剧目与集的真值在上游，手工插空剧只会得到一条
  永远没有集的记录。`series_id` 只接受纯数字或 `/player/<id>` 链接 —— 归一后的值会拼进
  上游路径，放宽字符集等于把路径注入带进来。
- **同步失败统一 502**（数据源在远端，失败几乎总是上游/页面结构问题），错误原因进响应体；
  provider 的错误文本不含直链与签名。
- **业务规则不复制到本包**：合并编排的「至少 2 集、同剧、不重复、不与已有组重叠」全部由
  `store.CreateMergeGroup` 校验，本包只做 `store.ErrInvalid` → 400、`store.ErrNotFound`
  → 404 的映射。
- **计数不做 N+1**：`store.ListDramaStats` 一条聚合查询取全库（标量子查询，不用多表
  JOIN，避免行数相乘）。

### 缓存面（ADR-0007）

**缓存由本服务自己负责**（`docs/adr/0007-own-hls-and-local-cache.md` 推翻 ADR-0004 的划界）：
海报、分片、关键帧索引落 `<cache_dir>` 的三个子目录，按 `cache_max_mb` 与
`cache_lru_days` 淘汰，键、布局、并发与失效规则的真值是 `docs/CACHE-MODEL.md`，参数
默认值与取值范围见本节「配置」表。淘汰循环在装配根启动（`cache.RunEviction`，启动即
一轮、之后每 5 分钟一轮），与请求路径无关，间隔是固定值、不做成可调项。

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

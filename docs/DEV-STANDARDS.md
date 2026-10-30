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
- 派生的 `emby_item` 表由 `store.RebuildEmbyItems(dramaID)` 幂等重建，调用点只有同步完成（`provider.SyncSeries`）与合并变更（`CreateMergeGroup` / `DeleteMergeGroup`）两处。重建是「整体替换」：先删该剧全部行再按源集号顺序插入，对外集号从 1 起连续；单集占 1 个集号、合并组也只占 1 个。
- 合并编排只在 `internal/store/merge.go`：`merge_group`（组）+ `merge_item`（组内成员，`ord` 即时间轴顺序）+ `emby_item`（派生）。`CreateMergeGroup` 校验成员至少 2 集、同剧、不重复、不与已有组重叠（违反返回 `ErrInvalid`，不落库）。
- 时间统一存 UTC 的 RFC3339 文本；展示时区转换在 Web 层做。
- store 用哨兵错误表达结果：查询/删除无行返回 `ErrNotFound`，唯一/主键约束冲突由 `wrapWriteErr` 统一映射为 `ErrConflict`（均可 `errors.Is` 判断），不向调用方泄漏裸 `*sqlite.Error`。
- `drama` / `episode` 的写入一律 upsert：`drama` 以 `(provider, provider_id)`、`episode` 以 `(drama_id, ep_no)` 为幂等身份，冲突时只更新可变业务列，身份列与入参 `id` 被忽略，返回行 id。
- 删剧走 `drama → episode` 的 `ON DELETE CASCADE` 连带删集（依赖 `foreign_keys=ON`，由 `Open` 的 DSN 施加），不影响其它剧目；`segment` 经 `episode` 级联一并删除。
- `segment` 表是「每集的分片索引」，由 `internal/segment` 索引任务整体重建：`ReplaceSegments` 一事务内先删后插（重复索引幂等、失败整体回滚），`ListSegments` 按 `start_sec` 升序（即片内序）；无行返回空切片而非 `ErrNotFound`（「还没索引」是正常状态，由分片端点按 503 处理）。
- 跨行写入用 `Store.InTx`：同一事务内执行、失败整体回滚。事务内 `Store` 复用同一批读写方法；取数/网络请求必须在事务之外完成（SQLite 单写者，长事务持锁）。

## 配置

运行时配置只有一张表 `settings(key TEXT PRIMARY KEY, value_json TEXT, updated_at TEXT)`。

启动引导只读环境变量三项：`DRAMA_HUB_DB`、`DRAMA_HUB_ADDR`、`DRAMA_HUB_LOG_LEVEL`。其余全部走 settings。

写入路径只有 `Settings.Set`（进程内），**没有 HTTP 写入口**：管理面 `/api/status` 只读。
因此首次部署与改配置要直写 `settings` 表，运维侧写法与校验方式见 `docs/OPS.md` §2.4。

| key | 默认 | 取值范围 | 作用 |
|---|---|---|---|
| `strm_prefix` | `""` | URL 或空 | strm 内 URL 前缀，指向 rclone/openlist 代理 |
| `stream_secret` | 必填 | ≥ 32 字节 | 出流 HMAC 签名密钥（见 ADR-0003） |
| `seg_target_sec` | `10` | 4–30 | 分片目标时长 |
| `seg_prefetch` | `2` | 0–10 | 预取分片数 |
| `pin_days` | `7` | 0–90 | 最近 N 天播放过的合并集整集预取 |
| `webdav_user` / `webdav_pass` | 必填 | — | WebDAV Basic 认证 |

**不属于本服务运行配置**（由缓存代理负责，见 ADR-0004）：缓存上限、LRU 天数。
它们只在 WebUI/文档生成 rclone 配置建议值时出现。**本服务不实现任何缓存淘汰**。
ffmpeg 并发上限固定为 `min(4, max(1, NumCPU/2))`，不做成可调项（见 `internal/segment` 的闸注释）。

新增参数必须同 PR 更新上表，并在 `settings` 里给默认值 + 范围校验。写非法值返回 400，不静默纠正。

## 出流（stream）

模块 `internal/stream` 与 `internal/segment`（合并集部分），装配在 `/s/` 前缀下：

| 端点 | 方法 | 说明 |
|---|---|---|
| `/s/{token}/e/{uid}` | GET / HEAD | 单集 MP4 字节中继，Range 透传；**uid 是合并集句柄时返回自造 m3u8**（M7，由装配根按 uid 分派） |
| `/s/{token}/e/{uid}/master.m3u8` | GET | 合并集 playlist 的标准别名（部分客户端会自行追加） |
| `/s/{token}/seg/{uid}/{seq}.ts` | GET | 分片：单集按片内序、合并集按跨集时间轴（M6/M7），MPEG-TS |
| `/s/` 子树其余路径 | 任意 | 501（已规划未实现） |

`GET` 路由模式同时匹配 `HEAD`（Go 1.22 ServeMux 行为），不单独注册；`HEAD` 与
`GET` 对同一 URL 的上游响应必须给出同一 `Content-Length`，且 `HEAD` 不读上游 body。

### uid 规则

- 单集 `uid = "e" + strconv.FormatInt(episode.id, 10)`（如 `e42`），派生自稳定的
  `episode.id`，不读 `emby_item`：那是幂等重建的派生表，而签名 URL 的缓存键必须
  长期稳定。
- 合并集 `uid = "m" + strconv.FormatInt(merge_group.id, 10)`（如 `m3`）。**合并集
  uid 不得匹配 `^e[0-9]+$`**（DESIGN D3：uid 区分内容类型，装配根靠前缀分派）。
- 单集 uid 只由 stream 消费；合并集 uid 的存续由 `emby_item` 派生表的 rebuild 保证
  （`merge_group.id` 稳定，组删除后派生表重建、uid 不再出现）。

uid 只是出流 URL 的句柄，与硬约束 6（对外集号只由 `emby_item` 决定）无关；
stream 不读、不写 `emby_item`。

### 合并集（M7）

合并集 = 虚拟 HLS 分片聚合（ADR-0001，AGENTS 硬约束 2），实现分三层：

1. **编排**（`internal/store/merge.go`）：`CreateMergeGroup` / `DeleteMergeGroup` /
   `RebuildEmbyItems` 维护 `merge_group` / `merge_item` / `emby_item` 三张表。
   合并变更与同步完成是派生表重建的两个调用点（硬约束 6：对外集号只由派生表
   决定，合并调整只重建派生表、不动 `drama` / `episode`）。
2. **时间轴**（`internal/segment/timeline.go` + `merged.go`）：`NewTimeline` 按成员
   顺序把各集分片拼成连续 seq 空间，回答 `seq → (集号, 集内秒, 绝对起点)`。
3. **端点**：playlist 渲染 `BuildPlaylist`（`#EXT-X-DISCONTINUITY` 插在集边界），
   分片按时间轴解析后 `-c copy` 现切；切片的 `-output_ts_offset` 用绝对起点保证
   跨集 DTS 连续（否则播放器在集边界因时间戳回退丢帧）。

错误矩阵与单集一致：索引未就绪 503 + `Retry-After`（不出半截流，ADR-0001）；
坏签名 401；未知组/越界 seq 404。

### 鉴权

```
token = base64url_nopad( HMAC-SHA256( secret, "stream:" + uid ) )
```

`stream.Sign` / `stream.Verify` 按 ADR-0003 逐字节实现，编码固定
`base64.RawURLEncoding`（43 字符、无 `=`），比较用 `crypto/hmac.Equal`。
secret 取 `settings.stream_secret`，**每请求读一次不缓存**（硬约束 5：改完即时
生效；换 secret 后旧 URL 立即失效，是 ADR-0003 的既定后果）。无过期、无状态、
无 token 表。

### 状态码矩阵

| 情形 | 响应 |
|---|---|
| 上游 200 | 200 + `Content-Length` + `Content-Type` + `Accept-Ranges`，body 透传 |
| 上游 206 | 206 + 上述 + `Content-Range`，body 透传 |
| 上游 416 | 416 原样透传（含上游的 `Content-Range`，若给了） |
| 上游其它 4xx / 5xx（含 403、404） | 502（`httpx.ErrUpstream`），**不转发上游 body** |
| 传输错误 / 超时 | 502 |
| secret 读失败 | 500（响应体与日志都不含 secret） |
| 签名不匹配 | 401（`httpx.ErrUnauthorized`） |
| uid 非法 / 剧集不存在 | 404（`httpx.ErrNotFound`） |
| `play_url` 为空 | 502（`httpx.ErrUpstream`） |
| 客户端断开（ctx 取消） | 不写响应，仅记日志 |

- **Range 纯透传，不在本地合成 206 / 416**：两者都由上游判定（实测红果 CDN
  都正确）。请求的 `Range` 头原样转发给上游。
- **不做 302**（硬约束 7）：Go 默认 client 在内部跟随上游重定向（≤10 次）并
  保留 `Range`；任何 3xx 都不会发给客户端。
- 回传给客户端的头**只有** `Content-Type`、`Content-Length`、`Content-Range`、
  `Accept-Ranges`；其余上游头一律丢弃。
- **先拿到上游响应并定下状态码，再写任何客户端字节**：所有上游失败都产出干净的
  502，不会先写 200 再失败。
- **上游必须给出精确 `Content-Length`**：chunked（无长度）响应让 HEAD 与 GET
  拿不到一致长度，直接打掉「长度精确」——此时返回 502，不给无长度的 200/206。

### 出站客户端

```go
&http.Client{Transport: &http.Transport{
    DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
    ResponseHeaderTimeout: 15 * time.Second,
    DisableCompression:    true,
}}
```

- **`DisableCompression: true` 是硬要求**：Go 默认 Transport 会给无 `Range` 的
  GET 自动加 `Accept-Encoding: gzip` 并透明解压，把响应里的 `Content-Length`
  抹掉 → 「长度精确」直接失效（本地 httptest 对照实验实测）。
- **禁止 `Client.Timeout`**：那是对整个响应体（含长视频）计时，会截断大文件。
  连不上 / 不回头由 `DialContext` 与 `ResponseHeaderTimeout` 覆盖，body 传输交给
  请求 ctx（客户端断开即取消），仍满足「出站 HTTP 一律带 timeout 或 ctx 超时」。
- 客户端实例挂在 `Server` 字段上，禁止包级可变变量。

### 已知边界

上游在 body 传输**中途**失败时响应头已发出，无法改写成 502，客户端看到截断。
不为此引入全量缓冲（缓冲整集字节是错误设计）。

## 分片（segment）

模块 `internal/segment`，装配在 `/s/` 子树下：`segment.Cutter` 用比 `/s/` 更具体的
pattern `GET /s/{token}/seg/{uid}/{file}` 挂在顶层 mux 上，`file` 形如 `<seq>.ts`
（seq 从 0 起、允许前导零；缓存代理按整串 URL 做 key，本服务不改写）。签名校验用
装配根注入的 `stream.Verify`（segment 与 stream 同 rank 40，不能互相 import）。

### 关键帧索引

`Cutter.Index(episodeID)` 用 `ffprobe -skip_frame nokey` 预扫源，把切点按目标时长
对齐到关键帧后整体写入 `segment` 表：

- **切点规则**：首片从 0 起；之后每个切点取第一个 `>= 当前切点 + seg_target_sec`
  的关键帧。切点都落在关键帧上，`-c copy` 切片内部才不会漂移（实测「目标 10s
  实际 10.66s」就是这条规则）。
- **总时长**：Σ `dur` == 源时长（±一帧）。`Timeline` 把有序的各集分片拼成连续
  seq 空间，`seq → (集号, 集内秒)` 供合并集（M7）O(1) 定位。
- **幂等与失败**：`ReplaceSegments` 一事务内先删后插，重复索引幂等；探测或切片
  失败**不写库、不缓存失败结果**（库里保留旧索引或保持为空）。
- **触发点**：索引不在请求路径上自动触发（ADR-0001：索引缺失一律 503）。调用
  `Cutter.Index` 的任务属 M7（合并编排）与 R1（管理面）。

### 分片端点状态码矩阵

| 情形 | 响应 |
|---|---|
| 正常切片 | 200 + `Content-Type: video/mp2t` + `Content-Length` + `Cache-Control` |
| 签名不匹配 | 401（`httpx.ErrUnauthorized`） |
| uid 非法 / 剧集不存在 / seq 越界 / 文件名非法 | 404（`httpx.ErrNotFound`） |
| `play_url` 为空、ffprobe/ffmpeg 失败 | 502（`httpx.ErrUpstream`），不缓存失败结果 |
| 索引未就绪（`segment` 表无该集行） | 503 + `Retry-After: 5`（`httpx.ErrUnavailable`），不出半截流 |

- 切片把完整 TS 字节收进内存再写响应（单片 10s 约 2–5 MB，DESIGN §2），ffmpeg
  中途失败时客户端看到干净的 502 而非截断流。
- ffmpeg/ffprobe 一律带硬超时（`timeoutProbe=30s` / `timeoutCut=60s`）；stderr 尾部
  只进日志，不落响应体；日志只记源 host，不落完整直链（可能带签名）。
- 并发闸：ffmpeg/ffprobe 调用排队过 `min(4, max(1, NumCPU/2))` 的闸，超出的请求
  排队不丢弃；客户端断开时放弃排队并结束请求。

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
| `path` | string | `r.URL.Path` 经 `httpx.redactPath` 掩码 |
| `status` | int | 响应状态码（未显式 `WriteHeader` 时 200）|
| `duration_ms` | int | 处理耗时（毫秒）|

级别按状态：5xx → Error，4xx → Warn，其余 Info。

**掩码规则**：出流路径形的 token 在路径段里（ADR-0003：`/s/{token}/e/{uid}`、
`/s/{token}/seg/{uid}/{seq}.ts`，cache-key 稳定性要求它可见），因此以 `/s/` 开头的路径，
把紧随其后的一段替换为 `{token}`（`/s/SECRET/e/42` → `/s/{token}/e/42`）。日志只记
`r.URL.Path`，天然不含 query，`?token=…` 这类参数不会落盘。

中间件只允许挂一次：在 `web` / `stream` / `webdav` 内再挂一层会让同一请求产生两条日志。


## 数据源（provider）

`internal/provider` 是官网 SSR 数据源（`provider` / `source` 一律写 `"web"`）。两个入口，各自的出站请求数固定：

| 入口 | 上游 | 请求数 | 副作用 |
|---|---|---|---|
| `Source.SyncSeries(ctx, st, sid)` | `GET {BaseURL}/player/{sid}` | 1 | 一个事务写 1 部剧 + 全部集 |
| `Source.FetchDirectURL(ctx, sid, vid)` | `GET {BaseURL}/player/{sid}/{vid}` | 1 | 不写库（读时解析） |

- `BaseURL` 默认 `https://hongguoduanju.com`，是唯一为测试开的缝（`--network=none` 下指向 `httptest.Server`）。空 `*http.Client` → 默认 15s 超时客户端；出站一律带 `ctx` + `Client.Timeout`。
- 响应体经 `io.LimitReader` 读，上限 4 MiB；超限按 `ErrParse`（页面异常放大，不是可重试的传输错误）。
- 解析：`_ROUTER_DATA` 后的 JSON 用标准库 `json.Decoder` 从 `{` 起解（不手写括号扫描）；JSON-LD 用 `regexp` 取出 `<script type="application/ld+json">` 块后按 `@type` 过滤。
- 校验（任一不满足即 `ErrParse`，**不写库**）：`isSuccess` 不为 `false`；页面的 `series_id` 与 `seriesDetail.series_id` 都等于请求 `sid`（被锁集会被上游 302 到别的集/别的剧页面）；`vid_list` 非空、元素全为数字、无重复。JSON-LD 块另做绑定校验：`url` 的 pathname 是 `/player/{sid}` 或 `/player/{sid}/{vid}`，且 `contentUrl` 的 pathname 含 `/{vid}`。
- `vid_list` 下标即集号（`ep_no = i+1`）。
- 哨兵：`ErrUpstream`（传输错误 / 非 200 / 空响应体）、`ErrParse`（结构变化 / 身份不符 / 超限）、`ErrNoDirectURL`（VideoObject 在但 `contentUrl` 缺失）。provider 不 import `internal/httpx`，状态码映射由边界包负责。

### `play_url` 留空的契约

**同步不写 `episode.play_url` / `play_url_at`。** 实测（2026-10-29，9 部剧逐一 ffprobe）：列表页 `vid_list` 给全全部集，但单集页 SSR 的 `contentUrl` 只有当集为第 1 集时有值，且下载到的 MP4 **实际时长恒为 10.08 秒**（JSON-LD 却声明整集时长 1m48s–3m38s），第 2..N 集 `contentUrl` 为空字符串、`publicVideoSeo: null`。把预览链写进 `play_url` 等于「整库每集只播 10 秒」，比留空更有害。

因此：同步只保证「剧与集齐全、`vid`/`ep_no` 正确」；`play_url` 为空表示**尚无可用播放源**。真直链按需走 `FetchDirectURL`，由消费者（出流）决定怎么用。整集字节来源属 M5 及之后的范围（见 MKACG-592 的 PL 决策项）。

### 测试

假官网一律是 `httptest.Server`：`Source.BaseURL` 指向它，`sync_at` 用注入时钟（`Source.now`），不依赖真实外网与真实时间。fixture 放 `internal/provider/testdata/html/`，裁剪自真实页面并在文件头注明来源、裁剪日期与覆盖场景。

## 代码组织

依赖方向单向：内部 import 仅允许 `rank(importer) > rank(importee)`；同级、反向、
未登记包一律失败，由 `scripts/check-deps.sh` 强制（`make lint` / `ci-lint.sh` 接入）。
新增包必须注册到 `scripts/check-deps.sh`。

| rank | 包 | 允许 import 的内部包 |
|---|---|---|
| 60 | `cmd/drama-hub` | 全部 internal |
| 50 | `internal/web` | 45 / 40 / 30 / 20 / 10 |
| 40 | `internal/webdav`、`internal/stream`、`internal/segment` | 30 / 20 / 10 |
| 30 | `internal/provider`、`internal/vfs`、`internal/httpx` | 20 / 10 |
| 20 | `internal/cache` | 10 |
| 10 | `internal/store` | 无 |

`internal/httpx` 是 HTTP 边界的横切层：只依赖标准库、不 import 任何 internal 包；
与它同 rank 30 的 `provider` / `vfs` 互不 import —— 上游/虚拟层不做 HTTP 状态分类。

- 一个文件一个关注点，超过 500 行先考虑拆分。
- 接口定义在消费方，只声明用到的方法；不预建「为将来准备」的接口。
- 构造函数注入依赖：`NewServer(store, settings, logger)`；不用全局单例、不用 `init()` 注册。
- 导出标识符必须有 doc 注释，说明契约与错误返回；私有函数只在「为什么」不显然时写一行注释。

## 虚拟文件树与渲染（internal/vfs）

树与文件内容全部按请求渲染，不落盘（AGENTS.md 硬约束 1）。`vfs.Tree` 只有两个入口：
`List(ctx)` 枚举全部目录与文件节点，`Render(ctx, path)` 返回文件的完整字节。

### 目录与命名（纯函数，`tree.go`）

```
短剧/
└── <剧名> (<年份>)/
    ├── tvshow.nfo
    └── Season 01/
        ├── <剧名> - S01E01 - <标签>.strm
        └── <剧名> - S01E01 - <标签>.nfo
```

- 剧目录 `<剧名> (<年份>)`：年份来自 `drama.year`；`year=0` 显式写成 `(<0>)`，不省略。
- 季目录恒为 `Season 01`（`emby_item.season` 恒为 1，1.0 单季）。
- 集号 `S%02dE%02d`：**对外集号取 `emby_item.ep_no`**（硬约束 6），不是 `episode.ep_no`。
- `<标签>` = `embyLabel`：单集取源集标题、空则 `第<对外集号>集`；**合并集取
  `第 A-B 集`**（A/B 是组内最小/最大源集号，A==B 时退化为 `第 A 集`）。
- 目录名与文件名里的 `/ \ : * ? " < > |` 一律替换为 `_`（Windows + WebDAV 非法字符集）。
- 路径以 `/` 分隔、相对 vfs 根、无前导 `/`；WebDAV 的绝对路径由 `internal/webdav` 映射。

**数据源是 `emby_item` 派生表**（`store.ListEmbyItems`），不是 `episode`：一个合并组
在树里只产出 1 条文件对，Emby 因此显示为 1 集、对外集号连续（硬约束 6）。合并调整
只重建派生表，`drama` / `episode` 原始数据不动。
### strm 形态

一行 URL + 尾 `\n`，前缀取 `settings.strm_prefix`：

```
{strm_prefix}/s/{token}/e/{uid}
```

- `uid` 取 `emby_item.uid`：单集 `e<episode.id>`、合并集 `m<merge_group.id>`（M7，
  与 stream / segment 的 uid 规则一致；派生表是唯一真值，硬约束 6）。
- `token` 由装配根注入的 `vfs.TokenFunc(secret, uid string) string` 计算，签名算法是
  `internal/stream` 的 ADR-0003 HMAC —— vfs（rank 30）不 import stream（rank 40）。
- `strm_prefix` 与 `stream_secret` 每次渲染现读，改值即时生效（硬约束 5）。

### NFO 形态

`encoding/xml` 序列化，固定头 `<?xml version="1.0" encoding="UTF-8" standalone="yes" ?>` +
两空格缩进体 + 尾换行。XML 转义交给编码器。

| 文件 | 根元素 | 字段 | 说明 |
|---|---|---|---|
| 集 `.nfo` | `episodedetails` | `title` / `season` / `episode` / `runtime` | 字段全部取自 `emby_item`：`episode` 是**对外集号**，`runtime` 单位是**分钟**（`runtime_sec / 60` 向下取整）；写秒会被 Emby 放大 60 倍 |
| `tvshow.nfo` | `tvshow` | `title` / `year` / `plot` | `plot` 来自 `drama.plot` |

`aired` 不写：`play_url_at` 是采集时间，不是播出日期。

### 长度

渲染结果的长度只有 `len(Render 的 []byte)` 一个真值，本包不提供独立的 `Size()` 或长度字段。
`internal/webdav` 的 `PROPFIND getcontentlength` 与 `internal/stream` 的 `Content-Length`
都取这个值（硬约束 3）。

### 查询成本
`Render` 是**常数次查询**、与全库规模无关：按目录名 `ListDramas` 匹配单部剧（1 次），
集级文件再取该剧的 `ListEmbyItems`（1 次，内部一次 `IN` 查询取单集源集、每组一次成员
查询）；`.strm` 另加两次现读设置。全树遍历（1 次 `ListDramas` + 每剧 1 次
`ListEmbyItems`）只属于 `List`，避免 Emby/rclone 逐个文件 GET 时把整库集表读一遍。

## WebDAV（internal/webdav）

挂载点 `/dav/`（`webdav.MountPath`），只读。协议实现委托 `golang.org/x/net/webdav`，
本包只提供两件事：`x/net/webdav.FileSystem` 适配（`fs.go`）与 Basic 认证（`handler.go`）。

### 路径映射

**DAV 根 = vfs 根目录**：`/` → `短剧`，`/剧名 (2024)/…` → `短剧/剧名 (2024)/…`。
根目录名从 `vfs.List` 现取（唯一不带 `/` 的目录节点），不在本包复制字面量。

因此 `rclone lsf --max-depth 3 dramahub:/` 的第一级就是剧目录（`docs/ACCEPTANCE.md` P2）：
Emby 库路径 `rclone mount dramahub:/ /mnt/dramahub` → `/mnt/dramahub`。

### 长度

`getcontentlength` 与 GET 的 `Content-Length` 都取 `len(vfs.Render 的字节)`（硬约束 3），
没有第二条长度来源。`PROPFIND` 里目录不带 `getcontentlength`（协议层对 collection 不适用）。

### 认证

Basic，凭据取 `settings.webdav_user` / `webdav_pass`，**每请求现读**（硬约束 5，改值即时生效）。
缺任一项或凭据不符 → `401` + `WWW-Authenticate: Basic`，响应体是 `httpx` 统一错误体
（`code: "unauthorized"`）。比较用 `subtle.ConstantTimeCompare`。

### 只读

`Mkdir` / `RemoveAll` / `Rename` 与任何带写意图的 `OpenFile` 一律失败
（`PUT`/`DELETE`/`MKCOL` → `405`）。锁定用 `x/net/webdav.MemLS`（协议层要求，不落盘）。

### 请求内快照

`x/net/webdav` 处理一个 `PROPFIND` 会对同一份数据反复调用 `Stat` / `OpenFile`（父目录一次、
每个子项再各一次）。逐次 `List` 会让一次目录列举变成 O(目录项数) 次全树查询，因此一次
请求内把 `List` 结果与渲染结果挂在请求 ctx 上（`snapshot`），每请求各算一次。**不跨请求**：
没有 TTL、没有失效逻辑，数据改了下一个请求即见（硬约束 5 同理）。

### 已知边界

- 修改时间恒为零值（`entry.ModTime`）：库里没有可靠的 mtime，`sync_at` / `play_url_at`
  是采集时间。零值让 ETag 稳定在「内容长度」上，`http.ServeContent` 也不写 `Last-Modified`。
- 不做封面 `poster.jpg`（属 artifact 缓存，单独任务）；不做合并集（M7）。

### 测试（两层，都不需要外部 rclone）

**单元**（`fs_unit_test.go`）：`fakeTree` 提供节点与渲染字节，**不碰 SQLite、不起
HTTP**。覆盖路径映射（DAV 根 = vfs 根目录）、长度只来自 `len(渲染字节)`、
`fs.ErrNotExist` 包装、`Readdir` 直接子项、只读语义、请求内快照（同请求只 `List`
一次、同路径只渲染一次、不跨请求复用）、渲染/列举错误透传。这几条守住适配逻辑本身。

**集成**（`webdav_test.go`）：真 SQLite + 真 handler（`httptest`），验证协议契约。
客户端游览（等同 `rclone lsf --max-depth 3`、Emby 扫库的请求序列）由进程内的递归
PROPFIND 客户端完成，不依赖外部二进制、不起守护进程、不落盘：

- `TestWalkTree_CompleteDepth3`：max-depth 3 枚举出完整树（剧 → Season 01 → strm/nfo），
  并对每个文件项断言 `getcontentlength == GET 实际 body 长度`。
- `TestWalkTree_DepthBounded`：遍历在预算内结束、深度不超过 max-depth。

消费方接口：`webdav.Tree`（`List` + `Render`）定义在 `fs.go`（接口在消费方，
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
的语义不变，只是兜底者从顶层 mux 换成 web 的 mux。`/dav/`、`/s/` 的 pattern 比
`/` 更具体，不受影响。

### 页面

| 路径 | 页面 | 能干的事 |
|---|---|---|
| `/` | 剧目 | 按上游剧 id 或 `/player/<id>` 链接同步入库；删剧目（级联删集、合并组、对外集号） |
| `/merges` | 合并编排 | 选剧 → 按想要的播放顺序点集 → 建组（点击顺序即 `ord`）→ 删组 |
| `/status` | 同步与缓存 | 同步计数与最近同步时间、运行配置现状（密钥只报「是否已设置」）、缓存代理建议参数 |

模板 `templates/*.html` 与 `static/app.css`、`static/app.js` 用 `//go:embed` 进二进制：
界面没有构建步骤、没有 `package.json`（ADR-0002）。页面 JS 只调下面这些 API，渲染结果
与 API 返回一一对应；页面响应同样给出精确 `Content-Length`（渲染成字节 → 再写响应）。

### 管理 API

| 端点 | 方法 | 说明 |
|---|---|---|
| `/api/dramas` | GET | 剧目列表：drama 行 + `episodes` / `playable_episodes` / `emby_items` / `merge_groups` 计数 |
| `/api/dramas` | POST | 新增剧目：`{"series_id":"<上游剧 id 或 /player/ 链接>"}`，同步入库后返回 201 + 整份列表 |
| `/api/dramas/{id}` | DELETE | 删剧目（级联删集与合并组），204；不存在 404 |
| `/api/dramas/{id}/episodes` | GET | 该剧全部集（`ep_no` 升序）；`has_play_url` 只报真假，不回传直链 |
| `/api/dramas/{id}/merge-groups` | GET | 该剧合并组与成员（`ord` 即时间轴顺序） |
| `/api/dramas/{id}/merge-groups` | POST | 建组：`{"label":"…","episode_ids":[…]}`，数组顺序即 `ord`，201 + 整份列表 |
| `/api/merge-groups/{id}` | DELETE | 删组（同一事务内重建派生表），204 |
| `/api/status` | GET | 同步计数 + 运行配置 + 缓存面 |

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

### 缓存面（ADR-0004）

视频缓存由缓存代理负责淘汰，本服务不实现淘汰，也不设缓存上限/保留期运行参数。看板只做
两件事：显示 `strm_prefix` 是否指向代理（空 = 直连，播放字节不经缓存），并给出
`--vfs-cache-mode full --vfs-cache-max-size 200G --vfs-cache-max-age 720h` 这份建议值
（取自设计基线 Q5，一次性输出、不参与运行逻辑）。改这两个数字属设计变更，不是配置调整。

## 提交

`<类型>(<范围>): <祈使句>`，正文说明「为什么」而不是「改了什么」。范围用包名（`stream`、`vfs`、`store`）。格式化与重命名单独提交。

## 运行环境（容器，验收依据）

- **验收一律跑容器**：`make test-e2e`（`--network=none`）、`make run-container`。本机 `make test` 只用于快速迭代，不是验收依据 —— 开发机 ffmpeg 与容器 ffmpeg 版本不同，产物会不一致。
- **ffmpeg 版本由镜像决定**：alpine + `apk add ffmpeg` = 6.1.2。改动 `Containerfile` 或 `env.lock` 属于版本变更，必须同时更新 `env.lock` 并跑 `make env-check`。
- **测试禁止访问外网**：`make test-e2e` 用 `--network=none` 强制。假上游一律用容器内 `httptest`。
- 新引入的外部进程（rclone 等）必须有**固定 digest 的镜像**，不能依赖本机 PATH。
- 分片转封装在容器内调用 `ffmpeg`，路径统一解析为 `ffmpeg`（在 PATH），不写死绝对路径。
- `docs/TEST-STANDARDS.md` 的「禁止真实外网/真实时间」在容器里同样生效，容器不改变测试编写规则。

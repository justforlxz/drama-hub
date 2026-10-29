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
- ffmpeg 调用统一过 `internal/segment` 的并发闸：默认 `min(4, NumCPU/2)`，可通过 `settings.ffmpeg_concurrency` 调整。超过上限的请求排队，不丢弃。
- 共享可变状态用 `sync.Mutex` 或 channel 二选一，不混用；禁止包级可变变量。

## 数据库

- 迁移文件 `internal/store/migrations/NNNN_<verb>_<subject>.sql`，只向前，不改历史文件。启动时自动执行到最新。
- 所有 SQL 只在 `internal/store` 出现；其它包通过方法访问。SQL 字符串拼接一律用参数占位。
- 连接参数固定：`journal_mode=WAL`、`busy_timeout=5000`、`foreign_keys=ON`。
- 派生的 `emby_item` 表由 `store.RebuildEmbyItems(dramaID)` 幂等重建，调用点只有同步完成与合并变更两处。
- 时间统一存 UTC 的 RFC3339 文本；展示时区转换在 Web 层做。

## 配置

运行时配置只有一张表 `settings(key TEXT PRIMARY KEY, value_json TEXT, updated_at TEXT)`。

启动引导只读环境变量三项：`DRAMA_HUB_DB`、`DRAMA_HUB_ADDR`、`DRAMA_HUB_LOG_LEVEL`。其余全部走 settings。

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
ffmpeg 并发上限固定为 `NumCPU`，不做成可调项（见 `internal/segment` 的并发闸）。

新增参数必须同 PR 更新上表，并在 `settings` 里给默认值 + 范围校验。写非法值返回 400，不静默纠正。

## 日志

- `slog` 结构化字段，键用小写下划线：`drama_id`、`ep_no`、`uid`、`seq`、`duration_ms`、`upstream_status`。
- **禁止记录**：完整带签名的播放直链、token 明文、WebDAV 密码。需要定位时记 `vid` + `host` + 路径前 32 字符。
- 每个请求一条入口日志（方法、路径、状态、耗时），出站上游一条（源、状态、字节数）。

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

## 提交

`<类型>(<范围>): <祈使句>`，正文说明「为什么」而不是「改了什么」。范围用包名（`stream`、`vfs`、`store`）。格式化与重命名单独提交。

## 运行环境（容器，验收依据）

- **验收一律跑容器**：`make test-e2e`（`--network=none`）、`make run-container`。本机 `make test` 只用于快速迭代，不是验收依据 —— 开发机 ffmpeg 与容器 ffmpeg 版本不同，产物会不一致。
- **ffmpeg 版本由镜像决定**：alpine + `apk add ffmpeg` = 6.1.2。改动 `Containerfile` 或 `env.lock` 属于版本变更，必须同时更新 `env.lock` 并跑 `make env-check`。
- **测试禁止访问外网**：`make test-e2e` 用 `--network=none` 强制。假上游一律用容器内 `httptest`。
- 新引入的外部进程（rclone 等）必须有**固定 digest 的镜像**，不能依赖本机 PATH。
- 分片转封装在容器内调用 `ffmpeg`，路径统一解析为 `ffmpeg`（在 PATH），不写死绝对路径。
- `docs/TEST-STANDARDS.md` 的「禁止真实外网/真实时间」在容器里同样生效，容器不改变测试编写规则。

# 插件契约

状态：定稿（2026-10-30，MKACG-604 / stage 1）
边界决策与被否决的方案见 `docs/adr/0006-framework-plugin-boundary.md`。
L1 口径（枚举到底有多少东西）见 `docs/L1-CORPUS-SCOPE.md`。

## 能力矩阵

| 能力 | 谁实现 | 对应方法 | 一次调用的上游成本 | 框架拿它做什么 |
|---|---|---|---|---|
| 目录发现 | 插件 | `Enumerate` | 红果：64 个请求 / 全量（30 份 sitemap + 34 页 `/category`），可分页 | 找出「源里有、库里没有」的作品 |
| 剧目详情 | 插件 | `Work` | 红果：1 个请求 / 部 | 写 `drama` 行 |
| 集列表 | 插件 | `Work`（同一返回值） | 0（与详情同一个响应） | 写 `episode` 行、重建 `emby_item` |
| 直链解析 | 插件 | `Resolve` | 红果：1 个请求 / 集 | `/video/{id}/{seq}.ts` 生成分片时按需取，不落库也不进缓存索引（`docs/PLAYBACK-CONTRACT.md`） |
| 刷新 | **框架** | — | — | 按 `docs/SCHEDULER-MODEL.md` 重新调用上面三个方法 |

两处刻意的合并 / 缺席，都不是留白：

- **剧目详情与集列表合成一个 `Work`。** 实测红果的 `GET /player/{id}` 一个响应里同时给出
  `seriesDetail`（名称、封面、简介、标签、集数）与 `vid_list`（全部集标识），
  `len(vid_list) == episode_cnt` 在 948 部样本上 100% 成立（`docs/L1-CORPUS-SCOPE.md` 取证 C）。
  拆成两个方法只会让每个插件把同一个响应解析两遍、或者框架对每部剧发两次请求。
- **没有 `Refresh` 方法。** 刷新是「框架重新调用 `Enumerate` / `Work` / `Resolve`」，
  不是插件的一个动作。给它一个 `Refresh` 方法等于把调度决策塞进插件，
  与 ADR-0006 第 4 条否决理由同源。

## Go 接口

```go
// Package plugin 定义播放源插件契约与注册表。
//
// 本包只依赖标准库：不认识 store / vfs / webdav / 调度器（ADR-0006）。
package plugin

import (
	"context"
	"errors"
	"time"
)

// 插件契约的哨兵错误。框架一律用 errors.Is 判定，不看错误文本。
var (
	// ErrUpstream：传输失败、非 2xx、读体失败、超时、空响应。可重试。
	ErrUpstream = errors.New("插件上游请求失败")
	// ErrRejected：上游限流或拒绝（429 / 403 / 风控页）。可重试，需退避。
	ErrRejected = errors.New("插件被上游拒绝")
	// ErrSchema：上游结构与契约不符（缺字段、身份校验失败、编码解不开）。不可重试。
	ErrSchema = errors.New("插件上游结构不符契约")
	// ErrNotFound：上游明确表示不存在该作品/该集。
	ErrNotFound = errors.New("上游不存在该对象")
	// ErrNoStream：作品/集存在，但没有可播放直链。这是正常状态，不是故障。
	ErrNoStream = errors.New("该集没有可播放直链")
)

// WorkRef 是插件作用域内的作品标识。
//
// 框架只把它当不透明键：不解析、不推断格式、不从中派生集号。
// 插件往里放什么（作品 id、集 id、URL 片段）由插件决定；对框架来说它就是一个字符串。
type WorkRef struct {
	Plugin string // 等于 Plugin.Name()；框架据此路由与查 source_state
	ID     string // 非空、在单个插件内唯一且稳定
}

// Work 是一部作品的元数据。
type Work struct {
	Ref         WorkRef
	Title       string
	Year        int       // 0 = 上游未声明
	Plot        string
	CoverURL    string    // 上游封面地址；空 = 未声明。本地资产由框架落 artifact
	Genres      []string  // 上游声明顺序；Genres[0] 是主类型；空 = 未声明
	EpisodeDecl int       // 上游声明的总集数；0 = 未声明
	UpdatedAt   time.Time // 上游最后可见时间；零值 = 未声明
}

// Episode 是一集的元数据。
//
// Title / DurationSec 留零值是合法的：批量枚举拿不到每集标题与时长时，
// 插件 MUST 留零值而不是编造，也不允许为补齐它们逐集发请求（红果逐集要
// 1 请求 × 2847 万集，见 docs/L1-CORPUS-SCOPE.md 取证 D/E）。
type Episode struct {
	No          int    // 集号，从 1 起连续
	Title       string
	DurationSec int
}

// WorkDetail 是一部作品的详情。Episodes 是按 No 升序、从 1 起连续的完整集表。
type WorkDetail struct {
	Work     Work
	Episodes []Episode
}

// Cursor 是不透明的枚举断点。框架原样持久化（source_state.cursor）与原样回传。
type Cursor struct {
	Token string
}

// Page 是一次枚举结果。Next == nil 表示枚举已结束。
type Page struct {
	Refs []WorkRef
	Next *Cursor
}

// Stream 是一次直链解析结果。
type Stream struct {
	URL       string
	ExpiresAt time.Time // 零值 = 不知道或不过期
}

// Plugin 是播放源插件契约。实现必须可并发调用（框架会并行取多部剧）。
type Plugin interface {
	// Name 是稳定的源标识，直接写进 drama.provider / episode.source。
	// 一经发布不得更改（改它等于改库里的历史数据）。
	Name() string

	// Enumerate 分页枚举源侧可见的全部作品标识。
	//
	// cur == nil 表示从头开始。返回 Page.Next == nil 表示本轮枚举已走到尽头。
	// 返回空 Refs + Next == nil 是合法的（源里一个作品都没有）。
	// 只返回标识，不要求元数据 —— 元数据由框架逐部调 Work 取。
	// 分页大小由插件自定；框架不解释 Cursor.Token 的内容。
	Enumerate(ctx context.Context, cur *Cursor) (Page, error)

	// Work 取一部作品的元数据与全部集。
	//
	// ref.ID 不在源侧 → ErrNotFound。
	// 返回的 WorkDetail.Episodes 必须是完整集表：从 1 起连续、无空洞、无重复。
	// 框架按 Episodes 的长度写 episode 行，因此「少返回」等于静默丢集。
	Work(ctx context.Context, ref WorkRef) (WorkDetail, error)

	// Resolve 解析某一集的可播放直链。
	//
	// epNo 超出该作品集数 → ErrNotFound；集存在但源侧不公开直链 → ErrNoStream。
	// 返回值只在本次请求内有意义，框架不持久化（docs/DATA-MODEL.md「播放入口」）。
	Resolve(ctx context.Context, ref WorkRef, epNo int) (Stream, error)

	// Health 做一次最小可用性自检：能连上、能认出一个已知对象。
	//
	// 只用于启动自检与状态页；不参与刷新逻辑，失败不影响服务启动。
	Health(ctx context.Context) error
}
```

### 契约的其余硬性要求（不进签名，但可 review）

1. **不写库、不 import 框架内部包。** 插件只依赖标准库 + `internal/plugin`。
   `internal/plugin/<源>` 的 rank 为 30，`grep` 可查（`scripts/check-deps.sh`）。
2. **不落任何状态。** 插件实例不保存游标、不保存上次结果、不缓存直链。
   唯一允许的字段是构造期注入的不可变配置（`Name`、`BaseURL`、`*http.Client`、`*slog.Logger`）。
3. **错误必须 `%w` 包装哨兵**，且错误文本 **MUST NOT** 含直链、签名参数、
   Cookie、请求头。可以含作品标识与集号（排障要）。
4. **超时归插件。** 每次出站请求 MUST 带 `ctx`，且 `*http.Client` 有 `Timeout`
   （默认值见「配置」）。框架给整个调用另加一层 `ctx` 超时。
5. **响应体必须限量。** 插件 MUST 用 `io.LimitReader` 封顶，超限按 `ErrSchema`
   （页面异常放大不是可重试故障）。红果实测页面（列表页 / 集页 / `/category` 一页）
   213–281 KB，封顶 **4 MiB**；子 sitemap 不是「一页」而是整份 5,242,7xx 字节的 XML
   （29 份实测均如此），单独封顶 **8 MiB**——两档上限分开，页面档不因 sitemap 放宽。
6. **`Enumerate` 返回的 `WorkRef.Plugin` MUST 等于 `Name()`。** 框架会校验，
   不等即按 `ErrSchema` 处理（这是插件实现 bug，不是上游问题）。
7. **`Work` 返回的 `Episodes[i].No` MUST 等于 `i+1`。** 不连续即 `ErrSchema`。

## 错误语义

框架对每个错误只有一个动作。这张表是唯一真值，S2/S3/S4 都照它实现：

| 哨兵 | 触发条件（举例） | 框架动作 | 库 | 可重试 |
|---|---|---|---|---|
| `ErrUpstream` | 连接失败、5xx、读体失败、`ctx` 超时、空响应体 | 跳过该对象，记 `source_state.last_error` 与时间，**不清游标** | 不变 | 是（下一周期） |
| `ErrRejected` | 429、403、444（上游丢连接，实测为限流）、风控页 | 立刻中止本轮的剩余工作，落当前游标，记错误 | 不变 | 是（退避到下一周期） |
| `ErrSchema` | 缺 `_ROUTER_DATA`、身份校验失败、`vid_list` 为空/非数字/重复 | 跳过该对象，记错误与计数；同一轮内连续 N 次（N=20）则中止本轮（当作源级结构变更） | 不变 | 否（人等） |
| `ErrNotFound` | 上游 404 / 明确表示已下架 | 跳过该对象，**不删库**（刷新只增改不删，见 `docs/DATA-MODEL.md`「增删改语义」） | 不变 | 否 |
| `ErrNoStream` | `VideoObject` 在但无 `contentUrl`；集页 404 | 该集可播性 = 否。出流端点返回 502，同步流程视为成功 | 不变 | 否 |

三条补充规则：

- **未知错误**（既非上述哨兵也非 `context.Canceled`）按 `ErrUpstream` 处理并额外记
  `unknown_error=true`——插件 bug 不该拖垮整轮，但必须在状态页看得见。
- **`context.Canceled` / `context.DeadlineExceeded`** 直接向上传播，不写
  `source_state.last_error`（用户点了取消，不是源故障）。
- **任何错误都不得导致已入库数据被删除或覆盖成空值。** 局部写入的边界是「一部剧一个事务」。

## 注册

显式注册，不用 `init()`（ADR-0006 第 5 条）。

```go
// internal/plugin/registry.go（框架侧）
type Registry struct { /* ... */ }

func NewRegistry() *Registry
func (r *Registry) Register(p Plugin) error // 名字重复 / 空名 / nil → 错误
func (r *Registry) Get(name string) (Plugin, bool)
func (r *Registry) Names() []string // 按注册顺序，稳定
```

```go
// cmd/drama-hub/main.go（装配根）
reg := plugin.NewRegistry()
if err := reg.Register(hongguo.New(hongguo.Config{
    BaseURL:    pluginBaseURL(ctx, "web"),
    HTTPClient: httpClient,
    Logger:     logger,
})); err != nil {
    return err
}
```

`Registry.Register` 在以下情况返回错误（不 panic）：`p == nil`、`p.Name() == ""`、
与前一个插件重名。

## 配置

配置走 `settings` 表（AGENTS.md 硬约束 5：可调参数进 settings），key 用统一前缀。
`<源>` 是 `Plugin.Name()`。

| key | 类型 | 默认 | 取值范围 | 作用 |
|---|---|---|---|---|
| `plugin.<源>.enabled` | bool | 内置源 `true` | — | 是否参与调度 |
| `plugin.<源>.base_url` | string(URL) | 插件内置默认 | 带 scheme 的绝对 URL 或空 | 源地址（空 = 用插件默认） |
| `plugin.<源>.request_timeout_sec` | int | `30` | 5–300 | 单次出站请求超时 |

刷新间隔相关的 key（`refresh_interval_sec`、`plugin.<源>.refresh_interval_sec`、
`refresh_batch`、`refresh_timeout_sec`）归 `docs/SCHEDULER-MODEL.md`「配置」，
本表不重复列：同一张表出现两处就会漂。

- 空值（`""`）表示「继承 / 用插件默认」，与「未设置」等价；两者行为相同。
- `plugin.<源>.enabled` 缺失时按 `true` 处理（内置源默认启用），因此升级到 2.0 后
  红果不需要先手工开一下。
- 新增 `kindBool` 到 `internal/store` 的配置注册表；`settings` 的 key 校验从
  「7 个固定 key 的 switch」改成「固定 key + `plugin.<源>.<项>` 前缀模式」。
  这是 `internal/store` 的改动，归 S2。
- 配置生效方式分两类（与硬性要求 2「构造期不可变配置」一致）：`plugin.<源>.enabled`
  与 `refresh_interval_sec`（全局与源级覆盖）热读——调度器每次醒来重读 settings，
  改完最多 60 秒按新值算；`plugin.<源>.base_url` 与 `plugin.<源>.request_timeout_sec`
  改后需重启（`Settings` 每次读都查库、不缓存，但插件实例与 `*http.Client` 在构造期
  定型，不得为实现热读去改插件接口）。

## 第一个实现：红果

位置 `internal/plugin/hongguo`，`Name() == "web"`（理由见 `docs/DATA-MODEL.md`
「provider 标识」）。三个上游端点：

| 方法 | 上游 | 说明 |
|---|---|---|
| `Enumerate` | `GET /sitemap/hongguoduanju/index.xml` → `index{N}.xml` → 并集 `GET /seo-important.xml`，再 `GET /category?page=2..34` | 只取 `/player/{id}` 与 `/detail?series_id={id}` 的 id。游标 `v1:<段>:<位置>`（段 = `s`/`e`/`c`）：sitemap 段是「第几份子 sitemap」，seo 段只一份，category 段是「第几页」。走完 30 份 sitemap 后，用 `/category` 全 34 页补齐 sitemap 滞后漏掉的最新剧目。一页 = 一份上游文档（子 sitemap 整份 43,556 个 loc 一次返回），**不在文件中间切页**：切在中间要把「第几份 + 已读位置」都塞进游标，而一次 5 MiB GET 的成本本来就大于一次游标往返 |
| `Work` | `GET /player/{id}` | 解析 `_ROUTER_DATA → loaderData["player_(series_id)/page"] → seriesDetail`：`series_name` / `series_cover` / `series_intro` / `tags` / `episode_cnt` / `vid_list` |
| `Resolve` | `GET /player/{id}/{ep_vid}` | JSON-LD `VideoObject.contentUrl` + 绑定校验（`url` 的 pathname 含 `/player/{id}`、`contentUrl` 含 `/ep_vid`） |

- 红果的集标识（`vid`）进 `episode.vid` 列；`ep_no = vid_list 下标 + 1`。
- 校验沿用现有实现（`internal/provider/parse.go` 已验证的部分）：`isSuccess` 不为
  false、页面 `series_id` 与 `seriesDetail.series_id` 都等于请求 id、`vid_list` 非空/
  全数字/无重复。
- **444 是限流，不是「不存在」。** 实测全量灌库累计约 1 万次 `/player` 之后，源侧开始对
  `/player` 全量回 HTTP 444（无响应体、~50 ms；同一时刻 sitemap 与 `/category` 仍是 200），
  约十几分钟后恢复。插件按 `ErrRejected` 处理（中止本轮、退避到下一周期）：不这么做的话
  4 个 worker 会在被限流的 17 分钟里打出 98,190 次请求（444 回得快，比正常吞吐高 20 倍）。
  取证与实测数字见 `docs/L1-CORPUS-SCOPE.md`「取证 F」。
- **已知的源侧硬限制**（不是插件缺陷、不许在插件里「修」）：只有第 1 集公开直链，
  且是 10 秒预览；第 2..N 集 `contentUrl` 缺失；第 4 集起集页 404。
  `docs/L1-CORPUS-SCOPE.md` 取证 D 有完整数据。
- 现有 `internal/provider` 的解析逻辑整体搬进 `internal/plugin/hongguo`，
  `SyncSeries` 的写库部分上移到框架（`internal/sync`），不留在插件里。

## 2.0 明确不做

- 插件进程隔离 / 动态加载（`.so`、`plugin` 包、子进程）。
- 插件市场、版本协商、`Caps` 能力协商（所有插件实现全部方法，不支持就以哨兵表示）。
- 插件自带的缓存或重试策略：重试归框架调度，缓存归本服务（ADR-0007，`docs/CACHE-MODEL.md`）。
- 跨源去重（同一部剧出现在两个源）：`(provider, provider_id)` 是身份，
  同一部剧在两个源就是两行。理由：去重需要标题归一 + 人工裁决，2.0 无多源共存需求。

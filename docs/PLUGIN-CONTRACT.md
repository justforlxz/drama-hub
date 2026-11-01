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
| 直链解析 | 插件 | `Resolve` | 红果：2 个请求 / 集（网页通道：详情页 + 集页）；受保护剧改走 App 通道（见「通道与取流顺序」） | `/video/{id}/{seq}.ts` 生成分片时按需取，不落库也不进缓存索引（`docs/PLAYBACK-CONTRACT.md`）；加密切片另带内容密钥 `Stream.Key`，见「加密切片与 Key」 |
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
//
// 「可播」的唯一判据是 URL 非空；Key 是附加属性，不影响可播性判定。
type Stream struct {
	URL string
	// Key 是 URL 指向内容的 AES-128 解密密钥（16 字节，CENC-AES-CTR），
	// nil = 明文。只在内存里存在：MUST NOT 落库、MUST NOT 进日志与错误文本
	// （「加密切片与 Key」）。
	Key       []byte
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
	// Key 与 URL 同生命周期：由播放面在本次请求内交给 ffmpeg，用完即弃。
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
   允许的字段只有两类：构造期注入的**不可变**配置（`Name`、`BaseURL`、`*http.Client`、
   `*slog.Logger`、App 通道的两个 host），以及下面这一个**可变**例外。

   **例外：匿名设备身份**（`Plugin.device`，MKACG-626 契约 3 / MKACG-628 落地）。
   这是本条唯一的可变字段，其余任何可变状态一律算缺陷：
   - 形态是**单条**缓存：成功一条，失败也存同一条（不是池、不是 `map`、不按剧分）；
   - 只在进程内存里：**不落库、不落盘、不进 settings** —— 重启即重新注册，
     与「插件无持久状态」不冲突；
   - 闸：并发首次取用只发一次注册（single-flight，不是两个开关）；失败同样落缓存，
     **60 秒内不再重试**；换身份（续期）共用同一条 60 秒闸，即「续期 ≤ 1 次/分钟」；
   - 不进日志：日志只记 `channel` / `vid` / 错误类别，身份字段本身是凭据形态，不记。

   为什么必须开这个口子：App 通道每次取流都要带设备参数，注册端点一次只发一条身份，
   重复注册会被上游当异常。没有这条缓存就得每次播放都注册一遍 —— 那是主动触发风控。
3. **错误必须 `%w` 包装哨兵**，且错误文本 **MUST NOT** 含直链、解密 key、
   签名参数、Cookie、请求头。可以含作品标识与集号（排障要）。
4. **超时归插件。** 每次出站请求 MUST 带 `ctx`，且 `*http.Client` 有 `Timeout`
   （默认值见「配置」）。框架给整个调用另加一层 `ctx` 超时。
5. **响应体必须限量。** 插件 MUST 用 `io.LimitReader` 封顶，超限按 `ErrSchema`
   （页面异常放大不是可重试故障）。红果实测页面（列表页 / 集页 / `/category` 一页）
   213–281 KB，封顶 **4 MiB**；子 sitemap 不是「一页」而是整份 5,242,7xx 字节的 XML
   （29 份实测均如此），单独封顶 **8 MiB**——两档上限分开，页面档不因 sitemap 放宽。
6. **`Enumerate` 返回的 `WorkRef.Plugin` MUST 等于 `Name()`。** 框架会校验，
   不等即按 `ErrSchema` 处理（这是插件实现 bug，不是上游问题）。
7. **`Work` 返回的 `Episodes[i].No` MUST 等于 `i+1`。** 不连续即 `ErrSchema`。
8. **出站请求 MUST 带源侧要的身份（UA / `Accept` / `Accept-Language` / `Referer`）。**
   Go 默认 UA（`Go-http-client/1.1`）是明显的非浏览器标识，而源侧风控是既定事实
   （实测红果跑到约 1 万次 `/player` 后全量回 444，见「错误语义」`ErrRejected` 行）。
   身份是「浏览器长什么样」的事实，**MUST NOT** 做成 settings 键（没有用户会调它）；
   红果这一份写在 `internal/plugin/hongguo` 的常量里，播放链路的 `-headers`
   由装配根从同一份常量生成（`PlaybackHeaderLines`），不另写一份 UA。
   实测证据与取舍见 MKACG-624。
   App 通道是另一份身份（Android App 形态），与官网页的浏览器身份**不是一套**：
   两份常量各管各的出站，MUST NOT 互相顶掉，也不是 settings 键（MKACG-626 §6）。
   **取字节的头还要再按通道分一次**（MKACG-638）：App 通道的媒体 CDN 对带站内 `Referer`
   的请求回 `403`（实测：仅 `Referer` 403，无头 / 仅 UA 都能探出完整集时长），所以播放面给
   App 通道的 `-headers` 只带 UA。这一份也由装配根从插件常量派生，不在 `internal/segment`
   里写第二份身份（`docs/PLAYBACK-CONTRACT.md`「出站身份」）。

## 加密切片与 Key

有的源只给加密片源（红果 App 通道的 MP4 带 `spade_a`）。契约把内容密钥作为
`Stream` 的附加属性带出来，解密归播放面，两件事都不在插件里做：

| 项 | 口径 |
|---|---|
| 类型 | `Key []byte`，**16 字节 AES-128**（CENC-AES-CTR 的内容密钥） |
| 明文 | `nil`。插件 MUST 给 `nil`；`len(Key) == 0`（含空切片）一律按明文处理 |
| 可播判据 | 仍是 `URL` 非空。`Key` 有无不改变任何状态码（`docs/PLAYBACK-CONTRACT.md` 状态码矩阵） |
| 生命周期 | 与 `URL` 相同：只在本次请求的内存里。**MUST NOT** 落库（`episode` / `artifact` / `settings` 都没有对应的列与键）、**MUST NOT** 进日志与错误文本、**MUST NOT** 进缓存键 |
| 消费方 | `internal/stream.Resolver` 原样透传到 `Direct{URL, Key}`，播放面把它交给 `ffmpeg -decryption_key`（在 `-i` 之前）；框架与插件都不实现解密 |

- key 是「上游给的内容密钥」，不是自造值：插件 MUST NOT 缓存它（与硬性要求 2
  「不落任何状态」同源）。一个 `URL` 只对应一个 `Key`——换 key 必须同时换 URL。
- 同一集的明文源与密文源字节流不同，分片身份里必须有通道段（`CodecSig` 的通道限定，
  MKACG-626 裁决 3），否则缓存会串味、索引会被沿用。

## 通道与取流顺序（不回落）

「通道」是同一个插件内部的取流分支，**不是新 provider**：App 通道取的是同一部剧的同一份
元数据，另立 provider 会让同一部剧变成两行（与「跨源去重不做」冲突），而
`Name() == "web"` 一经发布不得改（`docs/DATA-MODEL.md`「provider 标识」）。

`Resolve` 的顺序是**写死的**（MKACG-626 契约 2，MKACG-630 / MKACG-635 落地）：

```
网页通道（非预览）── 拿到明文直链 → 返回
        │
        └─ 受保护（protectedPlayback == true），或网页通道给 ErrNoStream
                ↓
        App 通道：设备身份 → 取流 → 选流 → 派生 key → 解 URL → 白名单校验
                ↓
        失败 → 原样返回哨兵（/video 侧 502），不回落
```

- **网页页 `protectedPlayback == true` 时，那条 10 秒 SEO 预览一律 `ErrNoStream`。**
  插件 MUST NOT 把预览 URL 当直链交付：播放器无法「如实告知这是预览」，静默交付等于把
  10.07 秒的片子冒充整集（实测见 `docs/L1-CORPUS-SCOPE.md` 取证 D）。
- **App 通道不可用时就是 502，不降级。** `/video` 返回 502 + 一条 `WarnContext`
  （`channel=app`、`vid`、错误类别），不回落成「200 + 10 秒 playlist」。
- 「可播」判据仍是 `URL` 非空：通道选择不改变状态码矩阵（`docs/PLAYBACK-CONTRACT.md`）。
- **未受保护的剧先试网页通道**：拿到明文直链就返回，不再发 App 请求（`protectedPlayback`
  只决定「要不要先试网页通道」，不决定「发不发 App 请求」）。网页回 `ErrNoStream`
  （第 2..N 集没有 `contentUrl`、集页 404 就是这种形态）时照样按上面的顺序落到 App 通道；
  其余错误原样上抛——上游故障不该被 App 通道掩盖。
- 同一集的网页源与 App 源字节流不同，因此分片身份必须带通道段（见「加密切片与 Key」
  与 `docs/CACHE-MODEL.md`「失效」）。

## 错误语义

框架对每个错误只有一个动作。这张表是唯一真值，S2/S3/S4 都照它实现：

| 哨兵 | 触发条件（举例） | 框架动作 | 库 | 可重试 |
|---|---|---|---|---|
| `ErrUpstream` | 连接失败、5xx、读体失败、`ctx` 超时；**网页通道 200 + 空响应体** | 跳过该对象，记 `source_state.last_error` 与时间，**不清游标** | 不变 | 是（下一周期） |
| `ErrRejected` | 429、403、444（上游丢连接，实测为限流）、风控页；**App 通道**：设备注册失败 / 空响应 / 业务拒绝、取流 403/429/444、响应 `code != 0` | 立刻中止本轮的剩余工作，落当前游标，记错误 | 不变 | 是（退避到下一周期） |
| `ErrSchema` | 缺 `_ROUTER_DATA`、身份校验失败、`vid_list` 为空/非数字/重复、响应体超限；**App 通道**：**200 + 空体或非 JSON**、密文缺 `spade_a` 或 key 派生失败、media URL 不在白名单 | 跳过该对象，记错误与计数；同一轮内连续 N 次（N=20）则中止本轮（当作源级结构变更） | 不变 | 否（人等） |
| `ErrNotFound` | 上游 404 / 明确表示已下架 | 跳过该对象，**不删库**（刷新只增改不删，见 `docs/DATA-MODEL.md`「增删改语义」） | 不变 | 否 |
| `ErrNoStream` | `VideoObject` 在但无 `contentUrl`；集页 404；受保护剧的 10 秒预览（不当作直链）；**App 通道取流 404（带体或不带体）** | 该集可播性 = 否。出流端点返回 502，同步流程视为成功 | 不变 | 否 |

三条补充规则：

- **未知错误**（既非上述哨兵也非 `context.Canceled`）按 `ErrUpstream` 处理并额外记
  `unknown_error=true`——插件 bug 不该拖垮整轮，但必须在状态页看得见。
- **`context.Canceled` / `context.DeadlineExceeded`** 直接向上传播，不写
  `source_state.last_error`（用户点了取消，不是源故障）。
- **任何错误都不得导致已入库数据被删除或覆盖成空值。** 局部写入的边界是「一部剧一个事务」。

### 判定顺序：状态码先于响应体（MKACG-635）

`fetch`（网页通道）与 `appPlayback`（App 通道）都必须**先判 HTTP 状态码，再判响应体**。反过来的代价不是日志措辞：
把「404 + 空体」当「身份被挡」会归成 `ErrRejected`，于是多换一轮设备身份、多打一次上游
（MKACG-630 的真实现象，MKACG-635 修掉）。修好之后 App 通道每一格是：

| 上游响应 | 哨兵 | 后果 |
|---|---|---|
| 403 / 429 / 444 | `ErrRejected` | 换一条身份重试一次（受续期 ≤1 次/分钟闸约束），仍失败即 502 |
| 404（带体或不带体） | `ErrNoStream` | 该集无片源：不换身份、不重试 |
| 其余非 200（5xx 等） | `ErrUpstream` | 不换身份；退避到下一周期 |
| 200 + 非 JSON / 空体 | `ErrSchema` | 结构不符，重试无用 |
| 200 + `code != 0` | `ErrRejected` | 与风控同一动作（换一次身份） |

网页通道同一顺序：429/403/444 → `ErrRejected`，404 → `ErrNotFound`（`Resolve` 的集页
404 再翻成 `ErrNoStream`），其余非 2xx → `ErrUpstream`；只有 200 才读体，200 + 空体
按 `ErrUpstream`（重试有意义）。**不许**把「空体」当成状态码之外的独立判据。

**播放面还有一格，不在这张表里**：ffprobe 探针的 stderr 非空（解密自检失败）由
`internal/segment` 报 `httpx.ErrUpstream`（502、下一轮重试），不是插件哨兵 ——
`internal/segment` 不 import `internal/plugin`，产不出 `plugin.ErrSchema`。实现口径见
`docs/DEV-STANDARDS.md`「播放面」。

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

- **App 通道的两个 host 不是 settings 键**（MKACG-626 D2，MKACG-628/630 落地）：
  `DeviceRegisterURL` 与 `AppVideoURL` 是 `hongguo.Config` 的构造期字段（默认值内置、
  测试注入假上游），与 `BaseURL` 同一类 —— 改它们的生效方式是**重启**，不是热读。
  理由是它们是 App 侧协议事实（路径、查询串里的版本号是逆向常量），没有用户会调；
  多一个键就多一条「配了却不生效」的来源。代价：上游换域名要发版。

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
- **网页通道的上限**（不是插件缺陷、不许在插件里「修」）：网页页只给第 1 集一条直链，
  且是 **10.07 秒 / 651 KB 的 360p 预览**；第 2..N 集 `contentUrl` 缺失；第 4 集起集页 404。
  `docs/L1-CORPUS-SCOPE.md` 取证 D 有完整数据。因此网页通道交付不了完整集，受保护剧
  （`protectedPlayback == true`）按「通道与取流顺序」直接换 App 通道，预览不当直链交付。

### App 通道（红果实现）

| 项 | 口径 |
|---|---|
| 归属 | 同一个插件内部的取流分支，`Name()` 仍是 `web`；不新增 provider、不新增 settings 键 |
| 端点 | 两个，**都是 `hongguo.Config` 的构造期字段**：`DeviceRegisterURL`（匿名设备注册）、`AppVideoURL`（取流）。默认值内置，测试注入假上游；上游换域名发版 |
| 设备身份 | 匿名注册拿 `device_id` / `install_id`（+ 本进程生成的 `cdid`）。规则见硬性要求 2 的例外：单条、失败也缓存、续期 ≤1 次/分钟、不落库不落盘不进 settings |
| 取址 | `POST` 取流端点（`mixed_video_id_map` 以集 `vid` 为键）→ 优先 h264 流 → `spade_a` 派生 16 字节 AES-128 内容密钥 → 有 `key_seed` 时再解出真实媒体地址 |
| 白名单 | media URL 必须在 `*.fqnovelvod.com` / `*.qznovelvod.com` / `*.douyinvod.com` 之下，https、无 userinfo、443。之外一律 `ErrSchema`，**不出站** |
| 错误归类 | 见「错误语义」的判定顺序表；风控被拒 → 换一次身份后 502，**不回落预览** |
| 敏感面 | `spade_a`、`key_seed`、内容密钥、解出的 URL 查询串**一律不进日志与错误文本**；日志只记 `channel` / `vid` / 错误类别，域名只在校验通过后出现 |
| 不做什么 | 不整集解密落盘、不自研 MP4-CENC 解析器、不新增第三方依赖、不搬参考实现的源码与常量表（只取协议与算法事实，Go 侧独立实现，夹具自建） |

- 现有 `internal/provider` 的解析逻辑整体搬进 `internal/plugin/hongguo`，
  `SyncSeries` 的写库部分上移到框架（`internal/sync`），不留在插件里。

## 2.0 明确不做

- 插件进程隔离 / 动态加载（`.so`、`plugin` 包、子进程）。
- 插件市场、版本协商、`Caps` 能力协商（所有插件实现全部方法，不支持就以哨兵表示）。
- 插件自带的缓存或重试策略：重试归框架调度，缓存归本服务（ADR-0007，`docs/CACHE-MODEL.md`）。
- 跨源去重（同一部剧出现在两个源）：`(provider, provider_id)` 是身份，
  同一部剧在两个源就是两行。理由：去重需要标题归一 + 人工裁决，2.0 无多源共存需求。

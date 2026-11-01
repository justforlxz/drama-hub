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
	Year        int // 0 = 上游未声明
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
	No          int // 集号，从 1 起连续
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
	// （docs/PLUGIN-CONTRACT.md「加密切片与 Key」）。
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

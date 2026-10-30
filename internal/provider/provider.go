package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

// providerName 是本数据源写入 drama.provider / episode.source 的标识。
const providerName = "web"

// defaultBaseURL 是官网站点根，仅作为默认值出现在这里；测试用 Source.BaseURL
// 指向 httptest.Server，生产代码不出现第二个站点字面量。
const defaultBaseURL = "https://hongguoduanju.com"

// defaultTimeout 是空 *http.Client 时兜底的出站超时。
const defaultTimeout = 15 * time.Second

// maxBodyBytes 是单次响应体的读取上限：实测列表页约 220 KB，4 MiB 足够且能挡住
// 上游异常放大 / 无限流。
const maxBodyBytes = 4 << 20

// 本包的哨兵错误。调用方用 errors.Is 判断；传输层状态码映射由边界包负责
// （边界包不在这里，provider 不认识 httpx）。
var (
	// ErrUpstream 表示上游请求失败：传输错误、非 200、读体失败或空响应。
	ErrUpstream = errors.New("上游请求失败")
	// ErrParse 表示上游页面结构变化：缺 _ROUTER_DATA、JSON 解不开、缺字段、
	// 页面身份不符、vid_list 非法、无匹配的 VideoObject 或响应体超限。
	ErrParse = errors.New("上游页面结构变化")
	// ErrNoDirectURL 表示该集页面没有直链（VideoObject 在，contentUrl 缺失）。
	ErrNoDirectURL = errors.New("该集无可播放直链")
)

// Source 是官网 SSR 数据源。零值不可用，用 NewSource 构造。
//
// BaseURL 是唯一为测试开的缝：--network=none 的测试必须把它指向
// httptest.Server。
type Source struct {
	Client  *http.Client // 非 nil；NewSource 遇 nil 时给默认 15s 超时客户端
	BaseURL string       // 空 → defaultBaseURL
	Logger  *slog.Logger // nil → slog.Default()

	now func() time.Time // 取当前时间，测试注入固定值
}

// NewSource 构造 Source：空 client 用默认 15s 超时客户端，空 logger 用默认 logger。
func NewSource(client *http.Client, logger *slog.Logger) *Source {
	return &Source{Client: client, BaseURL: defaultBaseURL, Logger: logger, now: time.Now}
}

// SyncSeries 拉取一部剧的列表页，在一个事务里写入剧目与全部集。
//
// 只发一次上游请求：官网列表页的 seriesDetail.vid_list 已给出全部集标识，其中
// 第 i 项就是第 i+1 集（实测），因此不做 N+1 集页抓取。episode 的 play_url /
// play_url_at 留空：官网 SSR 只公开首集约 10 秒的预览片，写进去会让整库每集只
// 播 10 秒；真直链按需走 FetchDirectURL（详见 docs/DEV-STANDARDS.md）。
//
// 取数与解析在事务之外完成；库里任何一步失败都会整体回滚，不留「有剧无集」的
// 半截数据。错误为 ErrUpstream / ErrParse / store 的哨兵。
func (s *Source) SyncSeries(ctx context.Context, st *store.Store, sid string) error {
	body, err := s.get(ctx, "/player/"+sid, sid)
	if err != nil {
		return err
	}
	ser, err := parsePlayerPage(string(body), sid)
	if err != nil {
		return err
	}

	drama := store.Drama{
		Provider:     providerName,
		ProviderID:   ser.sid,
		Title:        ser.name,
		Year:         yearFromUnix(ser.firstVisibleTime),
		Plot:         ser.intro,
		CoverURL:     ser.cover,
		EpisodeTotal: len(ser.vids),
		SyncAt:       s.nowAt().UTC().Format(time.RFC3339),
	}
	return st.InTx(ctx, func(tx *store.Store) error {
		dramaID, err := tx.UpsertDrama(ctx, drama)
		if err != nil {
			return fmt.Errorf("写入剧目 %s: %w", sid, err)
		}
		for i, vid := range ser.vids {
			ep := store.Episode{DramaID: dramaID, EpNo: i + 1, Source: providerName, Vid: vid}
			if _, err := tx.UpsertEpisode(ctx, ep); err != nil {
				return fmt.Errorf("写入第 %d 集: %w", i+1, err)
			}
		}
		// 同步完成是 emby_item 派生表的调用点之一（DEV-STANDARDS「数据库」）：剧与集
		// 落库后立刻重建对外剧集视图，否则 vfs 树看不到任何集（硬约束 6 只认派生表）。
		if _, err := tx.RebuildEmbyItems(ctx, dramaID); err != nil {
			return fmt.Errorf("重建对外剧集 %s: %w", sid, err)
		}
		return nil
	})
}

// FetchDirectURL 拉取单集页，解析该集在上游的直链。成功返回规范化后的 URL。
//
// 读时解析、不写库：整集直链的消费者（出流）自己决定是否缓存。绑定校验保证
// 返回的是本集链接——被锁的集会被上游 302 到别的集页面，那种页面的 JSON-LD 指向
// 别的 vid，一律按 ErrParse 拒绝。错误为 ErrUpstream / ErrParse / ErrNoDirectURL。
func (s *Source) FetchDirectURL(ctx context.Context, sid, vid string) (string, error) {
	body, err := s.get(ctx, "/player/"+sid+"/"+vid, sid)
	if err != nil {
		return "", err
	}
	return parseDirectURL(string(body), sid, vid)
}

// get 发一次 GET 并读回响应体。slog 一条出站记录（源 / sid / 状态 / 字节数），
// 不记直链内容。空响应体、读体失败、非 200 都是 ErrUpstream；超过 maxBodyBytes
// 视为 ErrParse（页面异常放大，不是可重试的传输错误）。
func (s *Source) get(ctx context.Context, path, sid string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.baseURL(), "/")+path, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: 构造请求 %s: %v", ErrUpstream, path, err)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		s.logger().WarnContext(ctx, "上游请求失败", "provider", providerName, "sid", sid, "upstream_status", 0, "bytes", 0)
		return nil, fmt.Errorf("%w: GET %s: %v", ErrUpstream, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	level := slog.LevelInfo
	if readErr != nil || resp.StatusCode != http.StatusOK {
		level = slog.LevelWarn
	}
	s.logger().Log(ctx, level, "上游请求",
		"provider", providerName, "sid", sid,
		"upstream_status", resp.StatusCode, "bytes", len(body))

	if readErr != nil {
		return nil, fmt.Errorf("%w: 读取 %s 响应体: %v", ErrUpstream, path, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: GET %s 返回 %d", ErrUpstream, path, resp.StatusCode)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: GET %s 返回空响应体", ErrUpstream, path)
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("%w: GET %s 响应体超过 %d 字节", ErrParse, path, maxBodyBytes)
	}
	return body, nil
}

func (s *Source) client() *http.Client {
	if s.Client == nil {
		return &http.Client{Timeout: defaultTimeout}
	}
	return s.Client
}

func (s *Source) baseURL() string {
	if s.BaseURL == "" {
		return defaultBaseURL
	}
	return s.BaseURL
}

// nowAt 是取当前时间的唯一入口：测试注入固定时钟（s.now），零值 Source 回落到
// time.Now 而不是空指针 panic。
func (s *Source) nowAt() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (s *Source) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

// yearFromUnix 把上游的 unix 秒（字符串）换成 UTC 年份，尽力而为：缺失或非法
// 返回 0，不报错（该字段缺失不该让整次同步失败）。
func yearFromUnix(raw string) int {
	if raw == "" {
		return 0
	}
	var sec int64
	if _, err := fmt.Sscanf(raw, "%d", &sec); err != nil || sec <= 0 {
		return 0
	}
	return time.Unix(sec, 0).UTC().Year()
}

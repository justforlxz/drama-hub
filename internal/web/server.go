package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/jellyfin"
	"github.com/justforlxz/drama-hub/internal/playback"
	"github.com/justforlxz/drama-hub/internal/scheduler"
	"github.com/justforlxz/drama-hub/internal/store"
)

// SeriesSyncer 是「新增剧目」的依赖：按上游剧 id 把一个剧同步入库。生产实现是
// *sync.Syncer（签名一致，见 internal/sync），测试注入假实现。
//
// 接口定义在消费方（docs/DEV-STANDARDS.md「代码组织」）：本包只用到下面两个方法，
// 不 import sync / plugin，也不预建别的数据源方法。Name 是写库用的 drama.provider
// （= 插件名），建剧 handler 靠它判断这次同步是「新建」还是「更新」（MKACG-623）。
type SeriesSyncer interface {
	SyncSeries(ctx context.Context, st *store.Store, sid string) error
	Name() string
}

// Source 是一个播放源在状态页上的展示身份与「启动时生效的构造期配置」。
//
// 装配根从注册表取名字（= Plugin.Name()），配一句人看得懂的标签；本包不 import
// plugin，也不猜有哪些源。
//
// enabled / refresh_interval_sec 等热读项由本包每请求现读 settings（硬约束 5），
// 改完下一次读即生效；base_url / request_timeout_sec 是构造期注入的（插件的
// *http.Client 在启动时定型），本包读不到「正在生效的那个值」，只能由装配根把启动
// 时注入的值带进来对比 —— 这就是 Boot* 两个字段的用途。
type Source struct {
	Name  string // = Plugin.Name()，也是 drama.provider / episode.source 的值
	Label string // 状态页展示用；空 = 只显示 Name

	BootBaseURL           string // 启动时读到并注入插件的 base_url 原值；"" = 插件内置默认
	BootRequestTimeoutSec int    // 启动时注入插件的出站超时（秒）
}

// Server 汇总管理 API 与管理界面。它实现 http.Handler，由 cmd 挂到 `/` 上；
// 完整路径（含 `/api` 前缀）原样传到本包，ServeMux 不剥前缀。
type Server struct {
	st       *store.Store
	settings *store.Settings
	syncer   SeriesSyncer
	schedule Schedule
	jellyfin Jellyfin
	sources  []Source
	// playback 是「正在播放」的内存会话表（MKACG-683），只读快照给 /api/playback。
	// 采集在 internal/segment，本包不写它。
	playback *playback.Tracker
	// covers 是海报字节的来源（与 WebDAV、同步共用同一份缓存实例）；signID 把
	// (stream_secret, uid) 算成对外播放 id；dramaPath 返回剧在树里的目录路径。
	// 三者都由装配根注入：本包不 import cache / stream / vfs 的实现。
	covers    Coverer
	signID    IDFunc
	dramaPath DramaPathFunc
	// adminToken 是写操作的令牌（DRAMA_HUB_ADMIN_TOKEN）。空 = 没配 = 写操作一律 401。
	adminToken string
	logger     *slog.Logger
	mux        *http.ServeMux
}

// Jellyfin 是 Jellyfin 媒体库维护的依赖：触发一轮、状态页要显示的现状，以及分类
// 开关列表（MKACG-668）。生产实现是 *jellyfin.Manager；测试注入假实现。
//
// 接口定义在消费方：本包只要这三个动作，不关心差异算法、单飞锁与内存状态；开关的
// 落库由本包直接写 store（写是管理面的职责，Manager 只读）。
type Jellyfin interface {
	Start(ctx context.Context) error
	State(ctx context.Context) jellyfin.State
	Categories(ctx context.Context) ([]jellyfin.CategoryState, error)
}

// Coverer 是海报字节的来源（生产实现：*cache.CoverCache，与同步、WebDAV 共用同一
// 实例）。接口定义在消费方：本包只要「字节 + 有没有」这一个动作，落盘布局、单飞锁、
// 回源超时都在实现方，web 不认识 cache 内部。
type Coverer interface {
	Poster(ctx context.Context, provider, providerID, url string) (body []byte, available bool, err error)
}

// IDFunc 由装配根注入，把 (stream_secret, uid) 算成对外播放 id。
//
// 与 vfs.IDFunc 同一形态：web（rank 50）不 import internal/stream（rank 20），
// 算法在装配根接上（docs/PLAYBACK-CONTRACT.md「id 怎么来」）。
type IDFunc func(secret, uid string) string

// DramaPathFunc 由装配根注入，返回一部剧在虚拟树里的目录路径（`短剧/<分类>/<剧目录>`）。
//
// 目录名只有 vfs 一处算法（分类兜底与转义、同分类内同名冲突后缀），管理界面拿它
// 拼可复制的 WebDAV 串，不自己拼（否则两处规则必然漂移）。
type DramaPathFunc func(ctx context.Context, d store.Drama) (string, error)

// Schedule 是刷新调度的依赖：手动触发一轮，以及状态页要显示的调度现状。
// 生产实现是 *scheduler.Scheduler（签名一致）；测试注入假实现。
//
// 接口定义在消费方：本包只要这两个动作，不关心三段、单飞锁与游标。
type Schedule interface {
	Trigger(ctx context.Context, source string) error
	Status(ctx context.Context) ([]scheduler.SourceStatus, error)
}

// NewServer 构造路由表。st / settings / syncer / schedule / jellyfinMgr 不可为 nil；
// jellyfinMgr 是媒体库维护（生产实现是 *jellyfin.Manager）；sources 是状态页要列的播放源
// （装配根按注册表给，空表示没有源）；adminToken 是写操作的令牌
// （`DRAMA_HUB_ADMIN_TOKEN`，空 = 没配 = 写操作一律 401）；logger 用于请求与同步日志。
//
// 路由分两类：管理界面（`/`、`/settings` 与内嵌的 `/static/`）与管理 API
// （`/api/` 子树）。不注册 catch-all：未登记路径由 ServeMux 兜底 404，
// 这是「未知路径 → 404」的前提（cmd 的装配注释同此约定）。
//
// 写操作（非 GET/HEAD/OPTIONS）在 ServeHTTP 里先过令牌，路由表本身不重复这件事：
// 新增写端点不需要记得加鉴权，忘不了。
func NewServer(st *store.Store, settings *store.Settings, syncer SeriesSyncer, schedule Schedule, jellyfinMgr Jellyfin, sources []Source, pb *playback.Tracker, covers Coverer, signID IDFunc, dramaPath DramaPathFunc, adminToken string, logger *slog.Logger) *Server {
	s := &Server{
		st:         st,
		settings:   settings,
		syncer:     syncer,
		schedule:   schedule,
		jellyfin:   jellyfinMgr,
		sources:    sources,
		playback:   pb,
		covers:     covers,
		signID:     signID,
		dramaPath:  dramaPath,
		adminToken: adminToken,
		logger:     logger,
		mux:        http.NewServeMux(),
	}

	// 页面与静态资源：模板与 CSS/JS 随二进制嵌入（ADR-0002，无构建步骤）。
	// 页面集合见 pages.go 的 pageMetaOf；未登记路径（含旧的 /status）由 ServeMux 兜底 404。
	// 三个页面（MKACG-688）：概览、同步、设置。旧剧库 / 剧详情 / 分类页已移除，
	// 不注册路由即 404（显式路由，不做 catch-all，也不做重定向）。
	s.mux.HandleFunc("GET /{$}", s.handlePage("overview"))
	s.mux.HandleFunc("GET /sync", s.handlePage("sync"))
	s.mux.HandleFunc("GET /settings", s.handlePage("settings"))
	s.mux.HandleFunc("GET /static/{file}", s.handleStatic)

	// 管理 API：路径即契约，页面 JS 与脚本只调这些端点（见 docs/DEV-STANDARDS.md）。
	s.mux.HandleFunc("GET /api/dramas", s.handleListDramas)
	s.mux.HandleFunc("POST /api/dramas", s.handleCreateDrama)
	// 剧详情页的两个只读端点：集分块（块边界由 store.deriveEmbyItems 派生）与封面字节。
	s.mux.HandleFunc("GET /api/dramas/{id}/episodes", s.handleDramaEpisodes)
	s.mux.HandleFunc("GET /api/dramas/{id}/cover", s.handleDramaCover)
	s.mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	// 「正在播放」快照：纯内存只读，GET 不过令牌闸门（MKACG-683）。
	s.mux.HandleFunc("GET /api/playback", s.handlePlayback)
	s.mux.HandleFunc("PATCH /api/settings", s.handlePatchSettings)
	// 建库是写操作：令牌闸门在 ServeHTTP 里统一过，这里不重复。
	s.mux.HandleFunc("POST /api/jellyfin/libraries", s.handleJellyfinLibraries)
	// 同一条路径的读：页面轮询进度与上次结果（MKACG-664），不碰 drama 表。
	s.mux.HandleFunc("GET /api/jellyfin/libraries", s.handleJellyfinLibrariesState)
	// 分类开关（MKACG-668）：GET 列全部分类 + 开关状态，PATCH 改一个。
	// PATCH 是写操作，令牌闸门同上。
	s.mux.HandleFunc("GET /api/jellyfin/categories", s.handleJellyfinCategories)
	s.mux.HandleFunc("PATCH /api/jellyfin/categories", s.handleJellyfinCategoryToggle)
	return s
}

// AdminTokenHeader 是写操作携带后台管理令牌的请求头。
//
// 令牌来自 `DRAMA_HUB_ADMIN_TOKEN`（env），**不是** settings 键：它保护的就是「改
// settings」这个动作，放进 settings 会变成「改配置不需要令牌」。
const AdminTokenHeader = "X-Admin-Token"

// ServeHTTP 实现 http.Handler。
//
// 写操作（非 GET/HEAD/OPTIONS）在分派前先验管理令牌：`/api/` 原本完全无鉴权，
// `POST /api/dramas` 能触发同步，加写配置端点会同时放大它。
// 读操作维持现状（单用户本机管理面，Emby 与页面都靠 GET 浏览）。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isAdminWrite(r.Method, r.URL.Path) && !s.adminAuthorized(r) {
		httpx.WriteError(w, fmt.Errorf("%w: %s", httpx.ErrUnauthorized, s.adminDeniedReason()))
		return
	}
	s.mux.ServeHTTP(w, r)
}

// isAdminWrite 判断一次请求是不是受令牌保护的写操作：管理 API 里所有非读方法。
//
// 按「方法 + /api/ 前缀」判定而不是逐个登记端点：新加的写端点默认受保护，漏不了。
// 读方法只认 GET / HEAD / OPTIONS，其余（POST / PUT / DELETE / PATCH / …）都算写 ——
// 硬列写方法等于给未来的 PUT 留门，注释里的「默认受保护」也就不再成立（MKACG-634）。
func isAdminWrite(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return strings.HasPrefix(path, "/api/")
}

// adminAuthorized 校验请求头里的管理令牌。没配令牌时一律拒绝（fail closed）：
// 「没配」不等于「不设防」，否则新部署会悄悄裸奔。
func (s *Server) adminAuthorized(r *http.Request) bool {
	if s.adminToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(AdminTokenHeader)), []byte(s.adminToken)) == 1
}

// adminDeniedReason 说明 401 的具体原因：没配令牌，还是令牌不对。
// 两种情形都是 401，但页面要把「去设 env」和「令牌填错了」区分开。
func (s *Server) adminDeniedReason() string {
	if s.adminToken == "" {
		return "未设置 DRAMA_HUB_ADMIN_TOKEN，写操作一律拒绝"
	}
	return "写操作需要正确的 " + AdminTokenHeader + " 请求头"
}

// writeJSON 写出 JSON 响应。Content-Length 取序列化后的字节数，不留给 chunked。
func writeJSON(w http.ResponseWriter, status int, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("序列化响应: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// writeBody 写出非 JSON 响应（页面与静态资源），同样给出精确 Content-Length。
func writeBody(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// decodeJSON 解析请求体到 dst，格式错误归为 httpx.ErrInvalid（400）。
//
// 请求体上限 64 KiB：管理面只收 id、label 与集 id 列表，不该有更大的载荷。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: 请求体不是合法 JSON: %v", httpx.ErrInvalid, err)
	}
	return nil
}

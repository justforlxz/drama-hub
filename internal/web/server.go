package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/scheduler"
	"github.com/justforlxz/drama-hub/internal/store"
)

// SeriesSyncer 是「新增剧目」的依赖：按上游剧 id 把一个剧同步入库。生产实现是
// *sync.Syncer（签名一致，见 internal/sync），测试注入假实现。
//
// 接口定义在消费方（docs/DEV-STANDARDS.md「代码组织」）：本包只用这一个方法，
// 不 import sync / plugin，也不预建别的数据源方法。
type SeriesSyncer interface {
	SyncSeries(ctx context.Context, st *store.Store, sid string) error
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

// Server 汇总管理 API 与三页管理界面。它实现 http.Handler，由 cmd 挂到 `/` 上；
// 完整路径（含 `/api` 前缀）原样传到本包，ServeMux 不剥前缀。
type Server struct {
	st       *store.Store
	settings *store.Settings
	syncer   SeriesSyncer
	schedule Schedule
	sources  []Source
	// adminToken 是写操作的令牌（DRAMA_HUB_ADMIN_TOKEN）。空 = 没配 = 写操作一律 401。
	adminToken string
	logger     *slog.Logger
	mux        *http.ServeMux
}

// Schedule 是刷新调度的依赖：手动触发一轮，以及状态页要显示的调度现状。
// 生产实现是 *scheduler.Scheduler（签名一致）；测试注入假实现。
//
// 接口定义在消费方：本包只要这两个动作，不关心三段、单飞锁与游标。
type Schedule interface {
	Trigger(ctx context.Context, source string) error
	Status(ctx context.Context) ([]scheduler.SourceStatus, error)
}

// NewServer 构造路由表。st / settings / syncer / schedule 不可为 nil；sources 是
// 状态页要列的播放源（装配根按注册表给，空表示没有源）；adminToken 是写操作的令牌
// （`DRAMA_HUB_ADMIN_TOKEN`，空 = 没配 = 写操作一律 401）；logger 用于请求与同步日志。
//
// 路由分两类：三页界面（`/`、`/merges`、`/status` 与内嵌的 `/static/`）与
// 管理 API（`/api/` 子树）。不注册 catch-all：未登记路径由 ServeMux 兜底 404，
// 这是「未知路径 → 404」的前提（cmd 的装配注释同此约定）。
//
// 写操作（非 GET/HEAD/OPTIONS）在 ServeHTTP 里先过令牌，路由表本身不重复这件事：
// 新增写端点不需要记得加鉴权，忘不了。
func NewServer(st *store.Store, settings *store.Settings, syncer SeriesSyncer, schedule Schedule, sources []Source, adminToken string, logger *slog.Logger) *Server {
	s := &Server{
		st:         st,
		settings:   settings,
		syncer:     syncer,
		schedule:   schedule,
		sources:    sources,
		adminToken: adminToken,
		logger:     logger,
		mux:        http.NewServeMux(),
	}

	// 页面与静态资源：模板与 CSS/JS 随二进制嵌入（ADR-0002，无构建步骤）。
	s.mux.HandleFunc("GET /{$}", s.handleDramasPage)
	s.mux.HandleFunc("GET /merges", s.handleMergesPage)
	s.mux.HandleFunc("GET /status", s.handleStatusPage)
	s.mux.HandleFunc("GET /static/{file}", s.handleStatic)

	// 管理 API：路径即契约，页面 JS 只调这些端点（见 docs/DEV-STANDARDS.md）。
	s.mux.HandleFunc("GET /api/dramas", s.handleListDramas)
	s.mux.HandleFunc("POST /api/dramas", s.handleCreateDrama)
	s.mux.HandleFunc("DELETE /api/dramas/{id}", s.handleDeleteDrama)
	s.mux.HandleFunc("GET /api/dramas/{id}/episodes", s.handleListEpisodes)
	s.mux.HandleFunc("GET /api/dramas/{id}/merge-groups", s.handleListMergeGroups)
	s.mux.HandleFunc("POST /api/dramas/{id}/merge-groups", s.handleCreateMergeGroup)
	s.mux.HandleFunc("DELETE /api/merge-groups/{id}", s.handleDeleteMergeGroup)
	s.mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("PATCH /api/settings", s.handlePatchSettings)
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
// `POST /api/dramas` 能触发同步、`DELETE` 能删库，加写配置端点会同时放大它。
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

// pathID 解析路径里的数字主键，非正整数归为 httpx.ErrInvalid（400）。
func pathID(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: %s=%q 不是正整数", httpx.ErrInvalid, name, raw)
	}
	return id, nil
}

// mapStoreErr 把 store 的哨兵接到 httpx 的传输层类别：ErrNotFound → 404、
// ErrInvalid → 400，其余原样返回（StatusCode 兜底 500）。
func mapStoreErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("%w: %v", httpx.ErrNotFound, err)
	case errors.Is(err, store.ErrInvalid):
		return fmt.Errorf("%w: %v", httpx.ErrInvalid, err)
	default:
		return err
	}
}

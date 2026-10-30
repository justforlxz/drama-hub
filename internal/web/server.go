package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

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

// Source 是一个播放源在状态页上的展示身份。装配根从注册表取名字（= Plugin.Name()），
// 配一句人看得懂的标签；本包不 import plugin，也不猜有哪些源。
//
// 其余状态（是否启用、生效配置、不可用原因）由本包每请求现读 settings 得出，
// 因此改配置下一次读即生效（硬约束 5）。
type Source struct {
	Name  string // = Plugin.Name()，也是 drama.provider / episode.source 的值
	Label string // 状态页展示用；空 = 只显示 Name
}

// Server 汇总管理 API 与三页管理界面。它实现 http.Handler，由 cmd 挂到 `/` 上；
// 完整路径（含 `/api` 前缀）原样传到本包，ServeMux 不剥前缀。
type Server struct {
	st       *store.Store
	settings *store.Settings
	syncer   SeriesSyncer
	schedule Schedule
	sources  []Source
	logger   *slog.Logger
	mux      *http.ServeMux
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
// 状态页要列的播放源（装配根按注册表给，空表示没有源）；logger 用于请求与同步日志。
//
// 路由分两类：三页界面（`/`、`/merges`、`/status` 与内嵌的 `/static/`）与
// 管理 API（`/api/` 子树）。不注册 catch-all：未登记路径由 ServeMux 兜底 404，
// 这是「未知路径 → 404」的前提（cmd 的装配注释同此约定）。
func NewServer(st *store.Store, settings *store.Settings, syncer SeriesSyncer, schedule Schedule, sources []Source, logger *slog.Logger) *Server {
	s := &Server{
		st:       st,
		settings: settings,
		syncer:   syncer,
		schedule: schedule,
		sources:  sources,
		logger:   logger,
		mux:      http.NewServeMux(),
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
	return s
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
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

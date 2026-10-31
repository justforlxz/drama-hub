package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/plugin"
	"github.com/justforlxz/drama-hub/internal/scheduler"
	"github.com/justforlxz/drama-hub/internal/store"
)

// fakeSyncer 冒充数据源：把上游剧 id 变成库里的一部剧 + 两集，形状与
// sync.Syncer.SyncSeries 一致（写剧 → 写集 → 重建派生表）。
//
// 真实插件的取数与解析由 internal/plugin/hongguo 与 internal/sync 自己覆盖；
// 这里只关心管理面「点了同步之后 API 回了什么」。
type fakeSyncer struct {
	sid   string
	calls int
	err   error
}

// Name 与下面 SyncSeries 写库用的 provider 一致（生产里是插件名 hongguo）；
// 建剧 handler 拿它查「这部剧是否已存在」（MKACG-623）。
func (f *fakeSyncer) Name() string { return "web" }

func (f *fakeSyncer) SyncSeries(ctx context.Context, st *store.Store, sid string) error {
	f.calls++
	f.sid = sid
	if f.err != nil {
		return f.err
	}
	id, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "web", ProviderID: sid, Title: "同步剧 " + sid,
		EpisodeTotal: 2, SyncAt: "2026-10-30T08:00:00Z",
	})
	if err != nil {
		return err
	}
	for i := 1; i <= 2; i++ {
		if _, err := st.UpsertEpisode(ctx, store.Episode{
			DramaID: id, EpNo: i, Source: "web", Vid: "vid" + string(rune('0'+i)),
		}); err != nil {
			return err
		}
	}
	_, err = st.RebuildEmbyItems(ctx, id)
	return err
}

// fakeSchedule 是调度依赖的替身：状态页要显示的调度现状由用例直接给定，
// 手动触发只记下被要求刷新的源名与要返回的错误。
//
// 真实的三段、单飞锁与游标由 internal/scheduler 自己覆盖；这里只关心管理面
// 「点了立即刷新之后 API 回了什么」。
type fakeSchedule struct {
	status    []scheduler.SourceStatus
	statusErr error
	trigger   error
	triggered []string
}

func (f *fakeSchedule) Trigger(_ context.Context, source string) error {
	f.triggered = append(f.triggered, source)
	return f.trigger
}

func (f *fakeSchedule) Status(context.Context) ([]scheduler.SourceStatus, error) {
	return f.status, f.statusErr
}

// newTestServer 起一套真实依赖：临时 SQLite + 真 handler。不 mock 自己的 handler。
func newTestServer(t *testing.T) (*Server, *store.Store, *fakeSyncer) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	syncer := &fakeSyncer{}
	// 状态页的播放源清单由装配根给（生产里来自注册表），测试固定一个源。
	sources := []Source{{Name: "web", Label: "红果官网 SSR"}}
	return NewServer(st, store.NewSettings(db.DB), syncer, &fakeSchedule{}, sources, testAdminToken,
		slog.New(slog.DiscardHandler)), st, syncer
}

// testAdminToken 是测试里的管理令牌：写操作（非 GET/HEAD/OPTIONS）要带它，
// 与生产里 DRAMA_HUB_ADMIN_TOKEN 的作用相同。
const testAdminToken = "test-admin-token"

// newRequest 造一个带管理令牌的请求。鉴权用例自己拿它去掉/改坏令牌头。
func newRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(AdminTokenHeader, testAdminToken)
	return req
}

// do 发一个真实请求，返回响应。
func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, newRequest(method, path, body))
	return rec
}

// seedDrama 插入一部剧及其集（vids 的数量即集数），并重建派生表 —— 与生产里
// 同步完成后的状态一致。直链不落库（0004 已删 play_url），这里没有可播集的概念。
func seedDrama(t *testing.T, st *store.Store, providerID, title string, vids []string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "web", ProviderID: providerID, Title: title, Year: 2024,
		EpisodeTotal: len(vids), SyncAt: "2026-10-30T08:00:00Z",
	})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	for i, vid := range vids {
		ep := store.Episode{DramaID: id, EpNo: i + 1, Title: "第 " + vid + " 集", Source: "web", Vid: vid}
		if _, err := st.UpsertEpisode(ctx, ep); err != nil {
			t.Fatalf("UpsertEpisode: %v", err)
		}
	}
	if _, err := st.RebuildEmbyItems(ctx, id); err != nil {
		t.Fatalf("RebuildEmbyItems: %v", err)
	}
	return id
}

// decode 解析 JSON 响应体；顺带校验 Content-Length 与 body 长度一致。
func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if cl := rec.Header().Get("Content-Length"); cl != "" && cl != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %q, body = %d 字节", cl, rec.Body.Len())
	}
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, rec.Body.String())
	}
	return out
}

// TestAPI_ListDramas 覆盖剧目列表的形状与计数：episodes / emby_items /
// merge_groups 都来自库内真实数据。
func TestAPI_ListDramas(t *testing.T) {
	srv, st, _ := newTestServer(t)
	id := seedDrama(t, st, "1001", "宴律", []string{"a", "b", "c"})
	if _, err := st.CreateMergeGroup(context.Background(), id, "第 2-3 集", []int64{
		mustEpisodeID(t, st, id, 2), mustEpisodeID(t, st, id, 3),
	}); err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}

	rec := do(t, srv, http.MethodGet, "/api/dramas", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	got := decode[dramaList](t, rec)
	if len(got.Dramas) != 1 {
		t.Fatalf("剧目数 = %d, 期望 1", len(got.Dramas))
	}
	d := got.Dramas[0]
	if d.ProviderID != "1001" || d.Title != "宴律" || d.Year != 2024 {
		t.Errorf("剧目字段 = %+v", d)
	}
	if d.EpisodeTotal != 3 || d.Episodes != 3 {
		t.Errorf("集数 = %d/%d, 期望 3/3", d.Episodes, d.EpisodeTotal)
	}
	// 3 集里两集并成 1 条对外剧集 + 剩下 1 集 = 2。
	if d.EmbyItems != 2 {
		t.Errorf("对外集 = %d, 期望 2", d.EmbyItems)
	}
	if d.MergeGroups != 1 {
		t.Errorf("合并组 = %d, 期望 1", d.MergeGroups)
	}
	if d.SyncAt != "2026-10-30T08:00:00Z" {
		t.Errorf("sync_at = %q", d.SyncAt)
	}
}

// TestAPI_CreateDrama 覆盖「增剧目」：输入可以是 /player/ 链接，落库由数据源完成，
// 响应与随后的 GET 完全一致（页面直接套用响应，二者必须同形同值）。
func TestAPI_CreateDrama(t *testing.T) {
	srv, _, syncer := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/api/dramas",
		`{"series_id":"https://hongguoduanju.com/player/7503745441910017049"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 201（body=%s）", rec.Code, rec.Body.String())
	}
	if syncer.sid != "7503745441910017049" {
		t.Errorf("传给数据源的 sid = %q, 期望从链接里剥出的 7503745441910017049", syncer.sid)
	}
	created := decode[dramaList](t, rec)
	if len(created.Dramas) != 1 || created.Dramas[0].Episodes != 2 {
		t.Fatalf("创建响应 = %+v", created)
	}

	after := decode[dramaList](t, do(t, srv, http.MethodGet, "/api/dramas", ""))
	if len(after.Dramas) != len(created.Dramas) || after.Dramas[0] != created.Dramas[0] {
		t.Errorf("POST 响应与 GET 不一致：\nPOST=%+v\nGET =%+v", created.Dramas, after.Dramas)
	}
}

// TestAPI_CreateDrama_RepeatReturnsOK 锁定 PL 裁定（MKACG-623 G7）：新建 201、
// 已存在 200。重复 POST 同一 series_id 时库里没有新行，回 Created 会让调用方以为
// 每次都产生了新行；响应体仍是整份列表，与 GET 一致。
func TestAPI_CreateDrama_RepeatReturnsOK(t *testing.T) {
	srv, st, syncer := newTestServer(t)

	first := do(t, srv, http.MethodPost, "/api/dramas", `{"series_id":"1001"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("首次 POST 状态码 = %d, 期望 201（body=%s）", first.Code, first.Body.String())
	}
	second := do(t, srv, http.MethodPost, "/api/dramas", `{"series_id":"1001"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("重复 POST 状态码 = %d, 期望 200（body=%s）", second.Code, second.Body.String())
	}
	if syncer.calls != 2 {
		t.Errorf("数据源调用次数 = %d, 期望 2（重复同步仍要回源）", syncer.calls)
	}
	dramas, err := st.ListDramas(context.Background())
	if err != nil {
		t.Fatalf("ListDramas: %v", err)
	}
	if len(dramas) != 1 {
		t.Errorf("重复同步后剧目数 = %d, 期望 1（不许建出第二行）", len(dramas))
	}
	repeated := decode[dramaList](t, second)
	after := decode[dramaList](t, do(t, srv, http.MethodGet, "/api/dramas", ""))
	if len(repeated.Dramas) != 1 || len(after.Dramas) != 1 || repeated.Dramas[0] != after.Dramas[0] {
		t.Errorf("重复 POST 响应与 GET 不一致：\nPOST=%+v\nGET =%+v", repeated.Dramas, after.Dramas)
	}
}

// TestAPI_CreateDrama_InvalidID 覆盖非法输入：非数字 id 直接 400，不触达数据源。
func TestAPI_CreateDrama_InvalidID(t *testing.T) {
	srv, _, syncer := newTestServer(t)
	for _, body := range []string{
		`{"series_id":""}`,
		`{"series_id":"abc"}`,
		`{"series_id":"../../etc/passwd"}`,
		`不是 JSON`,
	} {
		rec := do(t, srv, http.MethodPost, "/api/dramas", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%q 状态码 = %d, 期望 400", body, rec.Code)
		}
		got := decode[errorBodyShape](t, rec)
		if got.Code != "invalid" {
			t.Errorf("body=%q code = %q, 期望 invalid", body, got.Code)
		}
	}
	if syncer.calls != 0 {
		t.Errorf("数据源被调用 %d 次, 期望 0", syncer.calls)
	}
}

// TestAPI_CreateDrama_SyncFails 覆盖数据源失败：502 + 错误原因，且不落半截数据。
func TestAPI_CreateDrama_SyncFails(t *testing.T) {
	srv, st, syncer := newTestServer(t)
	syncer.err = errors.New("GET /player/1 返回 500")
	rec := do(t, srv, http.MethodPost, "/api/dramas", `{"series_id":"1"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", rec.Code)
	}
	got := decode[errorBodyShape](t, rec)
	if got.Code != "upstream" || !strings.Contains(got.Message, "返回 500") {
		t.Errorf("错误体 = %+v", got)
	}
	dramas, err := st.ListDramas(context.Background())
	if err != nil {
		t.Fatalf("ListDramas: %v", err)
	}
	if len(dramas) != 0 {
		t.Errorf("同步失败后库里还有 %d 部剧, 期望 0", len(dramas))
	}
}

// TestAPI_DeleteDrama 覆盖删剧目：204、列表清空、级联删集、重复删除 404、坏 id 400。
func TestAPI_DeleteDrama(t *testing.T) {
	srv, st, _ := newTestServer(t)
	id := seedDrama(t, st, "2002", "带删除的剧", []string{"a", "b"})

	rec := do(t, srv, http.MethodDelete, "/api/dramas/"+strconv.Itoa(int(id)), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("状态码 = %d, 期望 204", rec.Code)
	}
	eps, err := st.ListEpisodes(context.Background(), id)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(eps) != 0 {
		t.Errorf("删除后仍有 %d 集, 期望级联清空", len(eps))
	}
	if got := decode[dramaList](t, do(t, srv, http.MethodGet, "/api/dramas", "")); len(got.Dramas) != 0 {
		t.Errorf("列表仍有 %d 部剧", len(got.Dramas))
	}

	if rec := do(t, srv, http.MethodDelete, "/api/dramas/"+strconv.Itoa(int(id)), ""); rec.Code != http.StatusNotFound {
		t.Errorf("重复删除状态码 = %d, 期望 404", rec.Code)
	}
	if rec := do(t, srv, http.MethodDelete, "/api/dramas/abc", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("坏 id 状态码 = %d, 期望 400", rec.Code)
	}
}

// TestAPI_Episodes 覆盖合并编排页的集列表：按 ep_no 升序、只回库里的字段。
func TestAPI_Episodes(t *testing.T) {
	srv, st, _ := newTestServer(t)
	id := seedDrama(t, st, "3003", "选集的剧", []string{"a", "b", "c"})

	got := decode[episodeList](t, do(t, srv, http.MethodGet, "/api/dramas/"+strconv.Itoa(int(id))+"/episodes", ""))
	if len(got.Episodes) != 3 {
		t.Fatalf("集数 = %d, 期望 3", len(got.Episodes))
	}
	for i, ep := range got.Episodes {
		if ep.EpNo != i+1 {
			t.Errorf("第 %d 项 ep_no = %d", i, ep.EpNo)
		}
		if ep.Vid != []string{"a", "b", "c"}[i] {
			t.Errorf("ep_no=%d vid = %q", ep.EpNo, ep.Vid)
		}
	}
	if rec := do(t, srv, http.MethodGet, "/api/dramas/99999/episodes", ""); rec.Code != http.StatusNotFound {
		t.Errorf("不存在的剧状态码 = %d, 期望 404", rec.Code)
	}
}

// TestAPI_MergeGroups 覆盖合并编排：成员顺序 = 提交顺序、POST 响应与 GET 一致、
// 校验失败 400 且不落库、删除后列表清空。
func TestAPI_MergeGroups(t *testing.T) {
	srv, st, _ := newTestServer(t)
	dramaID := seedDrama(t, st, "4004", "编排的剧", []string{"a", "b", "c", "d"})
	path := "/api/dramas/" + strconv.Itoa(int(dramaID)) + "/merge-groups"
	e1, e2, e3, e4 := mustEpisodeID(t, st, dramaID, 1), mustEpisodeID(t, st, dramaID, 2),
		mustEpisodeID(t, st, dramaID, 3), mustEpisodeID(t, st, dramaID, 4)

	// 故意倒序提交：ord 必须按提交顺序，不是按 ep_no。
	body := `{"label":"第 3-1 集","episode_ids":[` + strconv.Itoa(int(e3)) + `,` + strconv.Itoa(int(e1)) + `]}`
	rec := do(t, srv, http.MethodPost, path, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 201（body=%s）", rec.Code, rec.Body.String())
	}
	created := decode[mergeGroupList](t, rec)
	if len(created.Groups) != 1 {
		t.Fatalf("合并组数 = %d, 期望 1", len(created.Groups))
	}
	members := created.Groups[0].Members
	if len(members) != 2 || members[0].EpisodeID != e3 || members[1].EpisodeID != e1 {
		t.Fatalf("成员顺序 = %+v, 期望 [E3, E1]", members)
	}
	if members[0].Ord != 1 || members[1].Ord != 2 {
		t.Errorf("ord = %d/%d, 期望 1/2", members[0].Ord, members[1].Ord)
	}

	listed := decode[mergeGroupList](t, do(t, srv, http.MethodGet, path, ""))
	if len(listed.Groups) != 1 || listed.Groups[0].ID != created.Groups[0].ID ||
		len(listed.Groups[0].Members) != 2 || listed.Groups[0].Members[0] != members[0] {
		t.Errorf("POST 响应与 GET 不一致：\nPOST=%+v\nGET =%+v", created.Groups, listed.Groups)
	}

	// 只有 1 集 → store.ErrInvalid → 400，且不落库。
	if rec := do(t, srv, http.MethodPost, path,
		`{"label":"单集","episode_ids":[`+strconv.Itoa(int(e4))+`]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("单集建组状态码 = %d, 期望 400", rec.Code)
	}
	// 与新组重叠 → 400。
	if rec := do(t, srv, http.MethodPost, path,
		`{"label":"重叠","episode_ids":[`+strconv.Itoa(int(e3))+`,`+strconv.Itoa(int(e2))+`]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("重叠建组状态码 = %d, 期望 400", rec.Code)
	}
	if got := decode[mergeGroupList](t, do(t, srv, http.MethodGet, path, "")); len(got.Groups) != 1 {
		t.Errorf("校验失败后合并组数 = %d, 期望 1（不能落库）", len(got.Groups))
	}

	// 删组：204，列表清空；再删同一个组 / 不存在的组都是 404。
	if rec := do(t, srv, http.MethodDelete, "/api/merge-groups/"+strconv.Itoa(int(created.Groups[0].ID)), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("删组状态码 = %d, 期望 204", rec.Code)
	}
	if got := decode[mergeGroupList](t, do(t, srv, http.MethodGet, path, "")); len(got.Groups) != 0 {
		t.Errorf("删组后仍有 %d 个合并组", len(got.Groups))
	}
	if rec := do(t, srv, http.MethodDelete, "/api/merge-groups/99999", ""); rec.Code != http.StatusNotFound {
		t.Errorf("删不存在的组状态码 = %d, 期望 404", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/api/dramas/99999/merge-groups", ""); rec.Code != http.StatusNotFound {
		t.Errorf("不存在的剧查合并组状态码 = %d, 期望 404", rec.Code)
	}
}

// TestAPI_Status 覆盖看板契约：计数、配置默认值、缓存面、密钥只报「已设置」。
func TestAPI_Status(t *testing.T) {
	srv, st, _ := newTestServer(t)
	const secret = "0123456789abcdef0123456789abcdef"
	if err := srv.settings.Set(context.Background(), store.KeyStreamSecret, secret); err != nil {
		t.Fatalf("Set stream_secret: %v", err)
	}
	seedDrama(t, st, "5005", "看板的剧", []string{"a", "b"})

	rec := do(t, srv, http.MethodGet, "/api/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Error("响应体里出现 stream_secret 明文")
	}
	got := decode[statusView](t, rec)
	if got.Totals.Dramas != 1 || got.Totals.Episodes != 2 ||
		got.Totals.EmbyItems != 2 || got.Totals.LastSyncAt != "2026-10-30T08:00:00Z" {
		t.Errorf("totals = %+v", got.Totals)
	}
	if got.Config.SegTargetSec != 10 || got.Config.CacheDir != "./data/cache" ||
		got.Config.CacheMaxMB != 8192 || got.Config.CacheLRUDays != 30 {
		t.Errorf("配置默认值 = %+v, 期望 10 / ./data/cache / 8192 / 30", got.Config)
	}
	if !got.Config.StreamSecretSet {
		t.Error("stream_secret_set = false, 期望 true")
	}
	if got.Config.WebDAVUserSet || got.Config.WebDAVPassSet {
		t.Error("webdav 凭据未写入却报已设置")
	}
	if len(got.Sources) != 1 {
		t.Fatalf("sources = %+v, 期望 1 个源", got.Sources)
	}
	if src := got.Sources[0]; src.Name != "web" || src.Label != "红果官网 SSR" ||
		!src.Enabled || src.RequestTimeoutSec != 30 || src.Reason != "" {
		t.Errorf("source = %+v, 期望 web 默认启用、超时 30 秒、无原因", src)
	}
	// 构造期配置：测试里的装配根没带 Boot* 值，现值与生效值不同 → 报「需重启」。
	// 这是网页上「改了要重启」那一行的数据来源。
	if src := got.Sources[0]; src.BaseURLEffective != "" ||
		src.RequestTimeoutSecEffective != 0 || !src.RestartRequired {
		t.Errorf("source = %+v, 期望 effective 全零且 restart_required", src)
	}
	// 状态页只报真正生效的配置：ADR-0004 的 rclone 建议参数（cache.*）已整段删除
	// （MKACG-616 / 617 结论），响应里不该再有这个键。
	if strings.Contains(rec.Body.String(), `"cache"`) {
		t.Errorf("响应体仍出现 cache 段：%s", rec.Body.String())
	}
}

// 构造期配置（base_url / request_timeout_sec）不是热读项：状态页要能区分「正在生效
// 的值」与「settings 现值」，两者不一致时置 restart_required —— 页面上就是「改了要
// 重启」那一行，避免显示新值却还在跑旧值。
func TestAPI_StatusRestartRequired(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()
	key := func(item string) store.SettingKey { return store.PluginSettingKey("web", item) }
	// 启动时注入的值：base_url 空（插件内置地址）、超时 30 秒。
	srv.sources = []Source{{Name: "web", Label: "红果官网 SSR", BootRequestTimeoutSec: 30}}
	got := decode[statusView](t, do(t, srv, http.MethodGet, "/api/status", ""))
	if src := got.Sources[0]; src.RestartRequired || src.BaseURLEffective != "" ||
		src.RequestTimeoutSecEffective != 30 {
		t.Fatalf("source = %+v, 期望与启动值一致、不报需重启", src)
	}

	// 改现值（等价于手改 settings）：不重启时生效值不动、标出需重启。
	if err := srv.settings.Set(ctx, key(store.PluginItemBaseURL), "http://127.0.0.1:18080"); err != nil {
		t.Fatalf("Set base_url: %v", err)
	}
	if err := srv.settings.Set(ctx, key(store.PluginItemRequestTimeout), "5"); err != nil {
		t.Fatalf("Set request_timeout_sec: %v", err)
	}
	got = decode[statusView](t, do(t, srv, http.MethodGet, "/api/status", ""))
	src := got.Sources[0]
	if !src.RestartRequired || src.BaseURLEffective != "" || src.RequestTimeoutSecEffective != 30 {
		t.Fatalf("source = %+v, 期望现值已变、生效值不变、需重启", src)
	}
	if src.BaseURL != "http://127.0.0.1:18080" || src.RequestTimeoutSec != 5 {
		t.Fatalf("source = %+v, 现值应为新值", src)
	}
}

// TestAPI_UnknownRoutes 锁定管理 API 的兜底：未登记路径 404、错方法 405。
func TestAPI_UnknownRoutes(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if rec := do(t, srv, http.MethodGet, "/api/nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/nope 状态码 = %d, 期望 404", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/api/", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/ 状态码 = %d, 期望 404", rec.Code)
	}
	if rec := do(t, srv, http.MethodPut, "/api/dramas", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/dramas 状态码 = %d, 期望 405", rec.Code)
	}
}

// TestNormalizeSeriesID 覆盖管理面输入的归一（纯函数，无 I/O）。
func TestNormalizeSeriesID(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"7503745441910017049", "7503745441910017049", true},
		{"  7503745441910017049 ", "7503745441910017049", true},
		{"https://hongguoduanju.com/player/7503745441910017049", "7503745441910017049", true},
		{"https://hongguoduanju.com/player/7503745441910017049/", "7503745441910017049", true},
		{"https://hongguoduanju.com/player/7503745441910017049?from=list", "7503745441910017049", true},
		{"https://hongguoduanju.com/player/7503745441910017049/vid123", "7503745441910017049", true},
		{"", "", false},
		{"abc", "", false},
		{"7503745441910017049abc", "", false},
		{"../../../etc/passwd", "", false},
		{"123456789012345678901", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeSeriesID(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("normalizeSeriesID(%q) = (%q, %v), 期望 (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// errorBodyShape 是 httpx 统一错误体在测试侧的最小形态。
type errorBodyShape struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// mustEpisodeID 取某剧第 no 集的 id。
func mustEpisodeID(t *testing.T, st *store.Store, dramaID int64, no int) int64 {
	t.Helper()
	eps, err := st.ListEpisodes(context.Background(), dramaID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	for _, ep := range eps {
		if ep.EpNo == no {
			return ep.ID
		}
	}
	t.Fatalf("剧 %d 没有第 %d 集", dramaID, no)
	return 0
}

// 插件被停用时看板给出原因：停用不是故障，但必须看得见（AC：插件未启用/配置缺失
// 照常启动，状态页可见原因）。
func TestAPI_StatusSourceDisabled(t *testing.T) {
	srv, _, _ := newTestServer(t)
	key := store.PluginSettingKey("web", store.PluginItemEnabled)
	if err := srv.settings.Set(context.Background(), key, "false"); err != nil {
		t.Fatalf("Set %s: %v", key, err)
	}

	got := decode[statusView](t, do(t, srv, http.MethodGet, "/api/status", ""))
	if len(got.Sources) != 1 {
		t.Fatalf("sources = %+v, 期望 1 个源", got.Sources)
	}
	if got.Sources[0].Enabled {
		t.Error("plugin.web.enabled=false 却报 enabled=true")
	}
	if !strings.Contains(got.Sources[0].Reason, string(key)+"=false") {
		t.Errorf("reason = %q, 期望带上停用的配置键", got.Sources[0].Reason)
	}
}

// scheduleOf 取出 newTestServer 装上的假调度依赖。
func scheduleOf(srv *Server) *fakeSchedule { return srv.schedule.(*fakeSchedule) }

// TestAPI_Refresh 覆盖手动触发的状态码矩阵（docs/SCHEDULER-MODEL.md「手动触发」）：
// 202 已启动 / 409 该源正在跑 / 404 无此源或未启用 / 400 请求体非法。
func TestAPI_Refresh(t *testing.T) {
	cases := []struct {
		name    string
		trigger error
		body    string
		want    int
	}{
		{"已启动", nil, `{"source":"web"}`, http.StatusAccepted},
		{"正在跑", scheduler.ErrSourceBusy, `{"source":"web"}`, http.StatusConflict},
		{"没有这个源", scheduler.ErrSourceNotFound, `{"source":"web"}`, http.StatusNotFound},
		{"已停用", scheduler.ErrSourceDisabled, `{"source":"web"}`, http.StatusNotFound},
		{"请求体非法", nil, `不是 JSON`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			sched := scheduleOf(srv)
			sched.trigger = c.trigger

			rec := do(t, srv, http.MethodPost, "/api/refresh", c.body)
			if rec.Code != c.want {
				t.Fatalf("状态码 = %d, 期望 %d（body=%s）", rec.Code, c.want, rec.Body.String())
			}
			if c.trigger == nil && c.body == `{"source":"web"}` {
				got := decode[refreshView](t, rec)
				if got.Source != "web" || !got.Started {
					t.Errorf("响应 = %+v, 期望 web + started", got)
				}
				if len(sched.triggered) != 1 || sched.triggered[0] != "web" {
					t.Errorf("触发记录 = %v, 期望 [web]", sched.triggered)
				}
			}
		})
	}
}

// TestAPI_StatusSchedule 覆盖状态页的调度字段：上次运行 / 上次成功 / 上次错误 /
// 正在跑 / 生效间隔 / 下次时间全部来自调度器，时间是 UTC RFC3339 文本。
func TestAPI_StatusSchedule(t *testing.T) {
	srv, _, _ := newTestServer(t)
	lastRun := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	lastOK := lastRun.Add(5 * time.Minute)
	nextDue := lastRun.Add(15 * time.Minute)
	scheduleOf(srv).status = []scheduler.SourceStatus{{
		Name: "web", Enabled: true, Running: true,
		LastRunAt: lastRun, LastOkAt: lastOK,
		LastError:   "2026-10-30T10:00:00Z 读取 sitemap 超时",
		IntervalSec: 900, NextDueAt: nextDue,
	}}

	got := decode[statusView](t, do(t, srv, http.MethodGet, "/api/status", ""))
	if len(got.Sources) != 1 {
		t.Fatalf("sources = %+v, 期望 1 个源", got.Sources)
	}
	src := got.Sources[0]
	if !src.Running || src.IntervalSec != 900 {
		t.Errorf("调度字段 = %+v, 期望 running + 900 秒", src)
	}
	if src.LastRunAt != "2026-10-30T09:00:00Z" || src.LastOkAt != "2026-10-30T09:05:00Z" ||
		src.NextDueAt != "2026-10-30T09:15:00Z" {
		t.Errorf("时间字段 = %q / %q / %q", src.LastRunAt, src.LastOkAt, src.NextDueAt)
	}
	if !strings.Contains(src.LastError, "读取 sitemap 超时") {
		t.Errorf("last_error = %q", src.LastError)
	}
}

// TestAPI_CreateDrama_DisabledSourceStillWorks 锁定 PL 裁定：plugin.<源>.enabled=false
// 的语义是「不参与刷新调度」，手工单剧补录（POST /api/dramas）照常可用。
func TestAPI_CreateDrama_DisabledSourceStillWorks(t *testing.T) {
	srv, _, syncer := newTestServer(t)
	if err := srv.settings.Set(context.Background(),
		store.PluginSettingKey("web", store.PluginItemEnabled), "false"); err != nil {
		t.Fatalf("Set enabled: %v", err)
	}

	rec := do(t, srv, http.MethodPost, "/api/dramas", `{"series_id":"1001"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("停用源的 POST /api/dramas 状态码 = %d, 期望 201（body=%s）", rec.Code, rec.Body.String())
	}
	if syncer.calls != 1 {
		t.Errorf("数据源调用次数 = %d, 期望 1", syncer.calls)
	}
}

// realSource 是「手动触发 + 真调度器」用例里的数据源：一页一个标识。
type realSource struct{ gotRefs int }

func (s *realSource) Name() string { return "web" }

func (s *realSource) Enumerate(ctx context.Context, _ *plugin.Cursor) (plugin.Page, error) {
	s.gotRefs++
	if s.gotRefs > 1 {
		return plugin.Page{}, nil
	}
	return plugin.Page{Refs: []plugin.WorkRef{{Plugin: "web", ID: "1001"}}}, nil
}

func (s *realSource) Work(context.Context, plugin.WorkRef) (plugin.WorkDetail, error) {
	return plugin.WorkDetail{}, errors.New("Work 不参与调度测试")
}

func (s *realSource) Resolve(context.Context, plugin.WorkRef, int) (plugin.Stream, error) {
	return plugin.Stream{}, plugin.ErrNoStream
}

func (s *realSource) Health(context.Context) error { return nil }

// gatedSyncer 让一轮停在写库阶段，好让第二次手动触发真的撞上单飞锁。
type gatedSyncer struct {
	entered chan struct{}
	gate    chan struct{}
}

func (g *gatedSyncer) Name() string { return "web" }

func (g *gatedSyncer) SyncSeries(ctx context.Context, _ *store.Store, _ string) error {
	g.entered <- struct{}{}
	select {
	case <-g.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// 端到端：真调度器 + 真 handler 验证「手动触发 202 → 撞车 409 → 跑完再 202」。
// 409 来自调度器的单飞锁，经过 internal/web 的哨兵映射，不靠假实现对齐。
func TestAPI_Refresh_RealScheduler(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "refresh.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	syncer := &gatedSyncer{entered: make(chan struct{}, 2), gate: make(chan struct{})}
	sched := scheduler.New(st, settings,
		[]plugin.Plugin{&realSource{}},
		map[string]scheduler.Syncer{"web": syncer},
		slog.New(slog.DiscardHandler))
	srv := NewServer(st, settings, syncer, sched, []Source{{Name: "web", Label: "红果官网 SSR"}},
		testAdminToken, slog.New(slog.DiscardHandler))

	if rec := do(t, srv, http.MethodPost, "/api/refresh", `{"source":"web"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("第一次手动触发状态码 = %d, 期望 202（body=%s）", rec.Code, rec.Body.String())
	}
	<-syncer.entered // 一轮已经进到写库阶段

	if rec := do(t, srv, http.MethodPost, "/api/refresh", `{"source":"web"}`); rec.Code != http.StatusConflict {
		t.Fatalf("撞车的第二次触发状态码 = %d, 期望 409（body=%s）", rec.Code, rec.Body.String())
	}

	close(syncer.gate)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !statusOf(t, srv).Running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if rec := do(t, srv, http.MethodPost, "/api/refresh", `{"source":"web"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("轮结束后的触发状态码 = %d, 期望 202", rec.Code)
	}
}

// statusOf 读一次 /api/status 里唯一的源。
func statusOf(t *testing.T, srv *Server) sourceView {
	t.Helper()
	got := decode[statusView](t, do(t, srv, http.MethodGet, "/api/status", ""))
	if len(got.Sources) != 1 {
		t.Fatalf("sources = %+v, 期望 1 个源", got.Sources)
	}
	return got.Sources[0]
}

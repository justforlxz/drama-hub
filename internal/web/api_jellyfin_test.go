package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/jellyfin"
	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/store"
)

// TestAPI_JellyfinLibraries 覆盖 POST /api/jellyfin/libraries 的三种结局（AC-11）：
// 202 已派出 / 409 上一轮还在跑 / 400 未配置且指出缺哪个键。请求体不再有参数：
// 删不删库由分类开关决定（MKACG-668）。
func TestAPI_JellyfinLibraries(t *testing.T) {
	t.Run("202 已派出", func(t *testing.T) {
		jf := &fakeJellyfin{}
		srv, _, _ := newTestServerWith(t, jf)
		rec := do(t, srv, http.MethodPost, "/api/jellyfin/libraries", "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("状态码 = %d, 期望 202（body=%s）", rec.Code, rec.Body.String())
		}
		if got := decode[jellyfinLibrariesResponse](t, rec); !got.Started {
			t.Errorf("响应 = %+v, 期望 started=true", got)
		}
		if jf.calls != 1 {
			t.Errorf("触发 %d 次, 期望 1 次", jf.calls)
		}
	})

	t.Run("409 上一轮还在跑", func(t *testing.T) {
		jf := &fakeJellyfin{startErr: fmt.Errorf("%w: 上一次还没结束", jellyfin.ErrBusy)}
		srv, _, _ := newTestServerWith(t, jf)
		rec := do(t, srv, http.MethodPost, "/api/jellyfin/libraries", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("状态码 = %d, 期望 409（body=%s）", rec.Code, rec.Body.String())
		}
	})

	t.Run("400 未配置且点名缺的键", func(t *testing.T) {
		jf := &fakeJellyfin{startErr: fmt.Errorf("%w: 缺少 jellyfin_url、jellyfin_api_key", jellyfin.ErrNotConfigured)}
		srv, _, _ := newTestServerWith(t, jf)
		rec := do(t, srv, http.MethodPost, "/api/jellyfin/libraries", "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("状态码 = %d, 期望 400（body=%s）", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "jellyfin_url") {
			t.Errorf("响应体 = %s, 期望指出缺哪个键", body)
		}
	})
}

// TestAPI_JellyfinCategories 覆盖分类开关的读写面（MKACG-668）：
// GET 列出全部分类与开关状态；PATCH 关掉一个 → 落库（下一次 GET 仍是关着）；
// 重新打开 → 行删除；缺 name / enabled、空分类名 → 400。
func TestAPI_JellyfinCategories(t *testing.T) {
	jf := &fakeJellyfin{categories: []jellyfin.CategoryState{
		{Name: "都市", Enabled: true},
		{Name: "爱情", Enabled: true},
	}}
	srv, _, _ := newTestServerWith(t, jf)

	rec := do(t, srv, http.MethodGet, "/api/jellyfin/categories", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	got := decode[jellyfinCategoriesResponse](t, rec)
	if len(got.Categories) != 2 || got.Categories[0] != (jellyfinCategoryView{Name: "都市", Enabled: true}) {
		t.Fatalf("分类列表 = %+v", got.Categories)
	}

	// 关掉一个：只写库，必须 200 且回显同值。
	rec = do(t, srv, http.MethodPatch, "/api/jellyfin/categories", `{"name":"爱情","enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if got := decode[jellyfinCategoryView](t, rec); got.Name != "爱情" || got.Enabled {
		t.Errorf("PATCH 响应 = %+v, 期望 name=爱情 enabled=false", got)
	}
	if jf.calls != 0 {
		t.Errorf("拨开关触发了 %d 轮同步，期望 0（删除只在下一轮同步里发生）", jf.calls)
	}
	// 真的落库了：把 Manager 的开关读到的最新状态换掉，再从 store 读回来的值才是真值。
	disabled, err := srv.st.DisabledJellyfinCategories(t.Context())
	if err != nil {
		t.Fatalf("DisabledJellyfinCategories: %v", err)
	}
	if len(disabled) != 1 || disabled[0] != "爱情" {
		t.Errorf("库里的关闭项 = %v, 期望 [爱情]", disabled)
	}

	// 重新打开：删行，GET 回到开启。
	rec = do(t, srv, http.MethodPatch, "/api/jellyfin/categories", `{"name":"爱情","enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("重新开启状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if disabled, err := srv.st.DisabledJellyfinCategories(t.Context()); err != nil || len(disabled) != 0 {
		t.Errorf("重新开启后关闭项 = %v, %v，期望空", disabled, err)
	}

	// 参数缺失 / 空分类名：400，且不落库。
	for _, body := range []string{
		`{"name":"都市"}`,
		`{"enabled":false}`,
		`{"name":"","enabled":false}`,
		`{"name":"都市","enabled":`,
	} {
		rec := do(t, srv, http.MethodPatch, "/api/jellyfin/categories", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s 状态码 = %d, 期望 400（body=%s）", body, rec.Code, rec.Body.String())
		}
	}
	if disabled, err := srv.st.DisabledJellyfinCategories(t.Context()); err != nil || len(disabled) != 0 {
		t.Errorf("非法请求后关闭项 = %v, %v，期望仍是空", disabled, err)
	}
}

// TestAPI_JellyfinCategoriesNeedsToken 锁定 PATCH 是写操作：没有令牌一律 401，
// 且不改库。
func TestAPI_JellyfinCategoriesNeedsToken(t *testing.T) {
	srv, _, _ := newTestServerWith(t, &fakeJellyfin{})
	req := newRequest(http.MethodPatch, "/api/jellyfin/categories", `{"name":"都市","enabled":false}`)
	req.Header.Del(AdminTokenHeader)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌状态码 = %d, 期望 401", rec.Code)
	}
	if disabled, err := srv.st.DisabledJellyfinCategories(t.Context()); err != nil || len(disabled) != 0 {
		t.Errorf("未授权请求改了库：%v, %v", disabled, err)
	}
}

// TestAPI_JellyfinLibrariesNeedsToken 锁定写操作鉴权对新增端点同样生效：
// 没有 X-Admin-Token 一律 401 且不触发。
func TestAPI_JellyfinLibrariesNeedsToken(t *testing.T) {
	jf := &fakeJellyfin{}
	srv, _, _ := newTestServerWith(t, jf)
	req := newRequest(http.MethodPost, "/api/jellyfin/libraries", "")
	req.Header.Del(AdminTokenHeader)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌状态码 = %d, 期望 401", rec.Code)
	}
	if jf.calls != 0 {
		t.Errorf("未授权请求触发了 %d 次媒体库同步", jf.calls)
	}
}

// TestAPI_StatusJellyfin 覆盖 /api/status 的 jellyfin 块（AC-1 / AC-8）：
// 配置现状、缺哪个键、上次结果都可见；密钥只有 api_key_set。
func TestAPI_StatusJellyfin(t *testing.T) {
	lastRun := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	jf := &fakeJellyfin{state: jellyfin.State{
		Configured:    true,
		Missing:       []string{},
		URL:           "https://jellyfin.example",
		LibraryRoot:   "/data/video/dramahub",
		LibraryPrefix: "短剧 - ",
		APIKeySet:     true,
		Running:       false,
		LastRunAt:     lastRun,
		LastOKAt:      lastRun,
		Created:       58,
		Uncertain:     2,
		Failed:        []string{"不存在的分类"},
		Orphans:       []string{"短剧 - 已下架"},
		Conflicts:     []string{},
	}}
	srv, _, _ := newTestServerWith(t, jf)

	got := decode[statusView](t, do(t, srv, http.MethodGet, "/api/status", ""))
	if !got.Jellyfin.Configured || !got.Jellyfin.APIKeySet {
		t.Errorf("jellyfin 块 = %+v, 期望 configured 与 api_key_set 都为 true", got.Jellyfin)
	}
	if got.Jellyfin.LibraryRoot != "/data/video/dramahub" {
		t.Errorf("library_root = %q", got.Jellyfin.LibraryRoot)
	}
	if want := lastRun.Format(time.RFC3339); got.Jellyfin.LastRunAt != want {
		t.Errorf("last_run_at = %q, 期望 %q（UTC RFC3339）", got.Jellyfin.LastRunAt, want)
	}
	if got.Jellyfin.Created != 58 || len(got.Jellyfin.Orphans) != 1 {
		t.Errorf("上次结果 = created %d / orphans %v, 期望 58 / 1 个", got.Jellyfin.Created, got.Jellyfin.Orphans)
	}
	if got.Jellyfin.Uncertain != 2 {
		t.Errorf("uncertain = %d, 期望 2（超时结果未知不与 created 混算）", got.Jellyfin.Uncertain)
	}
	if len(got.Jellyfin.Failed) != 1 || got.Jellyfin.Failed[0] != "不存在的分类" {
		t.Errorf("failed = %v, 期望 [不存在的分类]（被 Jellyfin 拒掉的分类要点名）", got.Jellyfin.Failed)
	}
	if body := do(t, srv, http.MethodGet, "/api/status", "").Body.String(); strings.Contains(body, "api_key\":") {
		t.Errorf("响应里出现了 api_key 字段：%s", body)
	}
}

// TestAPI_JellyfinLibrariesState 覆盖 GET /api/jellyfin/libraries（MKACG-664）：
// 页面轮询的轻量端点 —— 运行中给出阶段与进度，收尾后给出本轮计数与耗时；只读，
// 不触发新的一轮。
func TestAPI_JellyfinLibrariesState(t *testing.T) {
	started := time.Date(2026, 11, 3, 6, 30, 0, 0, time.UTC)
	jf := &fakeJellyfin{state: jellyfin.State{
		Configured: true,
		Missing:    []string{},
		Running:    true,
		StartedAt:  started,
		Phase:      jellyfin.PhaseCreate,
		Processed:  12,
		Total:      58,
		LastRunAt:  started.Add(-time.Minute),
		Unchanged:  55,
		DurationMS: 1234,
	}}
	srv, _, _ := newTestServerWith(t, jf)

	rec := do(t, srv, http.MethodGet, "/api/jellyfin/libraries", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	got := decode[jellyfinView](t, rec)
	if !got.Running {
		t.Error("running 应为 true（这一轮还在跑）")
	}
	if got.Phase != jellyfin.PhaseCreate || got.PhaseLabel != "创建媒体库" {
		t.Errorf("phase = %q / phase_label = %q, 期望 create / 创建媒体库（页面直接显示 label）",
			got.Phase, got.PhaseLabel)
	}
	if got.Processed != 12 || got.Total != 58 {
		t.Errorf("进度 = %d / %d, 期望 12 / 58", got.Processed, got.Total)
	}
	if want := started.Format(time.RFC3339); got.StartedAt != want {
		t.Errorf("started_at = %q, 期望 %q（页面靠它算已用时间）", got.StartedAt, want)
	}
	if got.Unchanged != 55 || got.DurationMS != 1234 {
		t.Errorf("unchanged = %d / duration_ms = %d, 期望 55 / 1234", got.Unchanged, got.DurationMS)
	}
	if jf.calls != 0 {
		t.Errorf("GET 触发了 %d 轮同步，期望 0（轮询端点只读）", jf.calls)
	}

	// 收尾后（没在跑）：阶段与 label 都空，页面只显示「上次同步：时间 + 结果」。
	jf.state = jellyfin.State{Configured: true, Missing: []string{}}
	idle := decode[jellyfinView](t, do(t, srv, http.MethodGet, "/api/jellyfin/libraries", ""))
	if idle.Running || idle.Phase != "" || idle.PhaseLabel != "" {
		t.Errorf("收尾后 running=%v / phase=%q / phase_label=%q, 期望 false / 空 / 空",
			idle.Running, idle.Phase, idle.PhaseLabel)
	}
}

// TestAPI_SettingsJellyfin 覆盖五项配置的写入面（AC-1 / AC-8）：
// api_key 只回 null、library_root 归一化去尾 /、越界值 400 且不落库。
func TestAPI_SettingsJellyfin(t *testing.T) {
	db, err := store.Open(context.Background(), pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	settings := store.NewSettings(db.DB)
	srv := NewServer(store.NewStore(db.DB), settings, &fakeSyncer{}, &fakeSchedule{}, &fakeJellyfin{},
		[]Source{{Name: "web", Label: "红果官网 SSR"}}, nil, testAdminToken, slog.New(slog.DiscardHandler))

	rec := do(t, srv, http.MethodPatch, "/api/settings",
		`{"settings":{"jellyfin_url":"https://jellyfin.example/","jellyfin_api_key":"abcd1234","jellyfin_library_root":"/data/video/dramahub/","jellyfin_library_prefix":"库 - ","jellyfin_timeout_sec":"15"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	got := decode[settingsView](t, rec)
	if value, ok := got.Settings["jellyfin_api_key"]; !ok || value != nil {
		t.Errorf("jellyfin_api_key 响应 = %v（存在 %v），期望 null（不回显）", value, ok)
	}
	if got.Settings["jellyfin_library_root"] != "/data/video/dramahub" {
		t.Errorf("jellyfin_library_root 归一化后 = %v, 期望去尾 /", got.Settings["jellyfin_library_root"])
	}
	if got.Settings["jellyfin_url"] != "https://jellyfin.example" {
		t.Errorf("jellyfin_url 归一化后 = %v", got.Settings["jellyfin_url"])
	}
	if kept, err := settings.String(t.Context(), store.KeyJellyfinAPIKey); err != nil || kept != "abcd1234" {
		t.Errorf("库里的 api key = %q, %v，期望 abcd1234", kept, err)
	}

	rec = do(t, srv, http.MethodPatch, "/api/settings", `{"settings":{"jellyfin_timeout_sec":"61"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("越界状态码 = %d, 期望 400（body=%s）", rec.Code, rec.Body.String())
	}
	if kept, err := settings.Int(t.Context(), store.KeyJellyfinTimeoutSec); err != nil || kept != 15 {
		t.Errorf("越界写入后 timeout = %d, %v，期望仍是 15", kept, err)
	}
}

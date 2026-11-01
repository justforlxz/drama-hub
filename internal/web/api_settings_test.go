package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/store"
)

// TestAPI_WriteEndpointsRequireToken 覆盖写操作的令牌闸门：`/api/` 原本完全无鉴权，
// POST /api/dramas 能触发同步、DELETE 能删库。没配令牌一律 401（fail closed），
// 带错也 401，读操作不受影响。
func TestAPI_WriteEndpointsRequireToken(t *testing.T) {
	writes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/dramas", `{"series_id":"1"}`},
		{http.MethodDelete, "/api/dramas/1", ""},
		{http.MethodPost, "/api/refresh", `{"source":"web"}`},
		{http.MethodPatch, "/api/settings", `{"settings":{"webdav_user":"alice"}}`},
		// PUT 现在没有注册端点，但闸门按「非读即写」判定：日后加了 PUT 也默认受保护，
		// 不需要回到这里登记（MKACG-634）。
		{http.MethodPut, "/api/settings", `{"settings":{"webdav_user":"alice"}}`},
	}

	t.Run("没带令牌", func(t *testing.T) {
		srv, _, syncer := newTestServer(t)
		for _, c := range writes {
			req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s 状态码 = %d, 期望 401", c.method, c.path, rec.Code)
			}
		}
		if syncer.calls != 0 {
			t.Errorf("未授权的 POST /api/dramas 触发了同步 %d 次", syncer.calls)
		}
		// 读操作照旧。
		for _, path := range []string{"/api/status", "/api/dramas"} {
			if rec := do(t, srv, http.MethodGet, path, ""); rec.Code != http.StatusOK {
				t.Errorf("GET %s 状态码 = %d, 期望 200（读操作不受令牌影响）", path, rec.Code)
			}
		}
	})

	t.Run("带错令牌", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		for _, c := range writes {
			req := newRequest(c.method, c.path, c.body)
			req.Header.Set(AdminTokenHeader, "wrong-token")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s 状态码 = %d, 期望 401", c.method, c.path, rec.Code)
			}
		}
	})

	t.Run("没配令牌时 fail closed", func(t *testing.T) {
		// 装配根没给令牌（env 没设）：所有写操作仍然 401，而不是「没配 = 不设防」。
		db, err := store.Open(context.Background(), pgtest.DSN(t))
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		srv := NewServer(store.NewStore(db.DB), store.NewSettings(db.DB), &fakeSyncer{}, &fakeSchedule{},
			nil, "", slog.New(slog.DiscardHandler))
		req := httptest.NewRequest(http.MethodPatch, "/api/settings",
			strings.NewReader(`{"settings":{"webdav_user":"alice"}}`))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("未配令牌的 PATCH 状态码 = %d, 期望 401（body=%s）", rec.Code, rec.Body.String())
		}
	})
}

// TestAPI_PatchSettings 覆盖写入契约：一次写多项、返回归一化后的值、密码不回显，
// 写进去的值立刻体现在 /api/status（页面与接口同一套读法）。
func TestAPI_PatchSettings(t *testing.T) {
	srv, _, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPatch, "/api/settings",
		`{"settings":{"webdav_user":"alice","webdav_pass":"s3cret","strm_prefix":"http://media.example:9000/","cache_max_mb":"4096","refresh_interval_sec":"900"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	var body settingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got := body.Settings["strm_prefix"]; got != "http://media.example:9000" {
		t.Errorf("归一化后的 strm_prefix = %v, 期望去尾 /", got)
	}
	if got := body.Settings["cache_max_mb"]; got != float64(4096) {
		t.Errorf("cache_max_mb 返回值 = %v, 期望 4096（数字）", got)
	}
	if got, ok := body.Settings["webdav_pass"]; !ok || got != nil {
		t.Errorf("webdav_pass 应回 null（不回显），实际 = %v（存在=%v）", got, ok)
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Error("响应体里出现 webdav_pass 明文")
	}

	// 写进去的值立刻是现读值：/api/status 与页面显示的就是它。
	got := decode[statusView](t, do(t, srv, http.MethodGet, "/api/status", ""))
	if got.Config.StrmPrefix != "http://media.example:9000" || got.Config.CacheMaxMB != 4096 ||
		got.Config.RefreshIntervalSec != 900 {
		t.Errorf("config = %+v, 期望刚写入的值", got.Config)
	}
	if !got.Config.WebDAVUserSet || !got.Config.WebDAVPassSet {
		t.Error("WebDAV 凭据已写入却报未设置")
	}
	if !got.AdminTokenSet {
		t.Error("admin_token_set = false, 期望 true")
	}
}

// 空 strm_prefix 仍是合法值（= 按请求 Host 推导，MKACG-616 B1），不能因为补校验把它
// 判成非法。它和上面两条负例一起把「先归一化、再要求 host」的顺序钉住（MKACG-634）。
func TestAPI_PatchSettingsEmptyStrmPrefix(t *testing.T) {
	srv, _, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPatch, "/api/settings", `{"settings":{"strm_prefix":""}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	got := decode[statusView](t, do(t, srv, http.MethodGet, "/api/status", ""))
	if got.Config.StrmPrefix != "" {
		t.Errorf("config.StrmPrefix = %q, 期望空串（Host 推导路径）", got.Config.StrmPrefix)
	}
}

// TestAPI_PatchSettingsRejectsBatch 覆盖拒绝语义：未知键 / 越界值 / 空请求都是 400，
// 而且整批不落库（半批生效最难排查）。边界值按下界合法性分别断言。
func TestAPI_PatchSettingsRejectsBatch(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	cases := []struct {
		name string
		body string
	}{
		{"未知键", `{"settings":{"webdav_user":"alice","nope":"1"}}`},
		{"越界值", `{"settings":{"webdav_user":"alice","cache_max_mb":"1048577"}}`},
		{"负数", `{"settings":{"cache_max_mb":"-1"}}`},
		{"非数字", `{"settings":{"cache_max_mb":"abc"}}`},
		{"空请求", `{"settings":{}}`},
		{"空密码", `{"settings":{"webdav_pass":""}}`},
		{"尾斜杠 URL 之外的非法值", `{"settings":{"strm_prefix":"not-a-url"}}`},
		// 归一化（去尾 `/`）在校验之前，会把这两条削成 `http:` / 空 host 的 URL，
		// 存进去之后 .strm 就是 `http:/video/…`，所以必须在校验里拦下（MKACG-634）。
		{"只有 scheme 的 URL", `{"settings":{"strm_prefix":"http://"}}`},
		{"空 host 的 URL", `{"settings":{"strm_prefix":"http:///path"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, srv, http.MethodPatch, "/api/settings", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400（body=%s）", rec.Code, rec.Body.String())
			}
			var errBody struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
				t.Fatalf("错误响应不是合法 JSON: %v", err)
			}
			if errBody.Code != "invalid" || errBody.Message == "" {
				t.Errorf("错误体 = %+v, 期望 code=invalid 且带原因", errBody)
			}
		})
	}

	// 批次被拒后库里没有任何改动：webdav_user 仍是「未设置」，cache_max_mb 仍是默认值。
	if _, err := srv.settings.String(ctx, store.KeyWebDAVUser); err == nil {
		t.Error("被拒的批次把 webdav_user 写进去了")
	}
	got := decode[statusView](t, do(t, srv, http.MethodGet, "/api/status", ""))
	if got.Config.CacheMaxMB != 8192 || got.Config.StrmPrefix != "" {
		t.Errorf("config = %+v, 期望默认值（8192 / 空前缀）", got.Config)
	}
}

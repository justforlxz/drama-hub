package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/jellyfin"
	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/store"
)

// TestAPI_JellyfinSyncFlow 是「点一次 → 看进度 → 看结果 → 运行中再点」的端到端证据
// （MKACG-664 AC-1 ~ AC-4）：真 Manager + 真 handler，上游是一个每次建库都慢一点的
// 假 Jellyfin。页面靠这三件事工作 —— 读到「运行中 + 进度」、读到收尾后的计数、第二个
// POST 拿到 409 —— 全部走真实 HTTP，而不是替身。
func TestAPI_JellyfinSyncFlow(t *testing.T) {
	var creates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/Library/VirtualFolders" {
			creates.Add(1)
			// 让「运行中」持续得够长，测试才能稳定读到中间态（真实 Jellyfin 更慢）。
			time.Sleep(120 * time.Millisecond)
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]")) // 列库：每次都是空库
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	cfg := jellyfin.Config{
		URL:         upstream.URL,
		APIKey:      "test-key",
		LibraryRoot: "/data/video/dramahub",
		Prefix:      "短剧 - ",
		Timeout:     2 * time.Second,
	}
	mgr := jellyfin.New(
		func(context.Context) (jellyfin.Config, error) { return cfg, nil },
		func(context.Context) ([]string, error) { return []string{"都市", "爱情", "科幻", "乡村"}, nil },
		func(context.Context) ([]string, error) { return nil, nil },
		slog.New(slog.DiscardHandler),
	)
	t.Cleanup(mgr.Wait)

	db, err := store.Open(context.Background(), pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := NewServer(store.NewStore(db.DB), store.NewSettings(db.DB), &fakeSyncer{}, &fakeSchedule{}, mgr,
		[]Source{{Name: "web", Label: "红果官网 SSR"}}, nil, testAdminToken, slog.New(slog.DiscardHandler))

	// 页面上的「正在跑」= 轮询 GET 读到的 running。AC-1 要求点下之后 2 秒内可见。
	start := time.Now()
	if rec := do(t, srv, http.MethodPost, "/api/jellyfin/libraries", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("POST 状态码 = %d, 期望 202（body=%s）", rec.Code, rec.Body.String())
	}
	running := pollJellyfin(t, srv, func(j jellyfinView) bool { return j.Running })
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("第一次读到「运行中」用了 %v, 期望 2 秒内", elapsed)
	}
	if running.StartedAt == "" || running.PhaseLabel == "" {
		t.Errorf("运行中 = 阶段 %q / started_at %q, 期望一开跑就有阶段与开始时间",
			running.PhaseLabel, running.StartedAt)
	}

	// 建库阶段的进度：总数是本轮要建的库数，processed 一格格前进（AC-1 的「计数在变化」）。
	creating := pollJellyfin(t, srv, func(j jellyfinView) bool {
		return j.Phase == jellyfin.PhaseCreate && j.Processed >= 1
	})
	if creating.Total != 4 {
		t.Errorf("建库阶段进度 = %d/%d, 期望总数 4", creating.Processed, creating.Total)
	}
	advanced := pollJellyfin(t, srv, func(j jellyfinView) bool {
		return j.Phase == jellyfin.PhaseCreate && j.Processed >= 2
	})
	if advanced.Processed > advanced.Total {
		t.Errorf("进度 %d/%d 越过总数", advanced.Processed, advanced.Total)
	}

	// AC-4：运行中再点一次 → 409，且不产生第二轮。
	if rec := do(t, srv, http.MethodPost, "/api/jellyfin/libraries", ""); rec.Code != http.StatusConflict {
		t.Errorf("运行中再点状态码 = %d, 期望 409（body=%s）", rec.Code, rec.Body.String())
	}

	// AC-2：收尾后不需要刷新页面就能读到本轮结果 —— 页面就是再读一次 GET。
	done := pollJellyfin(t, srv, func(j jellyfinView) bool { return !j.Running })
	if done.Created != 4 {
		t.Errorf("created = %d, 期望 4", done.Created)
	}
	if done.Unchanged != 0 || done.Removed != 0 || len(done.Failed) != 0 {
		t.Errorf("unchanged=%d removed=%d failed=%v, 期望 0 / 0 / 空", done.Unchanged, done.Removed, done.Failed)
	}
	if done.LastRunAt == "" || done.Phase != "" || done.PhaseLabel != "" {
		t.Errorf("收尾后 last_run_at=%q phase=%q phase_label=%q, 期望有时间且阶段为空",
			done.LastRunAt, done.Phase, done.PhaseLabel)
	}
	if got := creates.Load(); got != 4 {
		t.Errorf("上游建库请求 = %d, 期望 4（第二次点击被单飞锁挡住，没有第二轮）", got)
	}
}

// pollJellyfin 反复读 GET /api/jellyfin/libraries 直到 cond 成立（最多 5 秒）。
func pollJellyfin(t *testing.T, srv *Server, cond func(jellyfinView) bool) jellyfinView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := do(t, srv, http.MethodGet, "/api/jellyfin/libraries", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
		}
		if got := decode[jellyfinView](t, rec); cond(got) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等不到期望的状态（5 秒超时）")
	return jellyfinView{}
}

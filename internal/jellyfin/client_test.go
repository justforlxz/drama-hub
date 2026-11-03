package jellyfin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeJellyfin 是一个最小的 Jellyfin 管理 API：只实现本服务用到的四个端点，
// 记下每个请求（方法、路径、查询串、认证头），供断言「发了什么」。
//
// 它冒充的是**上游**（真实 Jellyfin），不是我们自己的 handler —— 与被测代码之间
// 隔着一层真的 HTTP。
type fakeJellyfin struct {
	t          *testing.T
	mu         sync.Mutex
	libraries  []Library
	status     int           // 非 0 时所有写请求返回它（含响应体，用来验证错误文本不外泄）
	body       string        // status 非 0 时的响应体
	failList   bool          // GET 也失败
	block      chan struct{} // 非 nil 时 GET 阻塞到它关闭（验证单次超时）
	writeDelay time.Duration // 非 0 时写请求先睡这么久（模拟 Jellyfin 扫库期间建库很慢）
	// badPaths 里的路径在「建库」时回 400：复刻 12.1 的
	// `ArgumentException: The specified path does not exist: <path>`。
	badPaths map[string]bool
	// deleteFail 里的库名在「删库」时回 500（且库仍然存在）。
	deleteFail map[string]bool
	// deleteVanish 里的库名在「删库」时回 500，但库里已经没有它了：
	// 复刻「服务端删成了、客户端没收到 204」（Jellyfin 也可能对不存在的名字回 500）。
	deleteVanish map[string]bool
	requests     []recorded
}

type recorded struct {
	method string
	path   string
	query  url.Values
	auth   string
}

func (f *fakeJellyfin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, recorded{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.Query(),
		auth:   r.Header.Get("Authorization"),
	})
	f.mu.Unlock()
	if f.block != nil && r.Method == http.MethodGet {
		select {
		case <-f.block:
		case <-r.Context().Done():
			return
		}
	}
	if f.status != 0 && (r.Method != http.MethodGet || f.failList) {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.libraries)
		return
	}
	// 建库请求先睡 writeDelay：模拟「Jellyfin 正在扫库，单次建库 >10s」，用来验证
	// 单次超时与整轮预算这两层是分开的。只拖建库（实测慢的就是它），扫描仍按正常
	// 速度返回，否则测不出「建库超时、扫描照常」这个组合。
	// 睡眠跟着请求的 ctx 走：客户端超时后服务端不该赖着不放手。
	f.mu.Lock()
	delay := f.writeDelay
	f.mu.Unlock()
	if delay > 0 && r.Method == http.MethodPost && r.URL.Path == pathLibraries {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	// 路径不存在时 Jellyfin 回 400，**不是**东西已经建好：这里不记进 libraries。
	if r.Method == http.MethodPost && r.URL.Path == pathLibraries {
		f.mu.Lock()
		bad := f.badPaths[r.URL.Query().Get("paths")]
		f.mu.Unlock()
		if bad {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	// 删库要真的从库里拿掉：重试与幂等的断言看的就是「下一次列库还在不在」。
	if r.Method == http.MethodDelete && r.URL.Path == pathLibraries {
		name := r.URL.Query().Get("name")
		f.mu.Lock()
		fail := f.deleteFail[name]
		vanish := f.deleteVanish[name]
		if vanish {
			f.removeLocked(name)
		} else if !fail {
			f.removeLocked(name)
		}
		f.mu.Unlock()
		if fail || vanish {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeLocked 从假 Jellyfin 的库列表里去掉一个名字（调用方持锁）。
func (f *fakeJellyfin) removeLocked(name string) {
	kept := f.libraries[:0]
	for _, lib := range f.libraries {
		if lib.Name != name {
			kept = append(kept, lib)
		}
	}
	f.libraries = kept
}

// seen 返回已收到的请求快照（handler 在自己的 goroutine 里跑，读也要加锁）。
func (f *fakeJellyfin) seen() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

// writes 返回已收到的写请求（建库 / 删库 / 扫描）—— 幂等的断言看它就是看「发了什么」。
func (f *fakeJellyfin) writes() []recorded {
	all := f.seen()
	out := make([]recorded, 0, len(all))
	for _, req := range all {
		if req.method != http.MethodGet {
			out = append(out, req)
		}
	}
	return out
}

// newFakeJellyfin 起一个假 Jellyfin，返回它和它的 client 配置。
func newFakeJellyfin(t *testing.T, ff *fakeJellyfin) (*fakeJellyfin, Config) {
	t.Helper()
	ff.t = t
	srv := httptest.NewServer(ff)
	t.Cleanup(srv.Close)
	return ff, Config{
		URL:         srv.URL,
		APIKey:      "secret-api-key",
		LibraryRoot: "/data/video/dramahub",
		Prefix:      "短剧 - ",
		Timeout:     time.Second,
	}
}

// TestClientRequests 锁定四个端点的请求形态（12.1 实测结论，见 docs/adr/0012）：
// 认证只走 Authorization: MediaBrowser Token="…"（X-Emby-Token 已失效），
// key 不进 query；建库/删库都按 name，建库必须 refreshLibrary=false。
func TestClientRequests(t *testing.T) {
	ff, cfg := newFakeJellyfin(t, &fakeJellyfin{
		libraries: []Library{{Name: "短剧 - 都市", Locations: []string{"/data/video/dramahub/都市"}}},
	})
	client := NewClient(cfg, nil)
	ctx := context.Background()

	libs, err := client.ListLibraries(ctx)
	if err != nil {
		t.Fatalf("ListLibraries: %v", err)
	}
	if len(libs) != 1 || libs[0].Name != "短剧 - 都市" {
		t.Fatalf("ListLibraries = %+v", libs)
	}
	if err := client.CreateLibrary(ctx, "短剧 - 都市", "/data/video/dramahub/都市"); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	if err := client.DeleteLibrary(ctx, "短剧 - 都市"); err != nil {
		t.Fatalf("DeleteLibrary: %v", err)
	}
	if err := client.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	want := []struct {
		method string
		path   string
		query  url.Values
	}{
		{http.MethodGet, "/Library/VirtualFolders", url.Values{}},
		{http.MethodPost, "/Library/VirtualFolders", url.Values{
			"name":           {"短剧 - 都市"},
			"collectionType": {"tvshows"},
			"paths":          {"/data/video/dramahub/都市"},
			"refreshLibrary": {"false"},
		}},
		{http.MethodDelete, "/Library/VirtualFolders", url.Values{
			"name":           {"短剧 - 都市"},
			"refreshLibrary": {"false"},
		}},
		{http.MethodPost, "/Library/Refresh", url.Values{}},
	}
	got := ff.seen()
	if len(got) != len(want) {
		t.Fatalf("请求数 = %d, 期望 %d（%+v）", len(got), len(want), got)
	}
	for i, w := range want {
		req := got[i]
		if req.method != w.method || req.path != w.path {
			t.Errorf("第 %d 个请求 = %s %s, 期望 %s %s", i, req.method, req.path, w.method, w.path)
		}
		if req.query.Encode() != w.query.Encode() {
			t.Errorf("%s %s 的查询串 = %q, 期望 %q", req.method, req.path, req.query.Encode(), w.query.Encode())
		}
		if want := `MediaBrowser Token="secret-api-key"`; req.auth != want {
			t.Errorf("Authorization = %q, 期望 %q", req.auth, want)
		}
		if strings.Contains(req.query.Encode(), "secret-api-key") {
			t.Errorf("api key 进了查询串：%q（会写进 Jellyfin 访问日志）", req.query.Encode())
		}
	}
}

// TestClientErrorText 锁定错误文本的形状：只有方法、端点路径与状态码。
// 响应体可能回显请求参数，这里刻意让它包含 api key —— 错误里出现 key 就是缺陷（AC-8）。
func TestClientErrorText(t *testing.T) {
	ff, cfg := newFakeJellyfin(t, &fakeJellyfin{
		status:   http.StatusInternalServerError,
		failList: true,
		body:     `{"error":"bad token secret-api-key"}`,
	})
	client := NewClient(cfg, nil)

	_, err := client.ListLibraries(context.Background())
	if err == nil {
		t.Fatal("GET 500 应返回错误")
	}
	if strings.Contains(err.Error(), "secret-api-key") {
		t.Errorf("错误文本带出了 api key：%v", err)
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), pathLibraries) {
		t.Errorf("错误文本 = %q, 期望含状态码 500 与端点路径 %s", err, pathLibraries)
	}
	if len(ff.seen()) != 1 {
		t.Errorf("请求数 = %d, 期望 1", len(ff.seen()))
	}
}

// TestClientTimeout 锁定「每次请求一个 ctx 超时」：Jellyfin 卡住时 client 立刻返回，
// 不会把整轮 reconcile 挂到进程退出（不带 http.Client.Timeout 的理由就是这个）。
func TestClientTimeout(t *testing.T) {
	block := make(chan struct{})
	_, cfg := newFakeJellyfin(t, &fakeJellyfin{block: block})
	t.Cleanup(func() { close(block) })
	cfg.Timeout = 20 * time.Millisecond

	_, err := NewClient(cfg, nil).ListLibraries(context.Background())
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if !strings.Contains(err.Error(), pathLibraries) {
		t.Errorf("错误文本 = %q, 期望点名端点 %s", err, pathLibraries)
	}
}

// TestClientURLJoin 锁定基址末尾斜杠的归一：`http://host:8096/` 与不带斜杠拼出同一路径
// （否则会变成 `//Library/VirtualFolders`，部分反代直接 404）。
func TestClientURLJoin(t *testing.T) {
	ff, cfg := newFakeJellyfin(t, &fakeJellyfin{})
	cfg.URL += "/"
	if _, err := NewClient(cfg, nil).ListLibraries(context.Background()); err != nil {
		t.Fatalf("ListLibraries: %v", err)
	}
	if seen := ff.seen(); len(seen) != 1 || seen[0].path != pathLibraries {
		t.Fatalf("请求 = %+v, 期望路径 %s", seen, pathLibraries)
	}
}

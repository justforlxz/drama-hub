package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

// coverBody 是假 CDN 返回的「海报」字节。内容只要非空且可断言即可。
var coverBody = []byte("\xff\xd8\xff\xe0 fake jpeg bytes")

const (
	testProvider   = "web"
	testProviderID = "7503745441910017049"
)

// fakeCDN 是可切换响应的假封面源，并记录被请求次数。
type fakeCDN struct {
	mu       sync.Mutex
	status   int
	body     []byte
	requests int
}

func (f *fakeCDN) set(status int, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *fakeCDN) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func newFakeCDN(t *testing.T) (*fakeCDN, *httptest.Server) {
	t.Helper()
	f := &fakeCDN{status: http.StatusOK, body: coverBody}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.requests++
		status, body := f.status, f.body
		f.mu.Unlock()
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "image/jpeg")
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// newTestCoverCache 建临时库 + 临时 cache_dir，时钟固定在固定值上。
func newTestCoverCache(t *testing.T, srv *httptest.Server, cacheDir string) (*CoverCache, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "cover.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	if err := settings.Set(ctx, store.KeyCacheDir, cacheDir); err != nil {
		t.Fatalf("设置 cache_dir: %v", err)
	}
	c := NewCoverCache(st, settings, srv.Client())
	c.now = func() time.Time { return time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC) }
	return c, st
}

// AC：同步后 artifact 有 kind='cover' 行，文件落在 <cache_dir>/cover/…，字段与实际
// 文件一致；再次本地化不回源（源图 404 也仍能取到本地海报）。
func TestLocalizeCoverWritesArtifactAndFile(t *testing.T) {
	cdn, srv := newFakeCDN(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	c, st := newTestCoverCache(t, srv, cacheDir)
	ctx := context.Background()

	if err := c.LocalizeCover(ctx, testProvider, testProviderID, srv.URL+"/cover.jpg"); err != nil {
		t.Fatalf("LocalizeCover: %v", err)
	}

	key := testProvider + "/" + testProviderID
	row, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if row.Kind != store.ArtifactKindCover || row.Key != key {
		t.Fatalf("artifact = %+v, 期望 kind=cover key=%s", row, key)
	}
	if row.Bytes != int64(len(coverBody)) {
		t.Fatalf("artifact.Bytes = %d, 期望 %d", row.Bytes, len(coverBody))
	}
	wantSum := sha256.Sum256(coverBody)
	if row.SHA256 != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("artifact.SHA256 = %q, 期望内容哈希", row.SHA256)
	}
	if row.LastUsedAt != "2026-10-30T09:00:00Z" {
		t.Fatalf("artifact.LastUsedAt = %q, 期望注入时钟的 UTC RFC3339", row.LastUsedAt)
	}

	// 落盘布局：<cache_dir>/cover/<hash 前 2 位>/<hash>.jpg（docs/CACHE-MODEL.md）。
	sum := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(sum[:])
	wantPath := filepath.Join(cacheDir, "cover", hash[:2], hash+".jpg")
	if row.Path != wantPath {
		t.Fatalf("artifact.Path = %q, 期望 %q", row.Path, wantPath)
	}
	onDisk, err := os.ReadFile(row.Path)
	if err != nil {
		t.Fatalf("读本地海报: %v", err)
	}
	if string(onDisk) != string(coverBody) {
		t.Fatalf("本地海报字节与上游不一致: %q", onDisk)
	}

	// 源图改成 404 后重跑：命中的是本地文件，不发请求、字段不变。
	cdn.set(http.StatusNotFound, nil)
	before := cdn.count()
	if err := c.LocalizeCover(ctx, testProvider, testProviderID, srv.URL+"/cover.jpg"); err != nil {
		t.Fatalf("第二次 LocalizeCover（源图 404）: %v", err)
	}
	if got := cdn.count(); got != before {
		t.Fatalf("命中本地后仍回源：请求数 %d → %d", before, got)
	}
	after, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, key)
	if err != nil {
		t.Fatalf("第二次 ArtifactByKey: %v", err)
	}
	if after != row {
		t.Fatalf("命中后索引行变了：%+v → %+v", row, after)
	}
	if _, err := os.Stat(row.Path); err != nil {
		t.Fatalf("命中后本地文件不可用: %v", err)
	}
}

// 本地文件被删（索引行还在）时重新下载；源图 404 则报 ErrCoverFetch 且不写索引行。
func TestLocalizeCoverMissingFileRefetches(t *testing.T) {
	cdn, srv := newFakeCDN(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	c, st := newTestCoverCache(t, srv, cacheDir)
	ctx := context.Background()
	url := srv.URL + "/cover.jpg"

	if err := c.LocalizeCover(ctx, testProvider, testProviderID, url); err != nil {
		t.Fatalf("LocalizeCover: %v", err)
	}
	row, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, testProvider+"/"+testProviderID)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if err := os.Remove(row.Path); err != nil {
		t.Fatalf("删本地海报: %v", err)
	}

	cdn.set(http.StatusNotFound, []byte("nope"))
	if err := c.LocalizeCover(ctx, testProvider, testProviderID, url); !errors.Is(err, ErrCoverFetch) {
		t.Fatalf("文件缺失且源图 404 的错误 = %v, 期望 ErrCoverFetch", err)
	}
	if got := cdn.count(); got != 2 {
		t.Fatalf("请求数 = %d, 期望 2（首次 + 文件缺失后重取）", got)
	}
	if _, err := os.Stat(row.Path); !os.IsNotExist(err) {
		t.Fatalf("下载失败却留下了文件: %v", err)
	}

	// 源图恢复后重取成功，索引行可读。
	cdn.set(http.StatusOK, coverBody)
	if err := c.LocalizeCover(ctx, testProvider, testProviderID, url); err != nil {
		t.Fatalf("恢复后 LocalizeCover: %v", err)
	}
	if _, err := os.Stat(row.Path); err != nil {
		t.Fatalf("重取后文件不在: %v", err)
	}
}

// 首次下载失败（500 / 空响应体）不写索引行，也不留半截文件。
func TestLocalizeCoverFailureLeavesNothing(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   []byte
	}{
		{"上游 500", http.StatusInternalServerError, []byte("boom")},
		{"空响应体", http.StatusOK, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cdn, srv := newFakeCDN(t)
			cdn.set(tc.status, tc.body)
			cacheDir := filepath.Join(t.TempDir(), "cache")
			c, st := newTestCoverCache(t, srv, cacheDir)
			ctx := context.Background()

			err := c.LocalizeCover(ctx, testProvider, testProviderID, srv.URL+"/cover.jpg")
			if !errors.Is(err, ErrCoverFetch) {
				t.Fatalf("err = %v, 期望 ErrCoverFetch", err)
			}
			if _, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, testProvider+"/"+testProviderID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("失败后 artifact 行 = %v, 期望 ErrNotFound", err)
			}
			if entries := listFiles(t, cacheDir); len(entries) != 0 {
				t.Fatalf("失败后在缓存目录留下文件: %v", entries)
			}
		})
	}
}

// 上游未声明封面（url 为空）时无事可做：不发请求、不写行。
func TestLocalizeCoverEmptyURL(t *testing.T) {
	cdn, srv := newFakeCDN(t)
	c, st := newTestCoverCache(t, srv, filepath.Join(t.TempDir(), "cache"))
	ctx := context.Background()

	if err := c.LocalizeCover(ctx, testProvider, testProviderID, ""); err != nil {
		t.Fatalf("空 url 的 LocalizeCover = %v, 期望 nil", err)
	}
	if got := cdn.count(); got != 0 {
		t.Fatalf("空 url 仍发了 %d 次请求", got)
	}
	if _, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, testProvider+"/"+testProviderID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("空 url 后 artifact 行 = %v, 期望 ErrNotFound", err)
	}
}

// AC（MKACG-620）：单次下载等多久由 settings 的 cover_timeout_sec 现读决定，
// 不再走代码常量——配 1 s 时，封面域名不回包最多等 1 s 就放弃。
func TestLocalizeCoverUsesConfiguredTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// 只回包不给响应头：客户端等到 ctx 超时。release 让 handler 在客户端放弃后
		// 也能立刻退出（srv.Close 会等所有 handler 结束）。
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	c, st := newTestCoverCache(t, srv, filepath.Join(t.TempDir(), "cache"))
	ctx := context.Background()
	if err := c.settings.Set(ctx, store.KeyCoverTimeoutSec, "1"); err != nil {
		t.Fatalf("设置 cover_timeout_sec: %v", err)
	}

	start := time.Now()
	err := c.LocalizeCover(ctx, testProvider, testProviderID, srv.URL+"/cover.jpg")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrCoverFetch) {
		t.Fatalf("超时的错误 = %v, 期望 ErrCoverFetch", err)
	}
	// 上限：配置值 1 s。用默认值 5 s 或旧常量 15 s 都会超这条。
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("等待 %v, 超过 cover_timeout_sec = 1 s", elapsed)
	}
	// 下限：确实按配置值等，而不是立刻失败。
	if elapsed < 900*time.Millisecond {
		t.Fatalf("等待 %v, 期望接近配置的 1 s", elapsed)
	}
	t.Logf("封面下载等待 %v（cover_timeout_sec = 1 s）", elapsed.Round(time.Millisecond))
	if _, err := st.ArtifactByKey(ctx, store.ArtifactKindCover, testProvider+"/"+testProviderID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("超时后 artifact 行 = %v, 期望 ErrNotFound", err)
	}
}

// coverExt 只影响本地文件名：Content-Type 优先，其次 URL 后缀，最后 .img。
func TestCoverExt(t *testing.T) {
	cases := []struct{ contentType, url, want string }{
		{"image/jpeg", "https://cdn/x", ".jpg"},
		{"image/png; charset=binary", "https://cdn/x", ".png"},
		{"image/webp", "https://cdn/x", ".webp"},
		{"image/gif", "https://cdn/x", ".gif"},
		{"application/octet-stream", "https://cdn/x.webp", ".webp"},
		{"application/octet-stream", "https://cdn/x?sign=1", ".img"},
		{"", "https://cdn/very.long.suffix", ".img"},
	}
	for _, tc := range cases {
		if got := coverExt(tc.contentType, tc.url); got != tc.want {
			t.Errorf("coverExt(%q, %q) = %q, 期望 %q", tc.contentType, tc.url, got, tc.want)
		}
	}
}

// listFiles 返回目录树下的普通文件相对路径（不存在目录视为空）。
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历缓存目录: %v", err)
	}
	return out
}

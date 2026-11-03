package webdav

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/vfs"
)

// newCacheFixture 起一个「真数据库 + 计数树 + 真 handler」的服务端：计数树用来证明
// 第二次 PROPFIND 没有回到数据库/树，而是直接回放了缓存字节。
func newCacheFixture(t *testing.T, dramas int) (fixture, *countingTree) {
	t.Helper()
	ctx := context.Background()
	const (
		user   = "davuser"
		pass   = "davpass"
		genre  = "剧情"
		secret = "0123456789abcdef0123456789abcdef"
	)

	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	for k, v := range map[store.SettingKey]string{
		store.KeyWebDAVUser:     user,
		store.KeyWebDAVPass:     pass,
		store.KeyStreamSecret:   secret,
		store.KeyMergeChunkSize: "1",
	} {
		if err := settings.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	for i := 0; i < dramas; i++ {
		if _, err := st.UpsertDrama(ctx, store.Drama{
			Provider: "web", ProviderID: "c" + strconv.Itoa(i),
			Title: "剧" + strconv.Itoa(i), Year: 2024, Genre: genre,
		}); err != nil {
			t.Fatalf("UpsertDrama %d: %v", i, err)
		}
	}

	tree := newCountingTree(vfs.NewTree(st, settings, stubID, nil, newDiscardLogger()))
	mux := http.NewServeMux()
	mux.Handle(MountPath, NewHandler(tree, settings, newDiscardLogger()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return fixture{
		srv: srv, client: &http.Client{Timeout: requestTimeout},
		user: user, pass: pass, genre: genre, st: st, settings: settings,
	}, tree
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体: %v", err)
	}
	return body
}

// 命中缓存：第二次同样的 PROPFIND 返回逐字节相同的响应，且完全不再查树。
func TestPropfindCacheHitReplaysIdenticalBody(t *testing.T) {
	f, tree := newCacheFixture(t, 3)
	dir := "/" + f.genre

	first := f.request(t, methodPropfind, dir, nil, map[string]string{"Depth": "1"})
	if first.StatusCode != http.StatusMultiStatus {
		t.Fatalf("首次状态码 = %d, 期望 207", first.StatusCode)
	}
	body1 := readAll(t, first)
	if tree.lists == 0 {
		t.Fatal("首次请求没有查树（预热失败）")
	}

	tree.reset()
	second := f.request(t, methodPropfind, dir, nil, map[string]string{"Depth": "1"})
	if second.StatusCode != http.StatusMultiStatus {
		t.Fatalf("第二次状态码 = %d, 期望 207", second.StatusCode)
	}
	body2 := readAll(t, second)

	if !bytes.Equal(body1, body2) {
		t.Fatalf("命中缓存的响应与首次不一致\nfirst: %s\nsecond: %s", body1, body2)
	}
	if tree.lists != 0 || tree.renders != 0 {
		t.Fatalf("命中缓存仍然动了树: lists=%d renders=%d", tree.lists, tree.renders)
	}
	if got, want := second.Header.Get("Content-Length"), strconv.Itoa(len(body1)); got != want {
		t.Fatalf("Content-Length = %q, 期望 %q", got, want)
	}
}

// 失效：写入提交后立刻能看到新剧，不读旧缓存。
func TestPropfindCacheInvalidatedByWrite(t *testing.T) {
	f, tree := newCacheFixture(t, 2)
	dir := "/" + f.genre

	code, ms := f.propfind(t, dir)
	if code != http.StatusMultiStatus {
		t.Fatalf("预热状态码 = %d", code)
	}
	before := len(ms.Responses)

	if _, err := f.st.UpsertDrama(context.Background(), store.Drama{
		Provider: "web", ProviderID: "new1", Title: "新剧", Year: 2026, Genre: f.genre,
	}); err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}

	tree.reset()
	code, ms = f.propfind(t, dir)
	if code != http.StatusMultiStatus {
		t.Fatalf("写入后状态码 = %d", code)
	}
	if got := len(ms.Responses); got != before+1 {
		t.Fatalf("写入后子项数 = %d, 期望 %d（读到旧缓存？）", got, before+1)
	}
	if tree.lists == 0 {
		t.Fatal("写入后没有重新查树")
	}
}

// 不同 Depth 是不同响应，不能互相命中。
func TestPropfindCacheSeparatesDepth(t *testing.T) {
	f, _ := newCacheFixture(t, 3)
	dir := "/" + f.genre

	code, shallow := f.propfind(t, dir) // Depth: 1
	if code != http.StatusMultiStatus {
		t.Fatalf("Depth:1 状态码 = %d", code)
	}

	resp := f.request(t, methodPropfind, dir, nil, map[string]string{"Depth": "0"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("Depth:0 状态码 = %d", resp.StatusCode)
	}
	var deep multistatus
	if err := xml.Unmarshal(readAll(t, resp), &deep); err != nil {
		t.Fatalf("Depth:0 响应解析: %v", err)
	}
	if len(deep.Responses) != 1 {
		t.Fatalf("Depth:0 返回 %d 条, 期望 1（命中了 Depth:1 的缓存？）", len(deep.Responses))
	}
	if len(shallow.Responses) < 2 {
		t.Fatalf("Depth:1 返回 %d 条, 期望包含子项", len(shallow.Responses))
	}
}

// 不同属性清单（请求体）是不同响应，缓存键必须体现它。
func TestPropfindCacheKeyIncludesRequestBody(t *testing.T) {
	f, _ := newCacheFixture(t, 2)
	dir := "/" + f.genre

	lenBody := `<?xml version="1.0"?><propfind xmlns="DAV:"><prop><getcontentlength/></prop></propfind>`
	nameBody := `<?xml version="1.0"?><propfind xmlns="DAV:"><prop><displayname/></prop></propfind>`

	first := f.request(t, methodPropfind, dir, strings.NewReader(lenBody), map[string]string{"Depth": "1"})
	body1 := readAll(t, first)
	second := f.request(t, methodPropfind, dir, strings.NewReader(nameBody), map[string]string{"Depth": "1"})
	body2 := readAll(t, second)

	if bytes.Equal(body1, body2) {
		t.Fatal("不同属性清单返回了同一份缓存响应")
	}
	if bytes.Contains(body2, []byte("getcontentlength")) {
		t.Fatalf("displayname 请求的响应里仍有 getcontentlength: %s", body2)
	}
}

// 认证先于缓存：改密码后旧凭据必须 401，不能靠缓存绕过认证。
func TestPropfindCacheDoesNotBypassAuth(t *testing.T) {
	f, _ := newCacheFixture(t, 2)
	dir := "/" + f.genre

	if code, _ := f.propfind(t, dir); code != http.StatusMultiStatus {
		t.Fatalf("预热状态码 = %d", code)
	}
	if err := f.settings.Set(context.Background(), store.KeyWebDAVPass, "新密码"); err != nil {
		t.Fatalf("Set webdav_pass: %v", err)
	}
	resp := f.request(t, methodPropfind, dir, nil, map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("旧密码状态码 = %d, 期望 401", resp.StatusCode)
	}
}

// 响应体超过单条上限时放弃缓存（10 万 / 21.8 万项那一档）。
func TestPropfindRecorderStopsBufferingOversize(t *testing.T) {
	rec := &propfindRecorder{ResponseWriter: httptest.NewRecorder(), cacheable: true}
	rec.WriteHeader(http.StatusMultiStatus)
	chunk := bytes.Repeat([]byte("x"), propfindCacheMaxEntry+1)
	if _, err := rec.Write(chunk); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if rec.cacheable {
		t.Fatal("超过单条上限仍标记为可缓存")
	}
}

// 正文写出后再报错（协议层 500）的响应不完整，不能缓存。
func TestPropfindRecorderRejectsSecondWriteHeader(t *testing.T) {
	rec := &propfindRecorder{ResponseWriter: httptest.NewRecorder(), cacheable: true}
	rec.WriteHeader(http.StatusMultiStatus)
	if _, err := rec.Write([]byte("partial")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	rec.WriteHeader(http.StatusInternalServerError)
	if rec.cacheable {
		t.Fatal("正文后出现第二个状态码仍标记为可缓存")
	}
}

// TestPropfindCacheDoesNotServeWhenDBDown 钉住数据库不可用时的现状语义：认证每请求
// 现读 settings，读不出来就没有资格命中任何缓存，必须仍是 401 —— 既不能把缓存里的
// 207 直接发出去，也不能退化成 5xx。
func TestPropfindCacheDoesNotServeWhenDBDown(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	for k, v := range map[store.SettingKey]string{
		store.KeyWebDAVUser:     "u",
		store.KeyWebDAVPass:     "p",
		store.KeyStreamSecret:   "0123456789abcdef0123456789abcdef",
		store.KeyMergeChunkSize: "1",
	} {
		if err := settings.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	if _, err := st.UpsertDrama(ctx, store.Drama{
		Provider: "web", ProviderID: "z1", Title: "剧", Year: 2024, Genre: "剧情",
	}); err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}

	tree := vfs.NewTree(st, settings, stubID, nil, newDiscardLogger())
	mux := http.NewServeMux()
	mux.Handle(MountPath, NewHandler(tree, settings, newDiscardLogger()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	do := func() int {
		req, err := http.NewRequestWithContext(ctx, methodPropfind, srv.URL+strings.TrimSuffix(MountPath, "/")+"/", nil)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		req.SetBasicAuth("u", "p")
		req.Header.Set("Depth", "0")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("PROPFIND: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	if code := do(); code != http.StatusMultiStatus {
		t.Fatalf("数据库正常时 PROPFIND = %d, 期望 207", code)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库: %v", err)
	}
	if code := do(); code != http.StatusUnauthorized {
		t.Fatalf("数据库停机时 PROPFIND = %d, 期望 401（缓存不得绕过认证）", code)
	}
}

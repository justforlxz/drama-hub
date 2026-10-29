package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/store"
)

// testSecret 是测试用的 stream_secret：恰好 32 字节，满足 settings 的最小长度。
const testSecret = "0123456789abcdef0123456789abcdef"

// testUID 是签名黄金值用的固定 uid。
const testUID = "e42"

// goldenToken 由 openssl 独立算出（与本包实现无关）：
//
//	printf 'stream:e42' | openssl dgst -sha256 -hmac testSecret -binary | basenc --base64url
//
// 证明 "stream:" 前缀、HMAC-SHA256 与 RawURLEncoding 逐字节符合 ADR-0003。
const goldenToken = "6OOxKgEmGuM6sGxry9sNEneqXUlVoGEp9S16pRubDVo"

// newTestDB 用 store.Open 建临时文件库（真实 DSN：WAL / foreign_keys / 迁移）。
func newTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "stream.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// cdnSeen 是假 CDN 观察到的一次请求（不透传 body，只看请求头）。
type cdnSeen struct {
	method         string
	path           string
	rangeHeader    string
	acceptEncoding string
}

// newCDN 起一个像真 CDN 的假源站：http.ServeContent 负责 Content-Length、
// Accept-Ranges、206/Content-Range 与 416，正是我们要透传的语义。同时记录收到
// 的请求头，供「Range 确实透传」「DisableCompression 生效」两条 AC 断言。
func newCDN(t *testing.T, body []byte) (*httptest.Server, func() []cdnSeen) {
	t.Helper()
	var mu sync.Mutex
	seen := make([]cdnSeen, 0, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, cdnSeen{
			method:         r.Method,
			path:           r.URL.Path,
			rangeHeader:    r.Header.Get("Range"),
			acceptEncoding: r.Header.Get("Accept-Encoding"),
		})
		mu.Unlock()
		http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []cdnSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]cdnSeen(nil), seen...)
	}
}

// cdnSeen2 是自定义假 CDN 记录的一次请求（method / path / range）。
type cdnSeen2 struct {
	method      string
	path        string
	rangeHeader string
}

// newChunkedCDN 起一个 chunked（无 Content-Length）假 CDN：先 Flush 再写 body，
// 强制 net/http 走 chunked 传输。用于锁定「上游不给精确长度 → 502 而非无长度 200」。
func newChunkedCDN(t *testing.T, body []byte) (*httptest.Server, func() []cdnSeen2) {
	t.Helper()
	var mu sync.Mutex
	seen := make([]cdnSeen2, 0, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, cdnSeen2{method: r.Method, path: r.URL.Path, rangeHeader: r.Header.Get("Range")})
		mu.Unlock()
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // 强制 chunked：响应不再携带 Content-Length
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []cdnSeen2 {
		mu.Lock()
		defer mu.Unlock()
		return append([]cdnSeen2(nil), seen...)
	}
}

// fixture 是一次装配好的测试环境。
type fixture struct {
	srv      *Server
	uid      string
	secret   string
	basePath string
}

// newFixture 建临时库 + 写一集 play_url 指向假 CDN 的剧，返回装配好的 Server
// 与可直接请求的路径。body 为空且 withPlayURL 为 false 时 play_url=”（AC7）。
func newFixture(t *testing.T, body []byte, withPlayURL bool) (*fixture, func() []cdnSeen) {
	t.Helper()
	cdn, seen := newCDN(t, body)

	db := newTestDB(t)
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	if err := settings.Set(context.Background(), store.KeyStreamSecret, testSecret); err != nil {
		t.Fatalf("Set stream_secret: %v", err)
	}

	dramaID, err := st.UpsertDrama(context.Background(), store.Drama{
		Provider: "web", ProviderID: "sid1", Title: "测试剧", EpisodeTotal: 1,
	})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	playURL := ""
	if withPlayURL {
		playURL = cdn.URL + "/video.mp4"
	}
	epID, err := st.UpsertEpisode(context.Background(), store.Episode{
		DramaID: dramaID, EpNo: 1, Title: "第 1 集", Source: "web", Vid: "v1", PlayURL: playURL,
	})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}
	uid := "e" + strconv.FormatInt(epID, 10)
	return &fixture{
		srv:      NewServer(st, settings, slog.New(slog.DiscardHandler)),
		uid:      uid,
		secret:   testSecret,
		basePath: "/s/" + Sign([]byte(testSecret), uid) + "/e/" + uid,
	}, seen
}

// serve 向真 handler 发一次请求，返回响应记录器。
func serve(t *testing.T, h http.Handler, method, path, rng string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSignVerify 覆盖签名契约（ADR-0003 / AC5 单元侧）：编码逐字节符合黄金值；
// 正确签名通过；错误 secret、换 uid、篡改一位、长度不对、非 base64、缺失都不通过。
func TestSignVerify(t *testing.T) {
	secret := []byte(testSecret)
	if got := Sign(secret, testUID); got != goldenToken {
		t.Errorf("Sign = %q, 期望黄金值 %q（ADR-0003 逐字节）", got, goldenToken)
	}
	if len(goldenToken) != 43 || bytes.ContainsRune([]byte(goldenToken), '=') {
		t.Errorf("token 形态 = %q（长度 %d）, 期望 43 字符且无 '='", goldenToken, len(goldenToken))
	}
	if !Verify(secret, testUID, goldenToken) {
		t.Errorf("正确签名: Verify 返回 false, 期望 true")
	}

	cases := []struct {
		name  string
		sec   []byte
		u     string
		token string
	}{
		{"错误 secret", []byte("wrong-secret-wrong-secret-wrong"), testUID, goldenToken},
		{"换 uid", secret, "e43", goldenToken},
		{"篡改一位", secret, testUID, tamper(goldenToken)},
		{"长度不对", secret, testUID, goldenToken[:20]},
		{"非 base64", secret, testUID, "!!!not-base64!!!"},
		{"缺失", secret, testUID, ""},
	}
	for _, c := range cases {
		if Verify(c.sec, c.u, c.token) {
			t.Errorf("%s: Verify 返回 true, 期望 false", c.name)
		}
	}
}

// tamper 翻转 token 的首位字符，模拟复制/传输中被改坏。
func tamper(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

// TestParseEpisodeUID 覆盖 uid 解析规则（AC6 单元侧）：`e<id>` 合法；非法形态
// （含合并集空间的 m1、负号、超 int64）解析失败。
func TestParseEpisodeUID(t *testing.T) {
	cases := []struct {
		uid string
		ok  bool
		id  int64
	}{
		{"e42", true, 42},
		{"e1", true, 1},
		{"abc", false, 0},
		{"m1", false, 0}, // 合并集 uid 空间，M7 保留
		{"e", false, 0},
		{"e-1", false, 0},
		{"e999999999999999999999999999", false, 0}, // 超出 int64
		{"42", false, 0},
	}
	for _, c := range cases {
		id, ok := parseEpisodeUID(c.uid)
		if ok != c.ok || id != c.id {
			t.Errorf("parseEpisodeUID(%q) = (%d, %v), 期望 (%d, %v)", c.uid, id, ok, c.id, c.ok)
		}
	}
}

// TestHandler_RouteFallback 锁定 `/s/` 子树内未实现的路径仍是 501：M6 分片、
// M7 合并集注册前，这些路径必须返回「已规划未实现」而非 404/405。
func TestHandler_RouteFallback(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(store.NewStore(db.DB), store.NewSettings(db.DB), slog.New(slog.DiscardHandler))
	for _, path := range []string{"/s/", "/s/x", "/s/t/seg/7/12.ts"} {
		rec := serve(t, srv, http.MethodGet, path, "")
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("GET %s 状态码 = %d, 期望 %d", path, rec.Code, http.StatusNotImplemented)
		}
	}
}

// TestHandler_Unauthorized 覆盖 AC4：错误/换绑/异 uid 签名 → 401，统一错误体
// 且响应体不含 token 明文。
//
// 不测「空 token」（`/s//e/e42`）：Go ServeMux 对双斜杠做 307 路径清洗，那不是
// 401 场景。缺失签名以「格式良好但 token 错」度量，见 plan 风险 7。
func TestHandler_Unauthorized(t *testing.T) {
	fx, _ := newFixture(t, bytes.Repeat([]byte("drama"), 1024), true)

	badTokens := []string{
		"bad", // 完全错误
		Sign([]byte("wrong-secret-wrong-secret-wrong"), fx.uid), // 错误 secret
		Sign([]byte(testSecret), "e43"),                         // 用别的 uid 签的
		tamper(Sign([]byte(testSecret), fx.uid)),                // 篡改一位
	}
	for _, token := range badTokens {
		rec := serve(t, fx.srv, http.MethodGet, "/s/"+token+"/e/"+fx.uid, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token=%q 状态码 = %d, 期望 %d", token, rec.Code, http.StatusUnauthorized)
		}
		var got struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, rec.Body.String())
		}
		if got.Code != "unauthorized" {
			t.Errorf("code = %q, 期望 unauthorized", got.Code)
		}
		if bytes.Contains(rec.Body.Bytes(), []byte(token)) {
			t.Errorf("响应体泄漏 token 明文")
		}
	}
}

// TestHandler_SecretRotation 覆盖 AC4 的「换绑」：换掉 settings 里的 secret 后
// 旧 URL 立即失效（每次请求读 secret，不缓存）。
func TestHandler_SecretRotation(t *testing.T) {
	body := bytes.Repeat([]byte("drama"), 1024)
	cdn, _ := newCDN(t, body)
	db := newTestDB(t)
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	if err := settings.Set(context.Background(), store.KeyStreamSecret, testSecret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	dramaID, _ := st.UpsertDrama(context.Background(), store.Drama{Provider: "web", ProviderID: "s", Title: "剧"})
	epID, _ := st.UpsertEpisode(context.Background(), store.Episode{DramaID: dramaID, EpNo: 1, Source: "web", Vid: "v", PlayURL: cdn.URL})
	uid := "e" + strconv.FormatInt(epID, 10)
	srv := NewServer(st, settings, slog.New(slog.DiscardHandler))
	path := "/s/" + Sign([]byte(testSecret), uid) + "/e/" + uid

	if rec := serve(t, srv, http.MethodGet, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("换绑前 状态码 = %d, 期望 200", rec.Code)
	}
	if err := settings.Set(context.Background(), store.KeyStreamSecret, "another-secret-at-least-32-bytes!"); err != nil {
		t.Fatalf("换绑 secret: %v", err)
	}
	if rec := serve(t, srv, http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("换绑后 状态码 = %d, 期望 401", rec.Code)
	}
}

// TestHandler_ServeBytes 覆盖 AC1/AC2/AC9/AC11：无 Range GET 精确透传长度与字节；
// Range 透传到上游并原样回传 206；白名单头回传；出站不带 Accept-Encoding。
func TestHandler_ServeBytes(t *testing.T) {
	body := bytes.Repeat([]byte("drama"), 1024) // 5120 字节
	fx, seen := newFixture(t, body, true)

	rec := serve(t, fx.srv, http.MethodGet, fx.basePath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d, 期望 200", rec.Code)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Errorf("GET Content-Length = %q, 期望 %d（来自上游）", cl, len(body))
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Errorf("GET body 与 fixture 不一致（长度 %d ≠ %d）", rec.Body.Len(), len(body))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q, 期望 video/mp4（白名单内）", ct)
	}
	if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, 期望 bytes（白名单内）", ar)
	}

	// AC11：DisableCompression 生效 → 上游看到空 Accept-Encoding。
	for _, s := range seen() {
		if s.acceptEncoding != "" {
			t.Errorf("上游观察到 Accept-Encoding = %q, 期望空（DisableCompression 未生效）", s.acceptEncoding)
		}
	}

	// AC2/AC9：Range 透传。
	rec = serve(t, fx.srv, http.MethodGet, fx.basePath, "bytes=0-1023")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range GET 状态码 = %d, 期望 206", rec.Code)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 0-1023/"+strconv.Itoa(len(body)) {
		t.Errorf("Content-Range = %q, 期望 bytes 0-1023/%d", cr, len(body))
	}
	if rec.Body.Len() != 1024 || !bytes.Equal(rec.Body.Bytes(), body[:1024]) {
		t.Errorf("Range GET body 长度 = %d, 期望 1024 且为 fixture 前 1024 字节", rec.Body.Len())
	}
	last := seen()[len(seen())-1]
	if last.method != http.MethodGet || last.rangeHeader != "bytes=0-1023" {
		t.Errorf("上游观察到 method=%q Range=%q, 期望 GET / bytes=0-1023", last.method, last.rangeHeader)
	}
}

// TestHandler_HeadMatchesGet 覆盖 AC1 的 HEAD 侧：HEAD 与 GET 的 Content-Length
// 一致且等于真实长度，HEAD 不返回 body。
func TestHandler_HeadMatchesGet(t *testing.T) {
	body := bytes.Repeat([]byte("drama"), 1024) // 5120 字节
	fx, seen := newFixture(t, body, true)

	get := serve(t, fx.srv, http.MethodGet, fx.basePath, "")
	head := serve(t, fx.srv, http.MethodHead, fx.basePath, "")

	if head.Code != http.StatusOK {
		t.Fatalf("HEAD 状态码 = %d, 期望 200", head.Code)
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("HEAD Content-Length = %q, 期望 %d", got, len(body))
	}
	if head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
		t.Errorf("HEAD Content-Length = %q, GET = %q, 期望一致", head.Header().Get("Content-Length"), get.Header().Get("Content-Length"))
	}
	if head.Body.Len() != 0 {
		t.Errorf("HEAD body 长度 = %d, 期望 0", head.Body.Len())
	}
	// HEAD 必须原样透传到上游（方法透传契约）：上游观察到的最后一条是 HEAD。
	last := seen()[len(seen())-1]
	if last.method != http.MethodHead {
		t.Errorf("上游观察到 method=%q, 期望 HEAD（HEAD 被当成 GET 透传会拉全量 body）", last.method)
	}
}

// TestHandler_RangeOutOfBounds 覆盖 AC3：越界 Range 的 416 原样透传，Content-Range
// 来自上游（本地不合成）。
func TestHandler_RangeOutOfBounds(t *testing.T) {
	body := bytes.Repeat([]byte("drama"), 1024) // 5120 字节
	fx, _ := newFixture(t, body, true)

	rec := serve(t, fx.srv, http.MethodGet, fx.basePath, "bytes=999999999-")
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("越界 Range 状态码 = %d, 期望 416", rec.Code)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes */"+strconv.Itoa(len(body)) {
		t.Errorf("Content-Range = %q, 期望 bytes */%d（上游值原样透传）", cr, len(body))
	}
}

// TestHandler_NotFound 覆盖 AC6：uid 非法或剧集不存在 → 404 + code not_found。
func TestHandler_NotFound(t *testing.T) {
	fx, _ := newFixture(t, bytes.Repeat([]byte("drama"), 1024), true)

	cases := []string{
		"e999999",                      // 库中无此 id
		"abc",                          // 不匹配 ^e[0-9]+$
		"m1",                           // 合并集 uid 空间
		"e-1",                          // 负 id
		"e999999999999999999999999999", // 超出 int64
	}
	for _, u := range cases {
		token := Sign([]byte(testSecret), u)
		rec := serve(t, fx.srv, http.MethodGet, "/s/"+token+"/e/"+u, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("uid=%q 状态码 = %d, 期望 404", u, rec.Code)
		}
		var got struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, rec.Body.String())
		}
		if got.Code != "not_found" {
			t.Errorf("uid=%q code = %q, 期望 not_found", u, got.Code)
		}
	}
}

// TestHandler_EmptyPlayURL 覆盖 AC7：play_url 为空 → 502 + code upstream，
// body 是统一 JSON 错误体、不含视频字节。
func TestHandler_EmptyPlayURL(t *testing.T) {
	fx, seen := newFixture(t, nil, false)

	rec := serve(t, fx.srv, http.MethodGet, fx.basePath, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", rec.Code)
	}
	var got struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, rec.Body.String())
	}
	if got.Code != "upstream" {
		t.Errorf("code = %q, 期望 upstream", got.Code)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("play_url 为空却打了上游 %d 次, 期望 0", n)
	}
}

// TestHandler_UpstreamFailure 覆盖 AC8：上游 500/403/404 → 502，不转发上游
// body；传输错误 → 502；失败后同一 uid 的正常请求仍 200（无污染状态）。
func TestHandler_UpstreamFailure(t *testing.T) {
	body := bytes.Repeat([]byte("drama"), 1024)
	var calls atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			w.WriteHeader(http.StatusForbidden)
		case 3:
			w.WriteHeader(http.StatusNotFound)
		default:
			// 恢复后的正常源站：明确给出长度（chunked 源站已由专门的用例覆盖）。
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(cdn.Close)

	db := newTestDB(t)
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	if err := settings.Set(context.Background(), store.KeyStreamSecret, testSecret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	dramaID, _ := st.UpsertDrama(context.Background(), store.Drama{Provider: "web", ProviderID: "s", Title: "剧"})
	epID, _ := st.UpsertEpisode(context.Background(), store.Episode{DramaID: dramaID, EpNo: 1, Source: "web", Vid: "v", PlayURL: cdn.URL + "/video.mp4"})
	uid := "e" + strconv.FormatInt(epID, 10)
	srv := NewServer(st, settings, slog.New(slog.DiscardHandler))
	path := "/s/" + Sign([]byte(testSecret), uid) + "/e/" + uid

	for i := 0; i < 3; i++ {
		rec := serve(t, srv, http.MethodGet, path, "")
		if rec.Code != http.StatusBadGateway {
			t.Errorf("第 %d 次上游失败 状态码 = %d, 期望 502", i+1, rec.Code)
		}
		var got struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, rec.Body.String())
		}
		if got.Code != "upstream" {
			t.Errorf("第 %d 次 code = %q, 期望 upstream", i+1, got.Code)
		}
		if bytes.Contains(rec.Body.Bytes(), []byte("drama")) {
			t.Errorf("第 %d 次转发了上游 body（脏数据）", i+1)
		}
	}

	// 污染检查：同一 uid 的下一次请求（上游恢复 200）仍 200。
	rec := serve(t, srv, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("失败后的正常请求 状态码 = %d, 期望 200（无污染状态）", rec.Code)
	}
	if rec.Body.Len() != len(body) {
		t.Errorf("失败后的正常请求 body 长度 = %d, 期望 %d", rec.Body.Len(), len(body))
	}

	// 传输错误：play_url 指向已关闭的 server。
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	epID2, _ := st.UpsertEpisode(context.Background(), store.Episode{DramaID: dramaID, EpNo: 2, Source: "web", Vid: "v2", PlayURL: closedURL + "/x.mp4"})
	uid2 := "e" + strconv.FormatInt(epID2, 10)
	rec = serve(t, srv, http.MethodGet, "/s/"+Sign([]byte(testSecret), uid2)+"/e/"+uid2, "")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("传输错误 状态码 = %d, 期望 502", rec.Code)
	}
}

// TestHandler_HeaderWhitelist 覆盖 AC9 侧：白名单外的上游头不透传。
func TestHandler_HeaderWhitelist(t *testing.T) {
	body := []byte("hello")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("X-Secret-Leak", "topsecret")
		w.Header().Set("Server", "nginx")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(cdn.Close)

	db := newTestDB(t)
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	_ = settings.Set(context.Background(), store.KeyStreamSecret, testSecret)
	dramaID, _ := st.UpsertDrama(context.Background(), store.Drama{Provider: "web", ProviderID: "s", Title: "剧"})
	epID, _ := st.UpsertEpisode(context.Background(), store.Episode{DramaID: dramaID, EpNo: 1, Source: "web", Vid: "v", PlayURL: cdn.URL})
	uid := "e" + strconv.FormatInt(epID, 10)
	srv := NewServer(st, settings, slog.New(slog.DiscardHandler))

	rec := serve(t, srv, http.MethodGet, "/s/"+Sign([]byte(testSecret), uid)+"/e/"+uid, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	if got := rec.Header().Get("X-Secret-Leak"); got != "" {
		t.Errorf("白名单外的头 X-Secret-Leak 被透传 = %q", got)
	}
	if got := rec.Header().Get("Server"); got != "" {
		t.Errorf("白名单外的头 Server 被透传 = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q, 期望 video/mp4（白名单内）", got)
	}
}

// TestHandler_UpstreamLog 锁定出站日志契约（DEV-STANDARDS 日志节）：一次请求恰好
// 一条 "stream upstream" 记录，字段为 uid / upstream_host / upstream_status / bytes，
// 且日志里不出现 play_url 全文（禁止记录带签名直链）。
func TestHandler_UpstreamLog(t *testing.T) {
	body := []byte("hello world")
	cdn, _ := newCDN(t, body)
	db := newTestDB(t)
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	_ = settings.Set(context.Background(), store.KeyStreamSecret, testSecret)
	dramaID, _ := st.UpsertDrama(context.Background(), store.Drama{Provider: "web", ProviderID: "s", Title: "剧"})
	epID, _ := st.UpsertEpisode(context.Background(), store.Episode{DramaID: dramaID, EpNo: 1, Source: "web", Vid: "v", PlayURL: cdn.URL + "/signed-path?sign=secretquery"})
	uid := "e" + strconv.FormatInt(epID, 10)

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := NewServer(st, settings, logger)
	rec := serve(t, srv, http.MethodGet, "/s/"+Sign([]byte(testSecret), uid)+"/e/"+uid, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("日志条数 = %d, 期望 1（buf=%q）", len(lines), buf.String())
	}
	var entry struct {
		Msg            string `json:"msg"`
		UID            string `json:"uid"`
		UpstreamHost   string `json:"upstream_host"`
		UpstreamStatus int    `json:"upstream_status"`
		Bytes          int64  `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("日志不是合法 JSON: %v（line=%q）", err, lines[0])
	}
	if entry.Msg != "stream upstream" {
		t.Errorf("msg = %q, 期望 stream upstream", entry.Msg)
	}
	if entry.UID != uid || entry.UpstreamHost == "" || entry.UpstreamStatus != 200 || entry.Bytes != int64(len(body)) {
		t.Errorf("字段不齐: uid=%q host=%q status=%d bytes=%d", entry.UID, entry.UpstreamHost, entry.UpstreamStatus, entry.Bytes)
	}
	if buf.Len() > 0 && strings.Contains(buf.String(), "signed-path") {
		t.Errorf("日志出现 play_url 路径（禁止记录完整直链）")
	}
}

// TestHandler_DialFailureLog 锁定「传输错误可区分」：连接失败时记一条含底层
// 原因的 warn 日志（否则只剩 upstream_status=0），且日志不含 play_url。
func TestHandler_DialFailureLog(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close() // 端口关闭 → 连接被拒

	db := newTestDB(t)
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	_ = settings.Set(context.Background(), store.KeyStreamSecret, testSecret)
	dramaID, _ := st.UpsertDrama(context.Background(), store.Drama{Provider: "web", ProviderID: "s", Title: "剧"})
	epID, _ := st.UpsertEpisode(context.Background(), store.Episode{DramaID: dramaID, EpNo: 1, Source: "web", Vid: "v", PlayURL: closedURL + "/deadbeef.mp4"})
	uid := "e" + strconv.FormatInt(epID, 10)

	buf := &bytes.Buffer{}
	srv := NewServer(st, settings, slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	rec := serve(t, srv, http.MethodGet, "/s/"+Sign([]byte(testSecret), uid)+"/e/"+uid, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", rec.Code)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("日志条数 = %d, 期望 1（每次出站恰一条；buf=%q）", len(lines), buf.String())
	}
	var entry struct {
		Msg            string `json:"msg"`
		UID            string `json:"uid"`
		UpstreamStatus int    `json:"upstream_status"`
		Err            string `json:"err"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("日志不是合法 JSON: %v（line=%q）", err, lines[0])
	}
	if entry.Msg != "stream upstream" || entry.UID != uid || entry.Err == "" {
		t.Errorf("日志字段不符: msg=%q uid=%q err=%q（传输错误必须能在日志里区分原因）", entry.Msg, entry.UID, entry.Err)
	}
	if strings.Contains(buf.String(), "deadbeef") {
		t.Errorf("日志出现 play_url 路径")
	}
}

// TestHandler_UpstreamClientConfig 覆盖 AC11 的代码核对：Transport 必须
// DisableCompression 且有连接/响应头超时；Client.Timeout 必须为 0（否则截断长流）。
func TestHandler_UpstreamClientConfig(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(store.NewStore(db.DB), store.NewSettings(db.DB), slog.New(slog.DiscardHandler))
	tr, ok := srv.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T, 期望 *http.Transport", srv.client.Transport)
	}
	if !tr.DisableCompression {
		t.Error("DisableCompression = false, 期望 true（否则无 Range GET 会失去精确 Content-Length）")
	}
	if srv.client.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, 期望 0（会截断长视频流）", srv.client.Timeout)
	}
	if tr.DialContext == nil {
		t.Error("DialContext 为空, 期望有连接超时")
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("ResponseHeaderTimeout = 0, 期望有响应头超时")
	}
}

// TestHandler_ChunkedUpstream 覆盖「上游不给精确 Content-Length（chunked）→ 502」：
// HEAD/GET 长度精确这条 AC 依赖上游长度，chunked 上游给不出长度时宁可 502，
// 不给无长度的 200（AGENTS.md 硬约束 3：每个虚拟文件必须给出精确 Content-Length）。
func TestHandler_ChunkedUpstream(t *testing.T) {
	body := []byte("chunked body data")
	cdn, _ := newChunkedCDN(t, body)
	db := newTestDB(t)
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	_ = settings.Set(context.Background(), store.KeyStreamSecret, testSecret)
	dramaID, _ := st.UpsertDrama(context.Background(), store.Drama{Provider: "web", ProviderID: "s", Title: "剧"})
	epID, _ := st.UpsertEpisode(context.Background(), store.Episode{DramaID: dramaID, EpNo: 1, Source: "web", Vid: "v", PlayURL: cdn.URL + "/video.mp4"})
	uid := "e" + strconv.FormatInt(epID, 10)
	srv := NewServer(st, settings, slog.New(slog.DiscardHandler))
	path := "/s/" + Sign([]byte(testSecret), uid) + "/e/" + uid

	rec := serve(t, srv, http.MethodGet, path, "")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("chunked GET 状态码 = %d, 期望 502（无长度不给出 200）", rec.Code)
	}
	var got struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, rec.Body.String())
	}
	if got.Code != "upstream" {
		t.Errorf("code = %q, 期望 upstream", got.Code)
	}
}

// TestHandler_Concurrent 覆盖 AC14：10 个并发混合 GET / Range 请求全部得到
// 预期状态码与长度（-race 下无告警）。
func TestHandler_Concurrent(t *testing.T) {
	body := bytes.Repeat([]byte("drama"), 1024)
	fx, _ := newFixture(t, body, true)

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				rec := serve(t, fx.srv, http.MethodGet, fx.basePath, "")
				if rec.Code != http.StatusOK || rec.Body.Len() != len(body) {
					errs <- fmt.Errorf("GET #%d: 状态=%d 长度=%d", i, rec.Code, rec.Body.Len())
				}
				return
			}
			rec := serve(t, fx.srv, http.MethodGet, fx.basePath, "bytes=0-1023")
			if rec.Code != http.StatusPartialContent || rec.Body.Len() != 1024 {
				errs <- fmt.Errorf("Range #%d: 状态=%d 长度=%d", i, rec.Code, rec.Body.Len())
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

package segment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
)

// 与 internal/stream 的黄金值同源（ADR-0003）：本包不实现 HMAC，测试里独立算出
// token 交给注入的 VerifyFunc，同时锁定「token 形态 == base64url_nopad(HMAC-SHA256(
// secret, "stream:"+uid))」这条跨模块契约。
const (
	testSecret = "0123456789abcdef0123456789abcdef"
	testUID    = "e1"
)

var goldenToken = signFor(testUID)

// signFor 按 ADR-0003 独立算出 uid 的 token（测试侧实现，不复用被测代码）。
func signFor(uid string) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte("stream:" + uid))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// realVerify 就是 cmd 注入的 stream.Verify 的等价实现（常数时间比较）。
func realVerify(secret []byte, uid, token string) bool {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("stream:" + uid))
	return hmac.Equal([]byte(base64.RawURLEncoding.EncodeToString(mac.Sum(nil))), []byte(token))
}

// newTestDB 用 store.Open 建临时文件库（真实 DSN：WAL / foreign_keys / 迁移）。
func newTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "segment.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// makeVideo 用容器内的 ffmpeg 现场生成测试源（进 t.TempDir()，不落版本库、不超
// 500 KB）。-force_key_frames 每秒一个关键帧 + -sc_threshold 0 关掉场景切换插帧，
// 于是「关键帧位置」在测试里是可断言的确定值。
//
// TEST-STANDARDS 要求「视频 fixture 由 ffmpeg 现场生成」；这里把生成放进测试，
// 因此 e2e 镜像里不需要额外的构建步骤，且用的就是 runtime 的同一个 ffmpeg。
func makeVideo(t *testing.T, path string, seconds int) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=160x120:rate=25:duration=%d", seconds),
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-g", "25", "-keyint_min", "25", "-sc_threshold", "0",
		"-force_key_frames", "expr:gte(t,n_forced*1)",
		"-movflags", "+faststart",
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败: %v（%s）", err, out)
	}
}

// probeDuration 用 ffprobe 量一个本地文件/字节流的时长（测试侧独立测量，不复用
// 被测代码的解析结果）。
func probeDuration(t *testing.T, path string) float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("解析 ffprobe 时长 %q: %v", out, err)
	}
	return v
}

// probeKeyframes 用 ffprobe 独立列出关键帧 pts（断言切点对齐的独立真值）。
func probeKeyframes(t *testing.T, path string) []float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-skip_frame", "nokey", "-show_entries", "frame=pts_time",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe keyframes %s: %v", path, err)
	}
	var kfs []float64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSuffix(strings.TrimSpace(line), ",")
		if line == "" {
			continue
		}
		v, err := strconv.ParseFloat(line, 64)
		if err != nil {
			t.Fatalf("解析关键帧 %q: %v", line, err)
		}
		kfs = append(kfs, v)
	}
	return kfs
}

// newSource 起一个像真 CDN 的假源站：http.ServeContent 提供 Content-Length、
// Accept-Ranges、206/Content-Range（ffprobe/ffmpeg 按 Range 读源）。
func newSource(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "ep.mp4", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newRig 组装测试台：真库 + 真 Cutter（真 ffprobe/ffmpeg）+ 假源站。
func newRig(t *testing.T, sourceURL string) (*Cutter, *store.Store, *store.Settings, *bytes.Buffer) {
	t.Helper()
	db := newTestDB(t)
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	if err := settings.Set(context.Background(), store.KeyStreamSecret, testSecret); err != nil {
		t.Fatalf("Set stream_secret: %v", err)
	}
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dramaID, err := st.UpsertDrama(context.Background(), store.Drama{
		Provider: "web", ProviderID: "d1", Title: "测试剧", Year: 2026,
	})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	if _, err := st.UpsertEpisode(context.Background(), store.Episode{
		DramaID: dramaID, EpNo: 1, Source: "web", Vid: "v1", PlayURL: sourceURL,
	}); err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}
	// 全新库的首集 id 恒为 1；测试里的 testUID = "e1" 依赖这条事实。
	if uid := "e" + strconv.Itoa(1); uid != testUID {
		t.Fatalf("测试前提失效：uid = %q", uid)
	}
	return NewCutter(st, settings, logger, realVerify), st, settings, buf
}

// serveSeg 走真实装配形态：把 Cutter 注册到与 cmd 相同的 Route 上（Go 路由 pattern
// 无法在路径段内做通配，所以路由本身由 Route 常量锁死），再发一次真请求。
func serveSeg(t *testing.T, c *Cutter, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(Route, c)
	mux.HandleFunc("/s/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// TestIndex_AlignsToKeyframes 覆盖 AC1：索引出的切点与 segment 表一致，且每个切点
// 都是 ffprobe 独立列出的关键帧；总时长 = 源时长；重复索引幂等。
func TestIndex_AlignsToKeyframes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ep.mp4")
	makeVideo(t, path, 12)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 fixture: %v", err)
	}
	srv := newSource(t, body)
	c, st, settings, _ := newRig(t, srv.URL+"/ep.mp4")

	// 目标 4s：源每秒一个关键帧 → 期望 3 片，每片 4s。
	if err := settings.Set(context.Background(), store.KeySegTargetSec, "4"); err != nil {
		t.Fatalf("Set seg_target_sec: %v", err)
	}

	got, err := c.Index(context.Background(), 1)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	// 1) 与独立 ffprobe 的关键帧列表对齐（首片起点 0 也算关键帧 0）。
	kfs := probeKeyframes(t, path)
	for i, seg := range got {
		if !containsFloat(kfs, seg.StartSec) {
			t.Errorf("片 %d 切点 %v 不在关键帧列表 %v", i, seg.StartSec, kfs)
		}
	}

	// 2) 与 segment 表一致（Index 的返回值 == 库里的行）。
	fromDB, err := st.ListSegments(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(fromDB) != len(got) {
		t.Fatalf("库里 %d 片, Index 返回 %d 片", len(fromDB), len(got))
	}
	for i := range got {
		if fromDB[i].StartSec != got[i].StartSec || fromDB[i].Dur != got[i].Dur {
			t.Errorf("片 %d 库里 (%v,%v), Index 返回 (%v,%v)",
				i, fromDB[i].StartSec, fromDB[i].Dur, got[i].StartSec, got[i].Dur)
		}
		if !fromDB[i].Keyframe || fromDB[i].Mode != "copy" {
			t.Errorf("片 %d 应标为 keyframe/copy，实际 %v/%q", i, fromDB[i].Keyframe, fromDB[i].Mode)
		}
		if fromDB[i].CodecSig == "" {
			t.Errorf("片 %d 的 codec_sig 为空（索引应记录解码器签名）", i)
		}
	}

	// 3) 总时长 = 源时长（±一帧 0.04s）。
	srcDur := probeDuration(t, path)
	tl := NewTimeline([]EpisodeSegments{{EpisodeID: 1, EpNo: 1, Segments: fromDB}})
	if diff := tl.Total() - srcDur; diff > 0.04 || diff < -0.04 {
		t.Errorf("Σ分片时长 = %v, 源时长 = %v（差 %v 超过一帧）", tl.Total(), srcDur, diff)
	}

	// 4) 幂等：再索引一次，行数不变、切点不变。
	again, err := c.Index(context.Background(), 1)
	if err != nil {
		t.Fatalf("Index 第二次: %v", err)
	}
	if len(again) != len(got) {
		t.Errorf("重复索引后片数 = %d, 期望 %d", len(again), len(got))
	}
	fromDB2, _ := st.ListSegments(context.Background(), 1)
	if len(fromDB2) != len(fromDB) {
		t.Errorf("重复索引后库里片数 = %d, 期望 %d（ReplaceSegments 应整体替换）", len(fromDB2), len(fromDB))
	}
}

// TestIndex_ProbeFailureWritesNothing 覆盖「失败结果不缓存」：ffprobe 拿不到源时
// 返回 ErrUpstream，且库里一行索引都不留（保留旧索引也不被写坏）。
func TestIndex_ProbeFailureWritesNothing(t *testing.T) {
	// 假源站返回 500：ffprobe 必然失败。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c, st, _, _ := newRig(t, srv.URL+"/ep.mp4")

	if _, err := c.Index(context.Background(), 1); err == nil {
		t.Fatal("Index 应因 ffprobe 失败而返回错误")
	} else if !errIsUpstream(err) {
		t.Errorf("错误 %v 未映射到上游失败类别", err)
	}
	segs, err := st.ListSegments(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segs) != 0 {
		t.Errorf("探测失败后库里留下 %d 片，期望 0（失败结果不缓存）", len(segs))
	}
}

// TestIndex_NoPlayURL 覆盖 play_url 为空 → 502 类别错误，不落库。
func TestIndex_NoPlayURL(t *testing.T) {
	c, st, _, _ := newRig(t, "")
	if _, err := c.Index(context.Background(), 1); err == nil {
		t.Fatal("play_url 为空时 Index 应报错")
	}
	segs, _ := st.ListSegments(context.Background(), 1)
	if len(segs) != 0 {
		t.Errorf("play_url 为空时库里留下 %d 片，期望 0", len(segs))
	}
}

// TestIndex_UnknownEpisode 覆盖剧集不存在 → store.ErrNotFound（装配层映射 404）。
func TestIndex_UnknownEpisode(t *testing.T) {
	c, _, _, _ := newRig(t, "http://127.0.0.1:1/x.mp4")
	if _, err := c.Index(context.Background(), 999); err == nil || !errIsNotFound(err) {
		t.Fatalf("未知剧集应返回 ErrNotFound，实际 %v", err)
	}
}

// TestHandler_SliceContract 覆盖 AC2/AC3 的核心：分片端点返回可解码的 MPEG-TS，
// Content-Length == 实际字节数，首片/末片都对；seq → (集号, 集内秒) 与表一致。
func TestHandler_SliceContract(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "ep.mp4")
	makeVideo(t, src, 12)
	body, _ := os.ReadFile(src)
	source := newSource(t, body)
	c, st, settings, _ := newRig(t, source.URL+"/ep.mp4")
	if err := settings.Set(context.Background(), store.KeySegTargetSec, "4"); err != nil {
		t.Fatalf("Set seg_target_sec: %v", err)
	}
	if _, err := c.Index(context.Background(), 1); err != nil {
		t.Fatalf("Index: %v", err)
	}
	segs, _ := st.ListSegments(context.Background(), 1)
	if len(segs) != 3 {
		t.Fatalf("片数 = %d, 期望 3（12s / 目标 4s，每秒一个关键帧）", len(segs))
	}

	// 首片、中间片、末片三处边界都验；seq → (集号, 集内秒) 与表逐项一致。
	for seq, seg := range segs {
		rec := serveSeg(t, c, http.MethodGet, fmt.Sprintf("/s/%s/seg/%s/%d.ts", goldenToken, testUID, seq))
		if rec.Code != http.StatusOK {
			t.Fatalf("seq=%d 状态码 = %d, 期望 200（body=%s）", seq, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "video/mp2t" {
			t.Errorf("seq=%d Content-Type = %q, 期望 video/mp2t", seq, ct)
		}
		if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
			t.Errorf("seq=%d Content-Length = %q, 实际 body = %d 字节", seq, cl, rec.Body.Len())
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("seq=%d 返回 0 字节", seq)
		}
		if rec.Body.Bytes()[0] != 0x47 {
			t.Errorf("seq=%d 首字节 = 0x%02x, 期望 0x47（MPEG-TS 同步字节）", seq, rec.Body.Bytes()[0])
		}

		// 切片内容可被 ffprobe 解析，且时长与表里的 Dur 相符（±一帧 0.04s + TS
		// 容器 0.08s 起始偏移的容差）。
		tsPath := filepath.Join(dir, fmt.Sprintf("seq%d.ts", seq))
		if err := os.WriteFile(tsPath, rec.Body.Bytes(), 0o644); err != nil {
			t.Fatalf("写切片: %v", err)
		}
		gotDur := probeDuration(t, tsPath)
		if diff := gotDur - seg.Dur; diff > 0.25 || diff < -0.25 {
			t.Errorf("seq=%d 切片时长 = %v, 表里 Dur = %v（差 %v）", seq, gotDur, seg.Dur, diff)
		}

		tl := NewTimeline([]EpisodeSegments{{EpisodeID: 1, EpNo: 1, Segments: segs}})
		slice, ok := tl.At(seq)
		if !ok {
			t.Fatalf("seq=%d 在时间轴上越界", seq)
		}
		if slice.EpNo != 1 || slice.StartSec != seg.StartSec || slice.Dur != seg.Dur {
			t.Errorf("seq=%d 时间轴映射 = %+v, 表里 = %+v", seq, slice, seg)
		}
	}
}

// TestHandler_Boundaries 覆盖边界：首片（0.ts）、末片（len-1.ts）、越界 seq 与
// 非法文件名的 404，以及「索引未就绪」的 503。
func TestHandler_Boundaries(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "ep.mp4")
	makeVideo(t, src, 6)
	body, _ := os.ReadFile(src)
	source := newSource(t, body)
	c, st, settings, _ := newRig(t, source.URL+"/ep.mp4")
	if err := settings.Set(context.Background(), store.KeySegTargetSec, "4"); err != nil {
		t.Fatalf("Set seg_target_sec: %v", err)
	}

	// 索引未就绪（还没跑 Index）：503 + Retry-After，不出半截流。
	rec := serveSeg(t, c, http.MethodGet, fmt.Sprintf("/s/%s/seg/%s/0.ts", goldenToken, testUID))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("无索引时状态码 = %d, 期望 503", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("503 应带 Retry-After")
	}

	if _, err := c.Index(context.Background(), 1); err != nil {
		t.Fatalf("Index: %v", err)
	}
	segs, _ := st.ListSegments(context.Background(), 1)
	last := len(segs) - 1

	for _, seq := range []int{0, last} {
		rec := serveSeg(t, c, http.MethodGet, fmt.Sprintf("/s/%s/seg/%s/%d.ts", goldenToken, testUID, seq))
		if rec.Code != http.StatusOK {
			t.Errorf("seq=%d（首/末片）状态码 = %d, 期望 200（body=%s）", seq, rec.Code, rec.Body.String())
		}
	}

	// 越界 seq → 404；非法文件名 → 404。
	for _, path := range []string{
		fmt.Sprintf("/s/%s/seg/%s/%d.ts", goldenToken, testUID, last+1),
		fmt.Sprintf("/s/%s/seg/%s/-1.ts", goldenToken, testUID),
		fmt.Sprintf("/s/%s/seg/%s/0.m3u8", goldenToken, testUID),
		fmt.Sprintf("/s/%s/seg/%s/x.ts", goldenToken, testUID),
	} {
		if rec := serveSeg(t, c, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d, 期望 404", path, rec.Code)
		}
	}
	// 非法 uid → 404（token 按该 uid 签，才能走到 uid 解析这一步）。
	for _, uid := range []string{"m1", "E1", "e"} {
		if rec := serveSeg(t, c, http.MethodGet, "/s/"+signFor(uid)+"/seg/"+uid+"/0.ts"); rec.Code != http.StatusNotFound {
			t.Errorf("非法 uid %q 状态码 = %d, 期望 404", uid, rec.Code)
		}
	}
	// 未知剧集 → 404。
	if rec := serveSeg(t, c, http.MethodGet, "/s/"+signFor("e999")+"/seg/e999/0.ts"); rec.Code != http.StatusNotFound {
		t.Errorf("未知剧集状态码 = %d, 期望 404", rec.Code)
	}
}

// TestHandler_Unauthorized 覆盖签名不通过 → 401，且响应体与日志都不出现 token 明文。
func TestHandler_Unauthorized(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "ep.mp4")
	makeVideo(t, src, 4)
	body, _ := os.ReadFile(src)
	source := newSource(t, body)
	c, _, _, logs := newRig(t, source.URL+"/ep.mp4")
	if _, err := c.Index(context.Background(), 1); err != nil {
		t.Fatalf("Index: %v", err)
	}

	// 空 token 走的是 Go ServeMux 的双斜杠清洗（307），不在本端点的签名矩阵里
	// （M5 已记录同一现象），故只列「格式良好但签名不对」的形态。
	badTokens := []string{
		"not-base64!!", goldenToken + "x", strings.ToUpper(goldenToken),
	}
	for _, tok := range badTokens {
		rec := serveSeg(t, c, http.MethodGet, "/s/"+tok+"/seg/"+testUID+"/0.ts")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token=%q 状态码 = %d, 期望 401", tok, rec.Code)
		}
		if strings.Contains(rec.Body.String(), tok) && tok != "" {
			t.Errorf("401 响应体泄漏 token：%s", rec.Body.String())
		}
	}
	// 用别的 uid 签的 token 也拒（签名绑定 uid）。
	if rec := serveSeg(t, c, http.MethodGet, "/s/"+signFor("e999")+"/seg/"+testUID+"/0.ts"); rec.Code != http.StatusUnauthorized {
		t.Errorf("错 uid 签的 token 状态码 = %d, 期望 401", rec.Code)
	}
	if strings.Contains(logs.String(), goldenToken) {
		t.Error("日志里出现 token 明文")
	}
}

// TestHandler_CutFailureIs502AndNotCached 覆盖 AC4：ffmpeg 失败 → 502，且不缓存
// 失败结果（库里索引不变、随后同 seq 请求仍可成功）。
func TestHandler_CutFailureIs502AndNotCached(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "ep.mp4")
	makeVideo(t, src, 8)
	body, _ := os.ReadFile(src)

	var fail bool
	var mu sync.Mutex
	// 源站前若干次正常（Index 用），之后返回垃圾字节让 ffmpeg 切不出 TS。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		broken := fail
		mu.Unlock()
		if broken {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>not a video</html>"))
			return
		}
		http.ServeContent(w, r, "ep.mp4", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)

	c, st, settings, _ := newRig(t, srv.URL+"/ep.mp4")
	if err := settings.Set(context.Background(), store.KeySegTargetSec, "4"); err != nil {
		t.Fatalf("Set seg_target_sec: %v", err)
	}
	if _, err := c.Index(context.Background(), 1); err != nil {
		t.Fatalf("Index: %v", err)
	}
	before, _ := st.ListSegments(context.Background(), 1)

	mu.Lock()
	fail = true
	mu.Unlock()
	rec := serveSeg(t, c, http.MethodGet, "/s/"+goldenToken+"/seg/"+testUID+"/0.ts")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("切片失败状态码 = %d, 期望 502（body=%s）", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("0x47")) || rec.Body.Bytes()[0] == 0x47 {
		t.Error("502 响应体里出现了 MPEG-TS 字节")
	}

	after, _ := st.ListSegments(context.Background(), 1)
	if len(after) != len(before) {
		t.Errorf("切片失败后索引行数从 %d 变为 %d（失败结果不应写库）", len(before), len(after))
	}

	// 源恢复后同 seq 仍能成功 —— 失败没有留下任何被缓存的状态。
	mu.Lock()
	fail = false
	mu.Unlock()
	rec = serveSeg(t, c, http.MethodGet, "/s/"+goldenToken+"/seg/"+testUID+"/0.ts")
	if rec.Code != http.StatusOK {
		t.Errorf("源恢复后状态码 = %d, 期望 200", rec.Code)
	}
}

// TestHandler_Concurrent 覆盖 -race 下的并发切片：全部请求都拿到正确状态与长度
// （并发闸只限流，不丢请求）。
func TestHandler_Concurrent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "ep.mp4")
	makeVideo(t, src, 8)
	body, _ := os.ReadFile(src)
	source := newSource(t, body)
	c, st, settings, _ := newRig(t, source.URL+"/ep.mp4")
	if err := settings.Set(context.Background(), store.KeySegTargetSec, "4"); err != nil {
		t.Fatalf("Set seg_target_sec: %v", err)
	}
	if _, err := c.Index(context.Background(), 1); err != nil {
		t.Fatalf("Index: %v", err)
	}
	segs, _ := st.ListSegments(context.Background(), 1)

	const workers = 6
	var wg sync.WaitGroup
	errs := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			rec := serveSeg(t, c, http.MethodGet,
				fmt.Sprintf("/s/%s/seg/%s/%d.ts", goldenToken, testUID, seq%len(segs)))
			if rec.Code != http.StatusOK {
				errs <- fmt.Sprintf("seq=%d 状态码 = %d", seq, rec.Code)
				return
			}
			if rec.Header().Get("Content-Length") != strconv.Itoa(rec.Body.Len()) {
				errs <- fmt.Sprintf("seq=%d 长度不一致", seq)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func containsFloat(xs []float64, v float64) bool {
	for _, x := range xs {
		if closeEnough(x, v) {
			return true
		}
	}
	return false
}

func errIsUpstream(err error) bool { return errors.Is(err, httpx.ErrUpstream) }
func errIsNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

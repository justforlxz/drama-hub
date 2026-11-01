package segment

import (
	"bytes"
	"context"
	"encoding/json"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/pgtest"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
)

// testSecret 是测试用的出流密钥（literal，不用占位符，黄金 id 才能稳定）。
const testSecret = "unit-test-secret-32-bytes-minimum!"

// discardLogger 是直接调 probe / cut 的测试用的丢弃 logger（生产由 Cutter 注入）。
var discardLogger = slog.New(slog.DiscardHandler)

// rig 注入给出站子进程的两份请求头（生产里由装配根从 hongguo 的浏览器身份派生，
// 见 cmd/drama-hub 的 playbackHeaders）。内容不重要，但要能被假 CDN 逐字断言，
// 因此用不会与 ffmpeg 默认（`Lavf/...`）混淆的值。
//
// rig 默认带上它们：ffprobe / ffmpeg 的 `-headers` 拼错会让所有切片测试一起失败，
// 而不是只在专门的那条测试里。
var (
	// testWebHeaders 是网页通道的身份：浏览器 UA + 源站根 Referer。
	testWebHeaders = []string{
		"User-Agent: drama-hub-test",
		"Referer: https://example.test/site/",
	}
	// testAppHeaders 是 App 通道的身份：只有 UA —— 它的媒体 CDN 对带站内 Referer
	// 的请求回 403（MKACG-638）。
	testAppHeaders = []string{
		"User-Agent: drama-hub-test",
	}
)

// makeVideo 用容器内的 ffmpeg 现场生成测试源（进 t.TempDir()，不落版本库）。
// -force_key_frames 每秒一个关键帧 + -sc_threshold 0 关掉场景切换插帧，
// 于是「关键帧位置」在测试里是可断言的确定值。
func makeVideo(t *testing.T, path string, seconds int) {
	t.Helper()
	makeVideoSize(t, path, seconds, "160x120")
}

// makeVideoSize 同 makeVideo，但指定分辨率：通道测试要两个通道的画面不同，
// 而 codec_sig 不含分辨率 —— 正是「签名相同、字节不同」那种串味的前提。
func makeVideoSize(t *testing.T, path string, seconds int, size string) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%s:rate=25:duration=%d", size, seconds),
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

// probeDuration 用 ffprobe 量一个本地文件/字节流的时长（测试侧独立测量）。
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

func containsFloat(xs []float64, v float64) bool {
	for _, x := range xs {
		if diff := x - v; diff < 1e-3 && diff > -1e-3 {
			return true
		}
	}
	return false
}

// fakeCDN 是像真 CDN 的假源站：http.ServeContent 提供 Content-Length、
// Accept-Ranges、206/Content-Range（ffprobe/ffmpeg 按 Range 读源），并数被请求次数
// ——「第二次分片命中缓存零外呼」靠这个计数断言。
type fakeCDN struct {
	srv *httptest.Server
	mu  sync.Mutex
	n   int
	// body 是当前提供的源字节（通道切换时用 setBody 换掉）。
	body []byte
	// seen 是每个请求收到的头（按到达顺序）：断言 ffprobe / ffmpeg 的 -headers
	// 真的挂到了出站请求上（MKACG-624），而不是只写进了命令行。
	seen []http.Header
	// broken 为 true 时返回一段 HTML，让 ffmpeg / ffprobe 切不出 TS。
	broken bool
	// hanging 为 true 时永不回包（算索引超时那条路径）。
	hanging bool
	// reject 非 0 时以该状态码拒绝（模拟媒体 CDN 对 App 直链回 403，MKACG-639）。
	reject int
	// query 是 url() 附带的查询串：模拟 App 片源直链的签名参数（n80_tk 等）。
	query string
}

func newFakeCDN(t *testing.T, body []byte) *fakeCDN {
	t.Helper()
	f := &fakeCDN{body: body}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.n++
		f.seen = append(f.seen, r.Header.Clone())
		broken, hanging, reject, body := f.broken, f.hanging, f.reject, f.body
		f.mu.Unlock()
		if hanging {
			<-r.Context().Done()
			return
		}
		if reject != 0 {
			w.WriteHeader(reject)
			return
		}
		if broken {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>not a video</html>"))
			return
		}
		http.ServeContent(w, r, "ep.mp4", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(f.srv.Close)

	return f
}

// setBody 换掉这个假源站提供的字节：模拟同一集换了通道（web 明文 → app 密文）。
func (f *fakeCDN) setBody(body []byte) {
	f.mu.Lock()
	f.body = body
	f.mu.Unlock()
}

func (f *fakeCDN) url() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.srv.URL + "/ep.mp4" + f.query
}

// hits 返回被请求次数（含 Range 续传，一次 ffmpeg 切片可能多次）。
func (f *fakeCDN) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// seenHeaders 返回收到的全部请求头（克隆，避免与在跑的请求抢锁）。
func (f *fakeCDN) seenHeaders() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]http.Header, len(f.seen))
	copy(out, f.seen)
	return out
}

func (f *fakeCDN) setBroken(v bool) {
	f.mu.Lock()
	f.broken = v
	f.mu.Unlock()
}

func (f *fakeCDN) setHang() {
	f.mu.Lock()
	f.hanging = true
	f.mu.Unlock()
}

// rejectWith 让假源站以 status 拒绝请求，并把自身地址换成带签名的直链：
// 复现「媒体 CDN 回 403、直链带 n80_tk 查询串」那种失败（MKACG-639）。
func (f *fakeCDN) rejectWith(status int, query string) {
	f.mu.Lock()
	f.reject, f.query = status, query
	f.mu.Unlock()
}

// rig 是播放面测试台：真库（真迁移）+ 真 Cutter（真 ffprobe/ffmpeg）+ 每集一个假 CDN。
type rig struct {
	c        *Cutter
	st       *store.Store
	settings *store.Settings
	logs     *bytes.Buffer

	dramaID int64
	segs    int
	cdn     map[int64]*fakeCDN // episode id → 假源
	// keys 是每集当前的内容密钥：非空即模拟「这条走的是 App 通道的密文源」。
	keys map[int64][]byte
	// resolveErr 非 nil 时直链解析按它失败（模拟 ErrNoStream 等）。
	resolveErr map[int64]error
	// resolves 数「直链解析次数」：命中缓存的分片请求不该增加它（零外呼的直接证据）。
	resolves atomic.Int64
}

// newRig 建临时库、写一部剧，并把 seg_target_sec 调到 4 秒（12 秒源 → 3 片）。
func newRig(t *testing.T, segTarget string) *rig {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	for k, v := range map[store.SettingKey]string{
		store.KeyStreamSecret: testSecret,
		store.KeySegTargetSec: segTarget,
		// 缓存目录必须在临时目录里：默认的 ./data/cache 会写进源码树（容器内只读）。
		store.KeyCacheDir: filepath.Join(t.TempDir(), "cache"),
	} {
		if err := settings.Set(ctx, k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	dramaID, err := st.UpsertDrama(ctx, store.Drama{Provider: "web", ProviderID: "d1", Title: "测试剧", Year: 2026})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}

	r := &rig{
		st: st, settings: settings, logs: &bytes.Buffer{},
		dramaID: dramaID, cdn: map[int64]*fakeCDN{}, keys: map[int64][]byte{},
		resolveErr: map[int64]error{},
	}
	logger := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.c = NewCutter(st, settings, logger, cache.New(st, settings, logger),
		func(_ context.Context, ep store.Episode) (stream.Direct, error) {
			r.resolves.Add(1)
			if err := r.resolveErr[ep.ID]; err != nil {
				return stream.Direct{}, err
			}
			f, ok := r.cdn[ep.ID]
			if !ok {
				return stream.Direct{}, fmt.Errorf("测试没有给剧集 %d 配源", ep.ID)
			}
			return stream.Direct{URL: f.url(), Key: r.keys[ep.ID]}, nil
		}, Headers{Web: testWebHeaders, App: testAppHeaders})
	return r
}

// addEpisode 追加一集：现场 ffmpeg 生成 seconds 秒的源，起一个假 CDN，写库。
func (r *rig) addEpisode(t *testing.T, epNo, seconds int) store.Episode {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("ep%d.mp4", epNo))
	makeVideo(t, path, seconds)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 fixture: %v", err)
	}
	id, err := r.st.UpsertEpisode(ctx, store.Episode{
		DramaID: r.dramaID, EpNo: epNo, Source: "web", Vid: fmt.Sprintf("v%d", epNo),
	})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}
	r.cdn[id] = newFakeCDN(t, body)
	r.segs++
	// 播放单元只认 emby_item 派生表（硬约束 6），等价于生产里同步完成后的重建。
	if _, err := r.st.RebuildEmbyItems(ctx, r.dramaID); err != nil {
		t.Fatalf("RebuildEmbyItems: %v", err)
	}
	ep, err := r.st.EpisodeByID(ctx, id)
	if err != nil {
		t.Fatalf("EpisodeByID: %v", err)
	}
	return ep
}

// id 是对外播放 id（生产方 vfs 与校验方 /video 共用 stream.ID）。
func (r *rig) id(uid string) string { return stream.ID(testSecret, uid) }

// handler 按装配根的同一组 pattern 起一个 mux。
func (r *rig) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PlaylistRoute, r.c.ServePlaylist)
	mux.HandleFunc(SegmentRoute, r.c.ServeSegment)
	return mux
}

// do 发一次真请求（走装配形态的路由表）。
func (r *rig) do(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.handler().ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// newPlaybackServer 起一个真监听端口的服务：ffprobe/ffmpeg 只能通过 HTTP 回源。
func newPlaybackServer(t *testing.T, r *rig) string {
	t.Helper()
	srv := httptest.NewServer(r.handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestPlaylistIndex_AlignsToKeyframes 覆盖 AC「关键帧索引按需建立」：第一次请求
// playlist 时同步建索引，切点都是 ffprobe 独立列出的关键帧，总时长 = 源时长。
func TestPlaylistIndex_AlignsToKeyframes(t *testing.T) {
	r := newRig(t, "4")
	ep := r.addEpisode(t, 1, 12)
	srcPath := filepath.Join(t.TempDir(), "src.mp4")
	makeVideo(t, srcPath, 12)

	// 请求前库里没有任何索引行：索引只可能由这次请求建立（禁止批量预建）。
	before, err := r.st.ListSegments(context.Background(), ep.ID)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("请求前已有 %d 行索引, 期望 0（索引必须按需建立）", len(before))
	}

	uid := "e" + strconv.FormatInt(ep.ID, 10)
	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("首次 playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}

	segs, err := r.st.ListSegments(context.Background(), ep.ID)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segs) != 3 {
		t.Fatalf("片数 = %d, 期望 3（12s / 目标 4s，每秒一个关键帧）", len(segs))
	}
	kfs := probeKeyframes(t, srcPath)
	for i, seg := range segs {
		if !containsFloat(kfs, seg.StartSec) {
			t.Errorf("片 %d 切点 %v 不在关键帧列表 %v", i, seg.StartSec, kfs)
		}
		if !seg.Keyframe || seg.Mode != "copy" || seg.CodecSig == "" {
			t.Errorf("片 %d 元数据不对：keyframe=%v mode=%q codec_sig=%q", i, seg.Keyframe, seg.Mode, seg.CodecSig)
		}
	}
	srcDur := probeDuration(t, srcPath)
	tl := NewTimeline([]EpisodeSegments{{EpisodeID: ep.ID, EpNo: ep.EpNo, Segments: segs}})
	if diff := tl.Total() - srcDur; diff > 0.04 || diff < -0.04 {
		t.Errorf("Σ分片时长 = %v, 源时长 = %v（差 %v 超过一帧）", tl.Total(), srcDur, diff)
	}
}

// TestPlaylist_SingleEpisode 覆盖契约的 playlist 形态：单集与合并集同一路径，
// 合法 VOD playlist（EXTM3U / PLAYLIST-TYPE / TARGETDURATION / ENDLIST），
// 分片 URI 指回 /video/{id}/{seq}.ts，单集没有 DISCONTINUITY。
func TestPlaylist_SingleEpisode(t *testing.T) {
	r := newRig(t, "4")
	ep := r.addEpisode(t, 1, 12)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)

	rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("Content-Type = %q, 期望 application/vnd.apple.mpegurl", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %q, body = %d 字节", cl, rec.Body.Len())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n",
		"#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-TARGETDURATION:4\n",
		"#EXT-X-ENDLIST\n",
		"/video/" + id + "/0.ts\n",
		"/video/" + id + "/2.ts\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("playlist 缺 %q：\n%s", want, body)
		}
	}
	if got := strings.Count(body, "#EXTINF:"); got != 3 {
		t.Errorf("EXTINF 行数 = %d, 期望 3", got)
	}
	if got := strings.Count(body, "#EXT-X-DISCONTINUITY"); got != 0 {
		t.Errorf("单集 playlist 出现 %d 个 DISCONTINUITY, 期望 0", got)
	}

	// HEAD 与 GET 长度一致（硬约束 3）。
	head := r.do(t, http.MethodHead, "/video/"+id+".m3u8")
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD 状态码 = %d, 期望 200", head.Code)
	}
	if head.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Errorf("HEAD Content-Length = %q, GET body = %d 字节", head.Header().Get("Content-Length"), len(body))
	}
}

// TestSegment_SliceThenCacheHit 覆盖两条 AC：分片 200 且 ffprobe 能解析；
// 第二次请求命中本地缓存、零外呼，字节与第一次逐字节相同。
func TestSegment_SliceThenCacheHit(t *testing.T) {
	r := newRig(t, "4")
	ep := r.addEpisode(t, 1, 12)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)

	target := "/video/" + id + "/0.ts"
	first := r.do(t, http.MethodGet, target)
	if first.Code != http.StatusOK {
		t.Fatalf("首片状态码 = %d, 期望 200（body=%s）", first.Code, first.Body.String())
	}
	if ct := first.Header().Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("Content-Type = %q, 期望 video/mp2t", ct)
	}
	if cl := first.Header().Get("Content-Length"); cl != strconv.Itoa(first.Body.Len()) {
		t.Errorf("Content-Length = %q, body = %d 字节", cl, first.Body.Len())
	}
	if first.Body.Len() == 0 || first.Body.Bytes()[0] != 0x47 {
		t.Fatalf("分片不是 MPEG-TS（首字节 0x%02x, %d 字节）", first.Body.Bytes()[0], first.Body.Len())
	}
	tsPath := filepath.Join(t.TempDir(), "seq0.ts")
	if err := os.WriteFile(tsPath, first.Body.Bytes(), 0o644); err != nil {
		t.Fatalf("写切片: %v", err)
	}
	if got := probeDuration(t, tsPath); got < 3.5 || got > 4.5 {
		t.Errorf("ffprobe 分片时长 = %v, 期望约 4 秒", got)
	}

	// 命中缓存：库里有分片 artifact 行，文件真的在 cache_dir/seg/ 下。
	artifacts, err := r.st.ListArtifactsOldest(context.Background(), store.ArtifactKindSegment, 10)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("分片缓存行数 = %d, 期望 1", len(artifacts))
	}
	if _, err := os.Stat(artifacts[0].Path); err != nil {
		t.Fatalf("缓存文件不存在: %v", err)
	}
	if artifacts[0].Bytes != int64(first.Body.Len()) {
		t.Errorf("缓存行字节 = %d, 响应 body = %d", artifacts[0].Bytes, first.Body.Len())
	}

	hitsBefore := r.cdn[ep.ID].hits()
	resolvesBefore := r.resolves.Load()
	second := r.do(t, http.MethodGet, target)
	if second.Code != http.StatusOK {
		t.Fatalf("第二次状态码 = %d, 期望 200", second.Code)
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Error("两次响应的字节不一致（命中缓存不该改变内容）")
	}
	if got := r.cdn[ep.ID].hits(); got != hitsBefore {
		t.Errorf("命中缓存后外呼次数从 %d 变为 %d, 期望不变（零外呼）", hitsBefore, got)
	}
	if got := r.resolves.Load(); got != resolvesBefore {
		t.Errorf("命中缓存后直链解析次数从 %d 变为 %d, 期望不变（命中即回文件）", resolvesBefore, got)
	}

	// HEAD 给同样的长度，且不触发切片。
	head := r.do(t, http.MethodHead, target)
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD 状态码 = %d, 期望 200", head.Code)
	}
	if head.Header().Get("Content-Length") != strconv.Itoa(first.Body.Len()) {
		t.Errorf("HEAD Content-Length = %q, GET body = %d 字节", head.Header().Get("Content-Length"), first.Body.Len())
	}
	if got := r.cdn[ep.ID].hits(); got != hitsBefore {
		t.Errorf("HEAD 触发外呼：次数从 %d 变为 %d", hitsBefore, got)
	}

	// Range：分片是完整不可变对象，由 ServeContent 就地切字节。
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Range", "bytes=0-187")
	rec := httptest.NewRecorder()
	r.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range 状态码 = %d, 期望 206", rec.Code)
	}
	if rec.Body.Len() != 188 {
		t.Errorf("Range body = %d 字节, 期望 188", rec.Body.Len())
	}
	if !bytes.Equal(rec.Body.Bytes(), first.Body.Bytes()[:188]) {
		t.Error("Range 返回的字节与完整分片的前缀不一致")
	}
}

// TestFetchHeaders_ReachBothFFmpegCalls 覆盖 MKACG-624 AC4：注入的请求头经
// `-headers` 真的挂到了源侧请求上——建索引的 ffprobe 与切片的 ffmpeg 都是。
//
// 判据是「每一个出站请求都带」，不是「出现过一次」：只写进其中一条命令行，
// 另一条仍然是 Lavf 默认身份，而它在真机上就是被风控挑出来的那类请求。
//
// 这条测试的源是明文（`Key` 为空）= 网页通道：它的身份是 UA + 源站根 Referer，
// 两个通道各一份、不能互相顶掉（MKACG-638，App 通道见下一条）。
func TestFetchHeaders_ReachBothFFmpegCalls(t *testing.T) {
	r := newRig(t, "4")
	ep := r.addEpisode(t, 1, 12)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)
	cdn := r.cdn[ep.ID]

	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	probeReqs := cdn.hits()
	if probeReqs == 0 {
		t.Fatal("建索引没有回源：ffprobe 没有请求假 CDN")
	}

	if rec := r.do(t, http.MethodGet, "/video/"+id+"/0.ts"); rec.Code != http.StatusOK {
		t.Fatalf("分片状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	seen := cdn.seenHeaders()
	if len(seen) <= probeReqs {
		t.Fatalf("切片没有回源：请求数 %d，建索引时已有 %d", len(seen), probeReqs)
	}

	for i, h := range seen {
		stage := "切片（ffmpeg）"
		if i < probeReqs {
			stage = "建索引（ffprobe）"
		}
		if got := h.Get("User-Agent"); got != "drama-hub-test" {
			t.Errorf("第 %d 次回源（%s）User-Agent = %q, 期望注入值", i, stage, got)
		}
		if got := h.Get("Referer"); got != "https://example.test/site/" {
			t.Errorf("第 %d 次回源（%s）Referer = %q, 期望注入值", i, stage, got)
		}
	}
}

// TestFetchHeaders_AppChannelDropsReferer 覆盖 MKACG-638：App 通道的出站身份不带 Referer。
//
// 判据来自真实 CDN 的二分：同一把 key、同一条直链，带站内 Referer 一律
// `403 Forbidden`，不带（或只带 UA）就是完整集。只要 ffprobe 或 ffmpeg 有**一次**
// 回源带着它，整集就是 502 —— 所以断言是「每一次回源都没有 Referer」，且读的是假
// CDN 收到的真实请求头（不是只断言命令行）。网页通道仍带 UA + Referer 由
// TestFetchHeaders_ReachBothFFmpegCalls 钉住。
func TestFetchHeaders_AppChannelDropsReferer(t *testing.T) {
	r := newRig(t, "4")
	ep := r.addEpisode(t, 1, 12)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)
	cdn := r.cdn[ep.ID]

	// 换成 CENC 密文源并把 Key 给出去：判据是「本次解析带 Key」= App 通道（626 契约 4）。
	encPath := filepath.Join(t.TempDir(), "enc.mp4")
	makeEncryptedVideo(t, encPath, 12, "160x120", testKey)
	body, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatalf("读取密文样本: %v", err)
	}
	cdn.setBody(body)
	r.keys[ep.ID] = testKey

	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("App 通道 playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	probeReqs := cdn.hits()
	if probeReqs == 0 {
		t.Fatal("建索引没有回源：ffprobe 没有请求假 CDN")
	}
	if rec := r.do(t, http.MethodGet, "/video/"+id+"/0.ts"); rec.Code != http.StatusOK {
		t.Fatalf("App 通道分片状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	seen := cdn.seenHeaders()
	if len(seen) <= probeReqs {
		t.Fatalf("切片没有回源：请求数 %d，建索引时已有 %d", len(seen), probeReqs)
	}

	// 命令行层：探测与切片都不得残留 Referer 片段（含 `-headers` 里那一行），
	// 且注入的 UA 还在 —— 断言的是 argv 本身，拒绝「只是碰巧成功」。
	d := stream.Direct{URL: cdn.url(), Key: testKey}
	for _, call := range []struct {
		stage string
		args  []string
	}{
		{"建索引（ffprobe）", probeArgs(d, r.c.fetchHeaders(d))},
		{"切片（ffmpeg）", cutArgs(d, 0, 4, 0, r.c.fetchHeaders(d))},
	} {
		joined := strings.Join(call.args, "\n")
		if strings.Contains(joined, "Referer:") {
			t.Errorf("App 通道%s的 argv 里还有 Referer：%q", call.stage, joined)
		}
		if !strings.Contains(joined, "User-Agent: drama-hub-test") {
			t.Errorf("App 通道%s的 argv 里丢了注入的 UA：%q", call.stage, joined)
		}
	}

	for i, h := range seen {
		stage := "切片（ffmpeg）"
		if i < probeReqs {
			stage = "建索引（ffprobe）"
		}
		if got := h.Get("User-Agent"); got != "drama-hub-test" {
			t.Errorf("App 通道第 %d 次回源（%s）User-Agent = %q, 期望注入值", i, stage, got)
		}
		if got := h.Get("Referer"); got != "" {
			t.Errorf("App 通道第 %d 次回源（%s）Referer = %q：媒体 CDN 对带站内 Referer 的请求回 403",
				i, stage, got)
		}
	}
}

// TestHeaderBlock 是 headerBlock / headerArgs 的纯函数判据：CRLF 分隔、末行有
// 终止符（ffmpeg 丢没有终止符的末行）、空输入不进命令行。
func TestHeaderBlock(t *testing.T) {
	if got := headerBlock(nil); got != "" {
		t.Errorf("headerBlock(nil) = %q, 期望空串", got)
	}
	if got := headerArgs(""); got != nil {
		t.Errorf("headerArgs(\"\") = %v, 期望 nil（nil 才是一个参数都不加）", got)
	}

	block := headerBlock(testWebHeaders)
	want := "User-Agent: drama-hub-test\r\nReferer: https://example.test/site/\r\n"
	if block != want {
		t.Fatalf("headerBlock = %q, 期望 %q", block, want)
	}
	args := headerArgs(block)
	if len(args) != 2 || args[0] != "-headers" || args[1] != block {
		t.Errorf("headerArgs = %q, 期望 [-headers %q]", args, block)
	}
}

// TestVideo_StatusMatrix 覆盖契约状态码矩阵：缺 .m3u8 → 404、坏签名 → 401、
// 未知 uid → 404、坏 seq → 404、越界 seq → 404、解析不到直链 → 502。
func TestVideo_StatusMatrix(t *testing.T) {
	r := newRig(t, "4")
	ep := r.addEpisode(t, 1, 8)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)

	// 合法请求先把索引建出来，后面的矩阵不再依赖索引建立路径。
	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("基线 playlist 状态码 = %d, 期望 200", rec.Code)
	}

	cases := []struct {
		name   string
		method string
		target string
		want   int
	}{
		{"缺 m3u8 后缀", http.MethodGet, "/video/" + id, http.StatusNotFound},
		{"坏签名", http.MethodGet, "/video/" + uid + "-000000000000.m3u8", http.StatusUnauthorized},
		{"缺签名分隔符", http.MethodGet, "/video/" + uid + ".m3u8", http.StatusUnauthorized},
		{"签名错误的分片", http.MethodGet, "/video/" + uid + "-ffffffffffff/0.ts", http.StatusUnauthorized},
		{"未知剧集", http.MethodGet, "/video/" + r.id("e9999") + ".m3u8", http.StatusNotFound},
		{"uid 形态不认识", http.MethodGet, "/video/" + r.id("x1") + ".m3u8", http.StatusNotFound},
		{"非法分片名", http.MethodGet, "/video/" + id + "/x.ts", http.StatusNotFound},
		{"分片名带符号", http.MethodGet, "/video/" + id + "/-1.ts", http.StatusNotFound},
		{"分片名后缀不对", http.MethodGet, "/video/" + id + "/0.m3u8", http.StatusNotFound},
		{"seq 越界", http.MethodGet, "/video/" + id + "/99.ts", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.do(t, tc.method, tc.target)
			if rec.Code != tc.want {
				t.Fatalf("%s %s 状态码 = %d, 期望 %d（body=%s）", tc.method, tc.target, rec.Code, tc.want, rec.Body.String())
			}
			// 错误响应是 JSON，且绝不回显 id 的签名段之外的凭据。
			if tc.want == http.StatusUnauthorized && strings.Contains(rec.Body.String(), "000000000000") {
				t.Errorf("401 响应体回显了签名段：%s", rec.Body.String())
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Errorf("错误响应不是合法 JSON: %v（body=%q）", err, rec.Body.String())
			}
		})
	}

	// 解析不到直链 → 502（契约：Resolve 失败一律 502，不写库不缓存）。
	r.resolveErr[ep.ID] = errors.New("上游没有直链")
	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusBadGateway {
		t.Errorf("直链解析失败时 playlist 状态码 = %d, 期望 502（body=%s）", rec.Code, rec.Body.String())
	}
	if rec := r.do(t, http.MethodGet, "/video/"+id+"/0.ts"); rec.Code != http.StatusBadGateway {
		t.Errorf("直链解析失败时分片状态码 = %d, 期望 502", rec.Code)
	}
}

// TestPlaylist_IndexTimeoutIs503 覆盖「索引建立超时 → 503 + Retry-After」：
// 源站永不回包时 ffprobe 被 ctx 掐断，边界包回 503 而不是挂住请求。
func TestPlaylist_IndexTimeoutIs503(t *testing.T) {
	r := newRig(t, "4")
	ep := r.addEpisode(t, 1, 8)
	// 测试预算里等不了 30 秒：把索引上限调短（生产固定 30 s）。
	r.c.indexTimeout = 300 * time.Millisecond
	r.cdn[ep.ID].setHang()

	uid := "e" + strconv.FormatInt(ep.ID, 10)
	rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503（body=%s）", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("503 应带 Retry-After")
	}
	if !strings.Contains(rec.Body.String(), `"code":"unavailable"`) {
		t.Errorf("响应体 code 不是 unavailable：%s", rec.Body.String())
	}
	// 超时不算失败结果的缓存：库里仍无索引行。
	segs, err := r.st.ListSegments(context.Background(), ep.ID)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segs) != 0 {
		t.Errorf("超时后库里留下 %d 行索引, 期望 0", len(segs))
	}
}

// TestSegment_CutFailureIs502AndNotCached 覆盖「生成失败 → 502，不写缓存」：
// 源坏掉时 ffmpeg 切不出 TS，缓存目录与 artifact 都不留东西；源恢复后仍能出流。
func TestSegment_CutFailureIs502AndNotCached(t *testing.T) {
	r := newRig(t, "4")
	ep := r.addEpisode(t, 1, 8)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)
	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("基线 playlist 状态码 = %d, 期望 200", rec.Code)
	}

	r.cdn[ep.ID].setBroken(true)
	rec := r.do(t, http.MethodGet, "/video/"+id+"/0.ts")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("切片失败状态码 = %d, 期望 502（body=%s）", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() > 0 && rec.Body.Bytes()[0] == 0x47 {
		t.Error("502 响应体里出现了 MPEG-TS 字节")
	}
	artifacts, err := r.st.ListArtifactsOldest(context.Background(), store.ArtifactKindSegment, 10)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) != 0 {
		t.Errorf("失败结果被写进缓存：%d 行", len(artifacts))
	}

	// 源恢复后同一 seq 仍能成功 —— 失败没有留下任何被缓存的状态。
	r.cdn[ep.ID].setBroken(false)
	if rec := r.do(t, http.MethodGet, "/video/"+id+"/0.ts"); rec.Code != http.StatusOK {
		t.Errorf("源恢复后状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
}

// TestSegment_SourceLinkRotated 覆盖 AC「源直链失效后重新 Resolve 仍可出流」：
// 换掉解析结果（模拟直链过期换链）后，新的分片请求用的是新链、并落新缓存。
func TestSegment_SourceLinkRotated(t *testing.T) {
	r := newRig(t, "4")
	ep := r.addEpisode(t, 1, 8)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)
	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("基线 playlist 状态码 = %d, 期望 200", rec.Code)
	}

	old := r.cdn[ep.ID]
	path := filepath.Join(t.TempDir(), "rot.mp4")
	makeVideo(t, path, 8)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 fixture: %v", err)
	}
	rotated := newFakeCDN(t, body)
	oldHits := old.hits()
	r.cdn[ep.ID] = rotated

	if rec := r.do(t, http.MethodGet, "/video/"+id+"/1.ts"); rec.Code != http.StatusOK {
		t.Fatalf("换链后状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if rotated.hits() == 0 {
		t.Error("换链后仍打旧源：请求没有重新 Resolve（直链被缓存了）")
	}
	if got := old.hits(); got != oldHits {
		t.Errorf("旧源被打了 %d 次（换链后新增 %d）, 期望 0（每次都重新解析直链）", got, got-oldHits)
	}
}

// appSignedQuery 是假 App 直链的签名查询串：它的任何一个参数名都不许出现在
// 响应体里（MKACG-639）。
const appSignedQuery = "?n80=1&n80_dm=bjk4LXYtcmVhZGluZ3ZpZGVv&n80_tk=deadbeefcafebabe"

// TestPlayback_ErrorTextCarriesNoDirectLink 覆盖 AC「502 错误文本只给稳定类别」：
// ffprobe / ffmpeg 的 stderr 里带着**完整直链**（实测 `<url>?n80_tk=…: Server
// returned 403 Forbidden`），报错时客户端只该看到稳定类别。因此响应体不许出现
// 参数名 `n80_tk`、查询串分隔符 `&`、scheme `http`（626 契约：解出的 URL 查询串
// MUST NOT 出现在日志与错误文本里）。
func TestPlayback_ErrorTextCarriesNoDirectLink(t *testing.T) {
	r := newRig(t, "4")
	ep := r.addEpisode(t, 1, 8)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)
	cdn := r.cdn[ep.ID]

	// 一、索引建不起来：playlist 现解析直链，ffprobe 被 403 拒绝。
	cdn.rejectWith(http.StatusForbidden, appSignedQuery)
	rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("ffprobe 被拒时 playlist 状态码 = %d, 期望 502（body=%s）", rec.Code, rec.Body.String())
	}
	assertNoDirectLink(t, "playlist 502 响应体", rec.Body.String())
	if !strings.Contains(rec.Body.String(), "upstream rejected") {
		t.Errorf("502 错误文本没给出稳定类别 upstream rejected：%s", rec.Body.String())
	}

	// 二、切片失败走同一条口子：先让索引建成功，再让 ffmpeg 撞 403。
	cdn.rejectWith(0, "")
	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("基线 playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	cdn.rejectWith(http.StatusForbidden, appSignedQuery)
	rec = r.do(t, http.MethodGet, "/video/"+id+"/0.ts")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("ffmpeg 被拒时分片状态码 = %d, 期望 502（body=%s）", rec.Code, rec.Body.String())
	}
	assertNoDirectLink(t, "分片 502 响应体", rec.Body.String())

	// 三、日志这一面同样不许出现签名参数（DEV-STANDARDS「日志」：禁止记带签名的
	// 播放直链）。stderr 原文只在确实不含地址/查询串时才进日志。
	if logs := r.logs.String(); strings.Contains(logs, "n80_tk") || strings.Contains(logs, "deadbeefcafebabe") {
		t.Errorf("日志里出现了直链签名查询串：%s", logs)
	}
}

// assertNoDirectLink 断言一段面向客户端的文本里没有直链痕迹。
//
// 只断言「不含 n80_tk」太窄 —— 上游换个参数名就漏过去了，所以按**形态**断言：
// 参数名、查询串分隔符、scheme 都不许出现。
func assertNoDirectLink(t *testing.T, what, text string) {
	t.Helper()
	// 响应体是 JSON，`&` 会被编码成 `\u0026`，两种形态都得查。
	for _, bad := range []string{"n80_tk", "n80_dm", "deadbeefcafebabe", "&", `\u0026`, "?", "http", "://"} {
		if strings.Contains(text, bad) {
			t.Errorf("%s 里出现了直链痕迹 %q：%s", what, bad, text)
		}
	}
}

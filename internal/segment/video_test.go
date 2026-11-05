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

// discardLogger 是直接调 probeDuration / remux 的测试用的丢弃 logger
// （生产由 Cutter 注入）。
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
func makeVideo(t *testing.T, path string, seconds int) {
	t.Helper()
	makeVideoSize(t, path, seconds, "160x120")
}

// makeVideoSize 同 makeVideo，但指定分辨率：通道测试要两个通道的画面不同，
// 而分片键只含「集 + 通道」——正是「同一集两条通道、字节不同」那种串味的前提。
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

// fileDuration 用 ffprobe 量一个本地文件/字节流的容器时长（测试侧独立测量）。
func fileDuration(t *testing.T, path string) float64 {
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

// topLevelBoxes 返回 MP4 顶层 box 的类型序列（只解析 size+type）。
func topLevelBoxes(t *testing.T, body []byte) []string {
	t.Helper()
	var boxes []string
	for i := 0; i+8 <= len(body); {
		size := int(body[i])<<24 | int(body[i+1])<<16 | int(body[i+2])<<8 | int(body[i+3])
		typ := string(body[i+4 : i+8])
		boxes = append(boxes, typ)
		if size <= 0 {
			break
		}
		i += size
	}
	return boxes
}

// assertSelfContainedSegment 钉住「片段自包含」：片头是 ftyp+moov（自带 init），
// 真 ffprobe 能单独解析出视频流与完整时长，真 ffmpeg 能无错解码。
//
// 这是 AC「逐片 ffprobe 可解析」「不是 10 秒预览」的测试侧证据：fixture 是 h264，
// 线上是 hevc，两者的差别只在编码参数，容器结构断言与编码无关。
//
// wantStart 是该片在播放单元里的起点（出流时把 tfdt 平移了这么多，ADR-0011「修正：
// 集边界时间戳」）。fMP4 没有 moov 时长（empty_moov），ffprobe 的 format.duration 是
// 「末时刻」，所以时长要用 `duration - start_time` 比，而不是直接比 duration。
func assertSelfContainedSegment(t *testing.T, body []byte, wantStart, wantDuration float64) {
	t.Helper()
	boxes := topLevelBoxes(t, body)
	if len(boxes) < 3 || boxes[0] != "ftyp" || boxes[1] != "moov" {
		t.Fatalf("片段顶层 box = %v, 期望以 ftyp,moov 开头（自包含 init）", boxes)
	}
	hasMoof := false
	for _, b := range boxes {
		if b == "moof" {
			hasMoof = true
		}
	}
	if !hasMoof {
		t.Fatalf("片段顶层 box 里没有 moof（不是 fragmented MP4）：%v", boxes)
	}

	path := filepath.Join(t.TempDir(), "seg.m4s")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("写片段: %v", err)
	}
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "stream=codec_type,codec_name",
		"-show_entries", "format=start_time,duration,format_name", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe 片段失败: %v", err)
	}
	var info struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
		Format struct {
			StartTime string `json:"start_time"`
			Duration  string `json:"duration"`
			Name      string `json:"format_name"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("解析 ffprobe 输出: %v（%s）", err, out)
	}
	hasVideo := false
	for _, s := range info.Streams {
		if s.CodecType == "video" {
			hasVideo = true
		}
	}
	if !hasVideo {
		t.Fatalf("片段里没有视频流：%s", out)
	}
	end, err := strconv.ParseFloat(info.Format.Duration, 64)
	if err != nil {
		t.Fatalf("解析片段时长 %q: %v", info.Format.Duration, err)
	}
	start, err := strconv.ParseFloat(info.Format.StartTime, 64)
	if err != nil {
		t.Fatalf("解析片段起点 %q: %v", info.Format.StartTime, err)
	}
	if diff := start - wantStart; diff > 1 || diff < -1 {
		t.Errorf("片段首时刻 = %.3fs, 期望该片起点 %.3fs（tfdt 没按播放位置平移？）", start, wantStart)
	}
	if span := end - start; span-wantDuration > 1 || span-wantDuration < -1 {
		t.Errorf("片段时长 %.3fs（%.3f → %.3f）与该集真实时长 %.3fs 差超过 1s", span, start, end, wantDuration)
	}

	if msg, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("解码片段失败: %v（%s）", err, msg)
	} else if len(bytes.TrimSpace(msg)) > 0 {
		t.Errorf("解码片段有错误输出：%s", msg)
	}
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
	// broken 为 true 时返回一段 HTML，让 ffmpeg / ffprobe 切不出片。
	broken bool
	// hanging 为 true 时永不回包（算探测超时那条路径）。
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

// hits 返回被请求次数（含 Range 续传，一次 ffmpeg 可能多次）。
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

// releaseHang 让之后的新请求恢复正常；已经挂起的请求仍等自己的 ctx 结束。
func (f *fakeCDN) releaseHang() {
	f.mu.Lock()
	f.hanging = false
	f.mu.Unlock()
}

// heal 模拟「上游换了一条能用的直链」：不再拒绝，并给 url() 换一个查询串。
// rig 的 invalidate 钩子用它，让「作废 + 重解析一次」拿到与旧链不同的新链。
func (f *fakeCDN) heal() {
	f.mu.Lock()
	f.reject = 0
	f.query = "?gen=2"
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
	db       *store.DB
	st       *store.Store
	settings *store.Settings
	logs     *bytes.Buffer

	dramaID int64
	// cacheRoot 是本次测试的 <cache_dir>（断言落盘文件数用）。
	cacheRoot string
	cdn       map[int64]*fakeCDN // episode id → 假源
	// keys 是每集当前的内容密钥：非空即模拟「这条走的是 App 通道的密文源」。
	keys map[int64][]byte
	// resolveErr 非 nil 时直链解析按它失败（模拟 ErrNoStream 等）。
	resolveErr map[int64]error
	// resolves 数「直链解析次数」：命中缓存的分片请求不该增加它（零外呼的直接证据）。
	resolves atomic.Int64
	// invalidates 数「作废直链」次数：CDN 拒绝后的自愈路径必须只动这一集。
	invalidates atomic.Int64
	// invalidate 是注入 Cutter 的作废入口（生产是 stream.Resolver.Invalidate）。
	invalidate func(ctx context.Context, episodeID int64) error
	// healOnInvalidate 打开的集，在作废时让假源站「换一条能用的链」：验证自愈路径
	// 只重解析一次就能播。默认关闭，403 类用例仍拿到 502。
	healOnInvalidate map[int64]bool
}

// newRig 建临时库并写一部剧。
func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	// 缓存目录必须在临时目录里：默认的 ./data/cache 会写进源码树（容器内只读）。
	root := filepath.Join(t.TempDir(), "cache")
	for k, v := range map[store.SettingKey]string{
		store.KeyStreamSecret: testSecret,
		store.KeyCacheDir:     root,
		// 分块大小 1：本rig 的每个 addEpisode 都是一条独立的播放单元（e<id>）。
		// 合并集的用例自己把这一项调成 10（见 mergedRig）。
		store.KeyMergeChunkSize: "1",
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
		db: db, st: st, settings: settings, logs: &bytes.Buffer{},
		dramaID: dramaID, cacheRoot: root, cdn: map[int64]*fakeCDN{}, keys: map[int64][]byte{},
		resolveErr: map[int64]error{}, healOnInvalidate: map[int64]bool{},
	}
	logger := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	resolveOne := func(_ context.Context, ep store.Episode) (stream.Direct, error) {
		r.resolves.Add(1)
		if err := r.resolveErr[ep.ID]; err != nil {
			return stream.Direct{}, err
		}
		f, ok := r.cdn[ep.ID]
		if !ok {
			return stream.Direct{}, fmt.Errorf("测试没有给剧集 %d 配源", ep.ID)
		}
		return stream.Direct{URL: f.url(), Key: r.keys[ep.ID]}, nil
	}
	// 批量入口沿用单集语义逐条解析：rig 的假源没有「一次页面解析多集」的能力，
	// 但生产入口（stream.Resolver.DirectMany）是一样的「一次调用解析一块成员」。
	// invalidate 模拟生产里的 stream.Resolver.Invalidate：只作废这一集，并让假源站
	// 「换一条能用的链」，好验证重解析后重试一次就成功。
	r.invalidate = func(_ context.Context, id int64) error {
		r.invalidates.Add(1)
		if r.healOnInvalidate[id] {
			if f, ok := r.cdn[id]; ok {
				f.heal()
			}
		}
		return nil
	}
	r.c = NewCutter(st, settings, logger, cache.New(st, settings, logger), resolveOne,
		func(ctx context.Context, eps []store.Episode) ([]stream.Direct, error) {
			out := make([]stream.Direct, len(eps))
			for i, ep := range eps {
				d, err := resolveOne(ctx, ep)
				if err != nil {
					return nil, fmt.Errorf("剧集 %d: %w", ep.ID, err)
				}
				out[i] = d
			}
			return out, nil
		}, r.invalidate, Headers{Web: testWebHeaders, App: testAppHeaders}, nil)
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
	// 不再有派生表要重建：播放单元由 PlaybackEpisodes 按当前 merge_chunk_size 现算
	// （docs/adr/0014），建好 episode 就是可播状态。
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

// TestPlaylist_ProbesDurationAndWritesChannel 覆盖「首播补齐时长 + 通道行」：
// 第一次请求 playlist 时 ffprobe 一次把 episode.duration_sec 回填，并写下 segment 行；
// 第二次请求不再探测（时长已非 0），也不再改行。
func TestPlaylist_ProbesDurationAndWritesChannel(t *testing.T) {
	r := newRig(t)
	ep := r.addEpisode(t, 1, 12)
	uid := "e" + strconv.FormatInt(ep.ID, 10)

	// 请求前：时长为 0（插件恒给 0），没有 segment 行。
	before, err := r.st.EpisodeByID(context.Background(), ep.ID)
	if err != nil {
		t.Fatalf("EpisodeByID: %v", err)
	}
	if before.DurationSec != 0 {
		t.Fatalf("请求前 duration_sec = %d, 期望 0", before.DurationSec)
	}
	if _, err := r.st.GetSegment(context.Background(), ep.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("请求前 segment 行错误 = %v, 期望 ErrNotFound", err)
	}

	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("首次 playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}

	after, err := r.st.EpisodeByID(context.Background(), ep.ID)
	if err != nil {
		t.Fatalf("EpisodeByID: %v", err)
	}
	if after.DurationSec != 12 {
		t.Errorf("回填 duration_sec = %d, 期望 12（ffprobe 实测）", after.DurationSec)
	}
	seg, err := r.st.GetSegment(context.Background(), ep.ID)
	if err != nil {
		t.Fatalf("GetSegment: %v", err)
	}
	if want := (store.Segment{EpisodeID: ep.ID, Channel: "web"}); seg != want {
		t.Errorf("segment 行 = %+v, 期望 %+v", seg, want)
	}

	// 第二次播放：时长已非 0，不再探测（零外呼）。
	hitsBefore := r.cdn[ep.ID].hits()
	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("第二次 playlist 状态码 = %d, 期望 200", rec.Code)
	}
	if got := r.cdn[ep.ID].hits(); got != hitsBefore {
		t.Errorf("第二次 playlist 外呼次数 %d → %d, 期望不变（时长已回填）", hitsBefore, got)
	}
}

// TestPlaylist_SingleEpisode 覆盖契约的 playlist 形态：单集与合并集同一路径，
// 合法 VOD playlist（EXTM3U / VERSION:7 / PLAYLIST-TYPE / TARGETDURATION / ENDLIST），
// 单集恰 1 片、URI 指回 /video/{id}/0.m4s、没有 DISCONTINUITY。
func TestPlaylist_SingleEpisode(t *testing.T) {
	r := newRig(t)
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
		"#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-PLAYLIST-TYPE:VOD\n",
		"#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-TARGETDURATION:12\n",
		"#EXTINF:12.000,\n",
		"#EXT-X-ENDLIST\n",
		"/video/" + id + "/0.m4s\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("playlist 缺 %q：\n%s", want, body)
		}
	}
	if got := strings.Count(body, "#EXTINF:"); got != 1 {
		t.Errorf("EXTINF 行数 = %d, 期望 1（每集一片）", got)
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

// TestSegment_RemuxThenCacheHit 覆盖三条 AC：分片是**整集**的自包含 fMP4（ffprobe 拿到
// 完整时长与视频流，不是 10 秒预览）；第二次请求命中本地缓存、零外呼且字节相同；
// 每集只落一个片段文件。
func TestSegment_RemuxThenCacheHit(t *testing.T) {
	r := newRig(t)
	ep := r.addEpisode(t, 1, 12)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)

	target := "/video/" + id + "/0.m4s"
	first := r.do(t, http.MethodGet, target)
	if first.Code != http.StatusOK {
		t.Fatalf("分片状态码 = %d, 期望 200（body=%s）", first.Code, first.Body.String())
	}
	if ct := first.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q, 期望 video/mp4", ct)
	}
	if cl := first.Header().Get("Content-Length"); cl != strconv.Itoa(first.Body.Len()) {
		t.Errorf("Content-Length = %q, body = %d 字节", cl, first.Body.Len())
	}
	t.Logf("单片 fMP4：%d 字节，顶层 box = %v", first.Body.Len(), topLevelBoxes(t, first.Body.Bytes()))
	assertSelfContainedSegment(t, first.Body.Bytes(), 0, 12)

	// 命中缓存：库里有且只有一条分片 artifact 行，文件真的在 cache_dir/seg/ 下。
	artifacts, err := r.st.ListArtifactsOldest(context.Background(), store.ArtifactKindSegment, 10)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("分片缓存行数 = %d, 期望 1（每集一片）", len(artifacts))
	}
	if _, err := os.Stat(artifacts[0].Path); err != nil {
		t.Fatalf("缓存文件不存在: %v", err)
	}
	if !strings.HasSuffix(artifacts[0].Path, ".m4s") {
		t.Errorf("缓存文件后缀 = %q, 期望 .m4s", artifacts[0].Path)
	}
	if artifacts[0].Bytes != int64(first.Body.Len()) {
		t.Errorf("缓存行字节 = %d, 响应 body = %d", artifacts[0].Bytes, first.Body.Len())
	}

	// AC「同一集第二次播放 ffmpeg 调用 0 次」：日志里只有一条生成记录，外呼不变。
	generated := strings.Count(r.logs.String(), `"msg":"segment generated"`)
	if generated != 1 {
		t.Fatalf("生成日志条数 = %d, 期望 1", generated)
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
	if got := strings.Count(r.logs.String(), `"msg":"segment generated"`); got != 1 {
		t.Errorf("第二次播放又生成了片段（生成日志 %d 条）", got)
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
// `-headers` 真的挂到了源侧请求上——探测时长的 ffprobe 与整集 remux 的 ffmpeg 都是。
//
// 判据是「每一个出站请求都带」，不是「出现过一次」：只写进其中一条命令行，
// 另一条仍然是 Lavf 默认身份，而它在真机上就是被风控挑出来的那类请求。
//
// 这条测试的源是明文（`Key` 为空）= 网页通道：它的身份是 UA + 源站根 Referer，
// 两个通道各一份、不能互相顶掉（MKACG-638，App 通道见下一条）。
func TestFetchHeaders_ReachBothFFmpegCalls(t *testing.T) {
	r := newRig(t)
	ep := r.addEpisode(t, 1, 12)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)
	cdn := r.cdn[ep.ID]

	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	probeReqs := cdn.hits()
	if probeReqs == 0 {
		t.Fatal("探测时长没有回源：ffprobe 没有请求假 CDN")
	}

	if rec := r.do(t, http.MethodGet, "/video/"+id+"/0.m4s"); rec.Code != http.StatusOK {
		t.Fatalf("分片状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	seen := cdn.seenHeaders()
	if len(seen) <= probeReqs {
		t.Fatalf("remux 没有回源：请求数 %d，探测时长时已有 %d", len(seen), probeReqs)
	}

	for i, h := range seen {
		stage := "remux（ffmpeg）"
		if i < probeReqs {
			stage = "探测时长（ffprobe）"
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
	r := newRig(t)
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
		t.Fatal("探测时长没有回源：ffprobe 没有请求假 CDN")
	}
	if rec := r.do(t, http.MethodGet, "/video/"+id+"/0.m4s"); rec.Code != http.StatusOK {
		t.Fatalf("App 通道分片状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	seen := cdn.seenHeaders()
	if len(seen) <= probeReqs {
		t.Fatalf("remux 没有回源：请求数 %d，探测时长时已有 %d", len(seen), probeReqs)
	}

	// 命令行层：探测与 remux 都不得残留 Referer 片段（含 `-headers` 里那一行），
	// 且注入的 UA 还在 —— 断言的是 argv 本身，拒绝「只是碰巧成功」。
	d := stream.Direct{URL: cdn.url(), Key: testKey}
	for _, call := range []struct {
		stage string
		args  []string
	}{
		{"探测时长（ffprobe）", probeArgs(d, r.c.fetchHeaders(d))},
		{"remux（ffmpeg）", remuxArgs(d, r.c.fetchHeaders(d))},
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
		stage := "remux（ffmpeg）"
		if i < probeReqs {
			stage = "探测时长（ffprobe）"
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
	r := newRig(t)
	ep := r.addEpisode(t, 1, 8)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)

	// 合法请求先把时长与通道行建出来，后面的矩阵不再依赖探测路径。
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
		{"签名错误的分片", http.MethodGet, "/video/" + uid + "-ffffffffffff/0.m4s", http.StatusUnauthorized},
		{"未知剧集", http.MethodGet, "/video/" + r.id("e9999") + ".m3u8", http.StatusNotFound},
		{"uid 形态不认识", http.MethodGet, "/video/" + r.id("x1") + ".m3u8", http.StatusNotFound},
		{"非法分片名", http.MethodGet, "/video/" + id + "/x.m4s", http.StatusNotFound},
		{"分片名带符号", http.MethodGet, "/video/" + id + "/-1.m4s", http.StatusNotFound},
		{"分片名后缀不对", http.MethodGet, "/video/" + id + "/0.ts", http.StatusNotFound},
		{"seq 越界", http.MethodGet, "/video/" + id + "/99.m4s", http.StatusNotFound},
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
	if rec := r.do(t, http.MethodGet, "/video/"+id+"/0.m4s"); rec.Code != http.StatusBadGateway {
		t.Errorf("直链解析失败时分片状态码 = %d, 期望 502", rec.Code)
	}
}

// TestPlaylist_ProbeTimeoutIs503 覆盖「时长探测超时 → 503 + Retry-After」：
// 源站永不回包时 ffprobe 被 ctx 掐断，边界包回 503 而不是挂住请求。
func TestPlaylist_ProbeTimeoutIs503(t *testing.T) {
	r := newRig(t)
	ep := r.addEpisode(t, 1, 8)
	// 测试预算里等不了 30 秒：把上限调短（生产固定 30 s）。
	r.c.ensureTimeout = 300 * time.Millisecond
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
	// 超时不算失败结果的缓存：时长仍是 0、也没有 segment 行。
	after, err := r.st.EpisodeByID(context.Background(), ep.ID)
	if err != nil {
		t.Fatalf("EpisodeByID: %v", err)
	}
	if after.DurationSec != 0 {
		t.Errorf("超时后 duration_sec = %d, 期望 0", after.DurationSec)
	}
	if _, err := r.st.GetSegment(context.Background(), ep.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("超时后留下 segment 行（err=%v）", err)
	}
}

// TestSegment_RemuxFailureIs502AndNotCached 覆盖「生成失败 → 502，不写缓存」：
// 源坏掉时 ffmpeg 转不出片，缓存目录与 artifact 都不留东西；源恢复后仍能出流。
func TestSegment_RemuxFailureIs502AndNotCached(t *testing.T) {
	r := newRig(t)
	ep := r.addEpisode(t, 1, 8)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)
	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("基线 playlist 状态码 = %d, 期望 200", rec.Code)
	}

	r.cdn[ep.ID].setBroken(true)
	rec := r.do(t, http.MethodGet, "/video/"+id+"/0.m4s")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("remux 失败状态码 = %d, 期望 502（body=%s）", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ftyp") {
		t.Error("502 响应体里出现了 MP4 字节")
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
	if rec := r.do(t, http.MethodGet, "/video/"+id+"/0.m4s"); rec.Code != http.StatusOK {
		t.Errorf("源恢复后状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
}

// TestSegment_CDNRejectsThenHeals 覆盖 ADR-0016 的失败自愈：CDN 回 403 时只作废
// 这一集的直链、重解析一次、重试一次；重试成功后落缓存，下一次请求零外呼。
func TestSegment_CDNRejectsThenHeals(t *testing.T) {
	r := newRig(t)
	ep := r.addEpisode(t, 1, 8)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)
	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("基线 playlist 状态码 = %d, 期望 200", rec.Code)
	}

	cdn := r.cdn[ep.ID]
	cdn.rejectWith(http.StatusForbidden, appSignedQuery)
	r.healOnInvalidate[ep.ID] = true
	resolvesBefore := r.resolves.Load()

	rec := r.do(t, http.MethodGet, "/video/"+id+"/0.m4s")
	if rec.Code != http.StatusOK {
		t.Fatalf("403 自愈后状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if got := r.invalidates.Load(); got != 1 {
		t.Errorf("作废次数 = %d, 期望 1（只作废这一集，重试一次）", got)
	}
	if got := r.resolves.Load() - resolvesBefore; got != 2 {
		t.Errorf("解析次数增量 = %d, 期望 2（首次 + 作废后重解析一次）", got)
	}
	// 自愈后的新链被缓存：再请求一次分片零解析、零作废。
	resolvesBefore = r.resolves.Load()
	if rec := r.do(t, http.MethodGet, "/video/"+id+"/0.m4s"); rec.Code != http.StatusOK {
		t.Fatalf("第二次分片状态码 = %d, 期望 200", rec.Code)
	}
	if got := r.resolves.Load(); got != resolvesBefore {
		t.Errorf("命中缓存的分片又解析了 %d 次直链, 期望 0", got-resolvesBefore)
	}
	if got := r.invalidates.Load(); got != 1 {
		t.Errorf("作废次数 = %d, 期望仍是 1", got)
	}
	// 自愈路径的日志只带集标识，不带直链与签名查询串。
	logs := r.logs.String()
	if strings.Contains(logs, "n80_tk") || strings.Contains(logs, "deadbeefcafebabe") {
		t.Errorf("日志里出现了直链签名查询串：%s", logs)
	}
	t.Logf("自愈路径实际日志：%s", logs)
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
	r := newRig(t)
	ep := r.addEpisode(t, 1, 8)
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	id := r.id(uid)
	cdn := r.cdn[ep.ID]

	// 一、时长探测失败：playlist 现解析直链，ffprobe 被 403 拒绝。
	cdn.rejectWith(http.StatusForbidden, appSignedQuery)
	rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("ffprobe 被拒时 playlist 状态码 = %d, 期望 502（body=%s）", rec.Code, rec.Body.String())
	}
	assertNoDirectLink(t, "playlist 502 响应体", rec.Body.String())
	if !strings.Contains(rec.Body.String(), "upstream rejected") {
		t.Errorf("502 错误文本没给出稳定类别 upstream rejected：%s", rec.Body.String())
	}

	// 二、remux 失败走同一条口子：先让探测成功，再让 ffmpeg 撞 403。
	cdn.rejectWith(0, "")
	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("基线 playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	cdn.rejectWith(http.StatusForbidden, appSignedQuery)
	rec = r.do(t, http.MethodGet, "/video/"+id+"/0.m4s")
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

// TestCacheFilesEqualEpisodes 覆盖 AC「连播 3 集后 cache 目录里是 3 个片段文件」：
// 片段只由「集 + 通道」决定，所以 3 集就是 3 个文件（不是每集 6 片 = 18 个）。
func TestCacheFilesEqualEpisodes(t *testing.T) {
	r := newRig(t)
	ids := make([]int64, 0, 3)
	for i := 1; i <= 3; i++ {
		ids = append(ids, r.addEpisode(t, i, 4).ID)
	}

	for _, id := range ids {
		uid := "e" + strconv.FormatInt(id, 10)
		if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusOK {
			t.Fatalf("第 %d 集 playlist 状态码 = %d, 期望 200（body=%s）", id, rec.Code, rec.Body.String())
		}
		if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/0.m4s"); rec.Code != http.StatusOK {
			t.Fatalf("第 %d 集分片状态码 = %d, 期望 200（body=%s）", id, rec.Code, rec.Body.String())
		}
	}

	// 真数磁盘上的文件，不看 artifact 行（行与文件都在才算真的落了一个片段）。
	var files []string
	err := filepath.WalkDir(filepath.Join(r.cacheRoot, "seg"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".m4s") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历缓存目录: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("seg/ 下的片段文件数 = %d, 期望 3（连播 3 集）：%v", len(files), files)
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

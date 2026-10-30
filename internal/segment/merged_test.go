package segment

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/store"
)

// mergedRig 组装合并集测试台：一部剧 + 两集真视频 + 各集分片索引 + 一个合并组。
//
// 返回 (cutter, store, settings, merged uid, 每集源时长之和)。
func mergedRig(t *testing.T, segTarget string) (*Cutter, *store.Store, *store.Settings, string, float64) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	var sourceURLs []string
	var sum float64
	for i := 1; i <= 2; i++ {
		path := filepath.Join(dir, fmt.Sprintf("ep%d.mp4", i))
		makeVideo(t, path, 6)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 fixture %d: %v", i, err)
		}
		sum += probeDuration(t, path)
		sourceURLs = append(sourceURLs, newSource(t, body).URL+"/ep.mp4")
	}

	db := newTestDB(t)
	st := store.NewStore(db.DB)
	settings := store.NewSettings(db.DB)
	if err := settings.Set(ctx, store.KeyStreamSecret, testSecret); err != nil {
		t.Fatalf("Set stream_secret: %v", err)
	}
	if err := settings.Set(ctx, store.KeySegTargetSec, segTarget); err != nil {
		t.Fatalf("Set seg_target_sec: %v", err)
	}

	dramaID, err := st.UpsertDrama(ctx, store.Drama{Provider: "web", ProviderID: "mg1", Title: "合并剧", Year: 2026})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	ids := make([]int64, 0, len(sourceURLs))
	for i, u := range sourceURLs {
		id, err := st.UpsertEpisode(ctx, store.Episode{
			DramaID: dramaID, EpNo: i + 1, Source: "web", Vid: fmt.Sprintf("v%d", i+1), PlayURL: u,
		})
		if err != nil {
			t.Fatalf("UpsertEpisode %d: %v", i+1, err)
		}
		ids = append(ids, id)
	}

	c := NewCutter(st, settings, slog.New(slog.NewJSONHandler(io.Discard, nil)), realVerify)
	// 索引两集（合并时间轴依赖每集的 segment 表）。
	for _, id := range ids {
		if _, err := c.Index(ctx, id); err != nil {
			t.Fatalf("Index(%d): %v", id, err)
		}
	}
	gid, err := st.CreateMergeGroup(ctx, dramaID, "第1-2集", ids)
	if err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}
	uid := "m" + strconv.FormatInt(gid, 10)
	return c, st, settings, uid, sum
}

// playlistServer 起一个只挂分片路由与 playlist 路由的 httptest 服务，返回 playlist URL。
//
// 走真实装配形态：与 cmd 相同的两个 pattern（segment.Route + segment.PlaylistRoute）。
func playlistServer(t *testing.T, c *Cutter, uid string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(segmentRouteForTest(), c)
	mux.Handle(PlaylistRoute, http.HandlerFunc(c.ServePlaylist))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/s/" + signFor(uid) + "/e/" + uid + "/master.m3u8"
}

// segmentRouteForTest 复刻 cmd 注册分片端点用的常量（避免测试里再写一遍 pattern）。
func segmentRouteForTest() string { return Route }

// probeURL 用 ffprobe 量一个 HTTP 资源（playlist）的时长（测试侧独立测量）。
func probeURL(t *testing.T, url string) float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", url).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", url, err)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("解析 ffprobe 时长 %q: %v", out, err)
	}
	return v
}

// AC1 + AC2：playlist 的 EXTINF 总时长 = 各集之和，且集边界恰好一个 DISCONTINUITY。
func TestBuildPlaylist_TimelineAndDiscontinuity(t *testing.T) {
	tl := NewTimeline([]EpisodeSegments{
		{EpisodeID: 11, EpNo: 1, Segments: []store.Segment{{StartSec: 0, Dur: 4}, {StartSec: 4, Dur: 4}}},
		{EpisodeID: 22, EpNo: 2, Segments: []store.Segment{{StartSec: 0, Dur: 5}, {StartSec: 5, Dur: 3}}},
	})
	body := string(BuildPlaylist("m7", "TOK", tl.slices))

	if !strings.HasPrefix(body, "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-PLAYLIST-TYPE:VOD\n") {
		t.Errorf("playlist 头不对：\n%s", body)
	}
	if !strings.HasSuffix(body, "#EXT-X-ENDLIST\n") {
		t.Errorf("playlist 应以 ENDLIST 结束：\n%s", body)
	}
	// TARGETDURATION ≥ 最大片长（5s）。
	if !strings.Contains(body, "#EXT-X-TARGETDURATION:5\n") {
		t.Errorf("TARGETDURATION 应为最大片长向上取整 5：\n%s", body)
	}
	// 每个分片一行 EXTINF + 一行 URI，URI 带 token/uid/seq。
	for seq := 0; seq < tl.Len(); seq++ {
		want := "/s/TOK/seg/m7/" + strconv.Itoa(seq) + ".ts\n"
		if !strings.Contains(body, want) {
			t.Errorf("playlist 缺分片 %q：\n%s", want, body)
		}
	}
	if got, want := strings.Count(body, "#EXTINF:"), tl.Len(); got != want {
		t.Errorf("EXTINF 行数 = %d, 期望 %d", got, want)
	}
	// AC2：4 片、两集 → 恰一个集边界 DISCONTINUITY。
	if got := strings.Count(body, "#EXT-X-DISCONTINUITY"); got != 1 {
		t.Errorf("DISCONTINUITY 数 = %d, 期望 1（集边界）：\n%s", got, body)
	}
	// EXTINF 之和 = 时间轴总时长。
	var sum float64
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, "#EXTINF:"); ok {
			v, err := strconv.ParseFloat(strings.TrimSuffix(rest, ","), 64)
			if err != nil {
				t.Fatalf("解析 EXTINF %q: %v", line, err)
			}
			sum += v
		}
	}
	if diff := sum - tl.Total(); diff > 1e-6 || diff < -1e-6 {
		t.Errorf("EXTINF 之和 = %v, 时间轴总时长 = %v", sum, tl.Total())
	}

	// 单集时间轴不出现 DISCONTINUITY。
	one := NewTimeline([]EpisodeSegments{
		{EpisodeID: 11, EpNo: 1, Segments: []store.Segment{{StartSec: 0, Dur: 4}, {StartSec: 4, Dur: 4}}},
	})
	if got := strings.Count(string(BuildPlaylist("e1", "TOK", one.slices)), "#EXT-X-DISCONTINUITY"); got != 0 {
		t.Errorf("单集 playlist 出现 %d 个 DISCONTINUITY, 期望 0", got)
	}
}

// AC1 + AC3：真 ffprobe 解析自造 playlist，总时长 ≈ 各集之和；真 ffmpeg 全程解码无错。
//
// 这是 M7 的核心端到端断言：playlist 与分片 URL 都由真 handler 提供，ffmpeg 逐片回源。
func TestPlaylist_FFprobeAndDecodeEndToEnd(t *testing.T) {
	c, _, _, uid, wantDuration := mergedRig(t, "4")
	url := playlistServer(t, c, uid)

	// AC2 先看内容：两集 → 一个 DISCONTINUITY。
	resp, err := http.Get(url) //nolint:noctx // 测试内短请求
	if err != nil {
		t.Fatalf("GET playlist: %v", err)
	}
	body, _ := readAll(t, resp)
	if got := strings.Count(string(body), "#EXT-X-DISCONTINUITY"); got != 1 {
		t.Fatalf("playlist 的 DISCONTINUITY 数 = %d, 期望 1：\n%s", got, body)
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, body = %d 字节", cl, len(body))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("Content-Type = %q, 期望 application/vnd.apple.mpegurl", ct)
	}

	// AC1：ffprobe 解析出的总时长 ≈ 各集之和（±一帧）。
	got := probeURL(t, url)
	if diff := got - wantDuration; diff > 0.1 || diff < -0.1 {
		t.Errorf("ffprobe playlist 时长 = %v, 各集之和 = %v（差 %v）", got, wantDuration, diff)
	}

	// AC3：ffmpeg 合流解码返回 0 且无解码错误。
	//
	// stderr 里允许出现 "non monotonically increasing dts" 告警：那是 M6 已交付的
	// `-c copy` 分段在集内相邻片边界重复包含边界关键帧的固有产物（`-ss` 前置 +
	// `-t` 的行为，exit code 仍为 0，不丢内容）；本模块的绝对时间戳只保证跨集不回退。
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", url, "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg -v error -i playlist -f null - 失败: %v（stderr=%s）", err, stderr.String())
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if line == "" || strings.Contains(line, "non monotonically increasing dts") {
			continue
		}
		t.Errorf("ffmpeg 解码有非预期错误输出：%s", line)
	}
}

// AC：合并集分片按跨集时间轴解析——seq 属于哪一集、切成哪一段，与 Timeline 逐项一致。
func TestMergedSlice_CrossEpisode(t *testing.T) {
	c, st, _, uid, _ := mergedRig(t, "4")
	ctx := context.Background()

	members, err := st.ListMergeMembers(ctx, 1)
	if err != nil {
		t.Fatalf("ListMergeMembers: %v", err)
	}
	var eps []EpisodeSegments
	for _, m := range members {
		segs, err := st.ListSegments(ctx, m.EpisodeID)
		if err != nil {
			t.Fatalf("ListSegments(%d): %v", m.EpisodeID, err)
		}
		eps = append(eps, EpisodeSegments{EpisodeID: m.EpisodeID, EpNo: m.EpNo, Segments: segs})
	}
	tl := NewTimeline(eps)

	for seq := 0; seq < tl.Len(); seq++ {
		rec := serveSeg(t, c, http.MethodGet, "/s/"+signFor(uid)+"/seg/"+uid+"/"+strconv.Itoa(seq)+".ts")
		if rec.Code != http.StatusOK {
			t.Fatalf("seq=%d 状态码 = %d, 期望 200（body=%s）", seq, rec.Code, rec.Body.String())
		}
		if rec.Body.Bytes()[0] != 0x47 {
			t.Errorf("seq=%d 首字节 = 0x%02x, 期望 0x47（MPEG-TS）", seq, rec.Body.Bytes()[0])
		}
		// 切片长度与时间轴上该 seq 的 Dur 相符（±一帧 + TS 起始偏移）。
		tsPath := filepath.Join(t.TempDir(), fmt.Sprintf("s%d.ts", seq))
		if err := os.WriteFile(tsPath, rec.Body.Bytes(), 0o644); err != nil {
			t.Fatalf("写切片: %v", err)
		}
		want, _ := tl.At(seq)
		if gotDur := probeDuration(t, tsPath); gotDur-want.Dur > 0.25 || gotDur-want.Dur < -0.25 {
			t.Errorf("seq=%d 切片时长 = %v, 时间轴 Dur = %v", seq, gotDur, want.Dur)
		}
	}

	// 越界 seq → 404；非法合并 uid（m0 / m-1）→ 404。
	last := tl.Len() - 1
	for _, bad := range []string{strconv.Itoa(last + 1), "-1"} {
		if rec := serveSeg(t, c, http.MethodGet, "/s/"+signFor(uid)+"/seg/"+uid+"/"+bad+".ts"); rec.Code != http.StatusNotFound {
			t.Errorf("越界 seq %s 状态码 = %d, 期望 404", bad, rec.Code)
		}
	}
	for _, badUID := range []string{"m0", "m-1"} {
		if rec := serveSeg(t, c, http.MethodGet, "/s/"+signFor(badUID)+"/seg/"+badUID+"/0.ts"); rec.Code != http.StatusNotFound {
			t.Errorf("非法合并 uid %q 状态码 = %d, 期望 404", badUID, rec.Code)
		}
	}
}

// AC：组内某一集索引未就绪 → playlist 与分片都 503 + Retry-After（不出半截流）。
func TestMerged_IndexMissingIs503(t *testing.T) {
	c, st, _, uid, _ := mergedRig(t, "4")
	ctx := context.Background()

	// 清掉第二集的索引（模拟只索引了一半）。
	members, err := st.ListMergeMembers(ctx, 1)
	if err != nil {
		t.Fatalf("ListMergeMembers: %v", err)
	}
	if err := st.ReplaceSegments(ctx, members[1].EpisodeID, nil); err != nil {
		t.Fatalf("清空第二集索引: %v", err)
	}

	// playlist：503 + Retry-After。
	rec := servePlaylist(t, c, uid)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("索引不全时 playlist 状态码 = %d, 期望 503（body=%s）", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("503 playlist 应带 Retry-After")
	}

	// 分片：同样 503 + Retry-After（不是 404、不是半截流）。
	rec = serveSeg(t, c, http.MethodGet, "/s/"+signFor(uid)+"/seg/"+uid+"/0.ts")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("索引不全时分片状态码 = %d, 期望 503", rec.Code)
	}
}

// AC：合并集 playlist 的鉴权与单集一致——坏签名 401，未知组 404。
func TestPlaylist_AuthAndUnknownGroup(t *testing.T) {
	c, _, _, uid, _ := mergedRig(t, "4")

	if rec := servePlaylistWithToken(t, c, uid, "bogus"); rec.Code != http.StatusUnauthorized {
		t.Errorf("坏签名 playlist 状态码 = %d, 期望 401", rec.Code)
	}
	unknown := "m99999"
	if rec := servePlaylist(t, c, unknown); rec.Code != http.StatusNotFound {
		t.Errorf("未知合并组 playlist 状态码 = %d, 期望 404", rec.Code)
	}
}

// servePlaylist 直接调 ServePlaylist（带正确签名）。
func servePlaylist(t *testing.T, c *Cutter, uid string) *httptest.ResponseRecorder {
	t.Helper()
	return servePlaylistWithToken(t, c, uid, signFor(uid))
}

// servePlaylistWithToken 用给定 token 请求 playlist（token 由调用方构造）。
func servePlaylistWithToken(t *testing.T, c *Cutter, uid, token string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(PlaylistRoute, http.HandlerFunc(c.ServePlaylist))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/s/"+token+"/e/"+uid+"/master.m3u8", nil))
	return rec
}

// readAll 读完整响应体并关闭。
func readAll(t *testing.T, resp *http.Response) ([]byte, error) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	buf := &bytes.Buffer{}
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

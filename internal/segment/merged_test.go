package segment

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/store"
)

// mergedRig 在基础测试台上再加一个合并组：两集各 indexTarget 秒，成员按 1→2 顺序。
//
// 返回 (rig, 合并集 uid, 两集源时长之和)。
func mergedRig(t *testing.T, segTarget string) (*rig, string, float64) {
	t.Helper()
	r := newRig(t, segTarget)
	var sum float64
	for i := 1; i <= 2; i++ {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("m%d.mp4", i))
		makeVideo(t, path, 6)
		sum += probeDuration(t, path)
		r.addEpisode(t, i, 6)
	}
	ids := make([]int64, 0, 2)
	eps, err := r.st.ListEpisodes(context.Background(), r.dramaID)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	for _, ep := range eps {
		ids = append(ids, ep.ID)
	}
	gid, err := r.st.CreateMergeGroup(context.Background(), r.dramaID, "第1-2集", ids)
	if err != nil {
		t.Fatalf("CreateMergeGroup: %v", err)
	}
	return r, "m" + strconv.FormatInt(gid, 10), sum
}

// TestBuildPlaylist_TimelineAndDiscontinuity 覆盖 playlist 的纯函数形态：
// EXTINF 总时长 = 时间轴总时长、集边界恰好一个 DISCONTINUITY、URI 指回 /video。
func TestBuildPlaylist_TimelineAndDiscontinuity(t *testing.T) {
	tl := NewTimeline([]EpisodeSegments{
		{EpisodeID: 11, EpNo: 1, Segments: []store.Segment{{StartSec: 0, Dur: 4}, {StartSec: 4, Dur: 4}}},
		{EpisodeID: 22, EpNo: 2, Segments: []store.Segment{{StartSec: 0, Dur: 5}, {StartSec: 5, Dur: 3}}},
	})
	body := string(BuildPlaylist("m7-0123456789ab", tl.slices))

	if !strings.HasPrefix(body, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n") {
		t.Errorf("playlist 头不对：\n%s", body)
	}
	if !strings.HasSuffix(body, "#EXT-X-ENDLIST\n") {
		t.Errorf("playlist 应以 ENDLIST 结束：\n%s", body)
	}
	// TARGETDURATION ≥ 最大片长（5s）。
	if !strings.Contains(body, "#EXT-X-TARGETDURATION:5\n") {
		t.Errorf("TARGETDURATION 应为最大片长向上取整 5：\n%s", body)
	}
	// 每个分片一行 EXTINF + 一行 URI，URI 是 /video/{id}/{seq}.ts。
	for seq := 0; seq < tl.Len(); seq++ {
		want := "/video/m7-0123456789ab/" + strconv.Itoa(seq) + ".ts\n"
		if !strings.Contains(body, want) {
			t.Errorf("playlist 缺分片 %q：\n%s", want, body)
		}
	}
	if got, want := strings.Count(body, "#EXTINF:"), tl.Len(); got != want {
		t.Errorf("EXTINF 行数 = %d, 期望 %d", got, want)
	}
	// 4 片、两集 → 恰一个集边界 DISCONTINUITY。
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
	if got := strings.Count(string(BuildPlaylist("e1-0123456789ab", one.slices)), "#EXT-X-DISCONTINUITY"); got != 0 {
		t.Errorf("单集 playlist 出现 %d 个 DISCONTINUITY, 期望 0", got)
	}
}

// TestPlaylist_MergedEndToEnd 是合并集的端到端断言：playlist 与分片都由真 handler
// 提供，真 ffprobe 量总时长、真 ffmpeg 逐片回源解码无错。
func TestPlaylist_MergedEndToEnd(t *testing.T) {
	r, uid, wantDuration := mergedRig(t, "4")

	rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8")
	if rec.Code != http.StatusOK {
		t.Fatalf("playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if got := strings.Count(body, "#EXT-X-DISCONTINUITY"); got != 1 {
		t.Fatalf("playlist 的 DISCONTINUITY 数 = %d, 期望 1：\n%s", got, body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("Content-Type = %q, 期望 application/vnd.apple.mpegurl", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %q, body = %d 字节", cl, rec.Body.Len())
	}

	// 真服务起在本地端口，让 ffprobe/ffmpeg 通过 HTTP 逐片回源。
	srv := newPlaybackServer(t, r)
	url := srv + "/video/" + r.id(uid) + ".m3u8"
	if got := probeURL(t, url); got-wantDuration > 0.1 || got-wantDuration < -0.1 {
		t.Errorf("ffprobe playlist 时长 = %v, 各集之和 = %v（差 %v）", got, wantDuration, got-wantDuration)
	}

	// ffmpeg 合流解码返回 0 且无解码错误。stderr 里允许 "non monotonically
	// increasing dts"：那是 `-c copy` 分段在片边界重复包含边界关键帧的固有产物。
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

// TestMergedSlice_CrossEpisode 覆盖跨集分片：seq 属于哪一集、切成哪一段，与 Timeline
// 逐项一致；越界 seq 与非法合并 uid 都是 404。
func TestMergedSlice_CrossEpisode(t *testing.T) {
	r, uid, _ := mergedRig(t, "4")
	ctx := context.Background()
	// 先请求 playlist：合并组的分片索引由这次请求按需建立。
	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}

	members, err := r.st.ListMergeMembers(ctx, 1)
	if err != nil {
		t.Fatalf("ListMergeMembers: %v", err)
	}
	var eps []EpisodeSegments
	for _, m := range members {
		segs, err := r.st.ListSegments(ctx, m.EpisodeID)
		if err != nil {
			t.Fatalf("ListSegments(%d): %v", m.EpisodeID, err)
		}
		eps = append(eps, EpisodeSegments{EpisodeID: m.EpisodeID, EpNo: m.EpNo, Segments: segs})
	}
	tl := NewTimeline(eps)

	for seq := 0; seq < tl.Len(); seq++ {
		rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/"+strconv.Itoa(seq)+".ts")
		if rec.Code != http.StatusOK {
			t.Fatalf("seq=%d 状态码 = %d, 期望 200（body=%s）", seq, rec.Code, rec.Body.String())
		}
		if rec.Body.Bytes()[0] != 0x47 {
			t.Errorf("seq=%d 首字节 = 0x%02x, 期望 0x47（MPEG-TS）", seq, rec.Body.Bytes()[0])
		}
		tsPath := filepath.Join(t.TempDir(), fmt.Sprintf("s%d.ts", seq))
		if err := os.WriteFile(tsPath, rec.Body.Bytes(), 0o644); err != nil {
			t.Fatalf("写切片: %v", err)
		}
		want, _ := tl.At(seq)
		if gotDur := probeDuration(t, tsPath); gotDur-want.Dur > 0.25 || gotDur-want.Dur < -0.25 {
			t.Errorf("seq=%d 切片时长 = %v, 时间轴 Dur = %v", seq, gotDur, want.Dur)
		}
	}

	last := tl.Len() - 1
	for _, bad := range []string{strconv.Itoa(last + 1), "-1"} {
		if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/"+bad+".ts"); rec.Code != http.StatusNotFound {
			t.Errorf("越界 seq %s 状态码 = %d, 期望 404", bad, rec.Code)
		}
	}
	// `m-1` 不在这里：id 形态是 `<uid>-<签名>`，uid 里带 `-` 时拆出的 uid 就不是 `m-1`，
	// 按契约落 401（签名不对）而不是 404。真实 uid 永远不含 `-`（`e<id>` / `m<id>`）。
	for _, badUID := range []string{"m0", "m99999"} {
		if rec := r.do(t, http.MethodGet, "/video/"+r.id(badUID)+".m3u8"); rec.Code != http.StatusNotFound {
			t.Errorf("非法/未知合并 uid %q 状态码 = %d, 期望 404", badUID, rec.Code)
		}
	}
}

// TestMerged_IndexOnDemand 覆盖「索引按需建立」对合并组同样成立：清掉某成员的索引后
// 再请求，索引被重建而不是 503（禁止批量预建，但不等于不给播）。
func TestMerged_IndexOnDemand(t *testing.T) {
	r, uid, _ := mergedRig(t, "4")
	ctx := context.Background()
	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("首次 playlist 状态码 = %d, 期望 200", rec.Code)
	}

	members, err := r.st.ListMergeMembers(ctx, 1)
	if err != nil {
		t.Fatalf("ListMergeMembers: %v", err)
	}
	cleared := members[1].EpisodeID
	if err := r.st.ReplaceSegments(ctx, cleared, nil); err != nil {
		t.Fatalf("清空成员索引: %v", err)
	}

	rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8")
	if rec.Code != http.StatusOK {
		t.Fatalf("成员索引缺失时 playlist 状态码 = %d, 期望 200（按需重建；body=%s）", rec.Code, rec.Body.String())
	}
	segs, err := r.st.ListSegments(ctx, cleared)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segs) == 0 {
		t.Error("成员索引没有被按需重建（库里仍是空的）")
	}
}

// TestPlaylist_AuthAndUnknownGroup 覆盖合并集的鉴权：坏签名 401（签名绑定 uid）。
func TestPlaylist_AuthAndUnknownGroup(t *testing.T) {
	r, _, _ := mergedRig(t, "4")

	if rec := r.do(t, http.MethodGet, "/video/bogus-signature.m3u8"); rec.Code != http.StatusUnauthorized {
		t.Errorf("坏签名 playlist 状态码 = %d, 期望 401", rec.Code)
	}
}

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

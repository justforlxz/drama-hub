package segment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
)

// mergedRig 在基础测试台上放两集（每集 seconds 秒），并把 merge_chunk_size 调成 10，
// 于是两集并成一个合并块（对外剧集读时现算，ADR-0014）。
// uid 形态的真值在 store：m<dramaID>_<epFrom>_<epTo>。
func mergedRig(t *testing.T, seconds int) (*rig, string, float64) {
	t.Helper()
	r := newRig(t)
	// 本测试要的是「合并块」：把分块大小调成 10，两集就会并成一块。
	if err := r.settings.Set(context.Background(), store.KeyMergeChunkSize, "10"); err != nil {
		t.Fatalf("设置 merge_chunk_size: %v", err)
	}
	var sum float64
	for i := 1; i <= 2; i++ {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("m%d.mp4", i))
		makeVideo(t, path, seconds)
		sum += fileDuration(t, path)
		r.addEpisode(t, i, seconds)
	}
	// uid 形态的真值在 store（m<dramaID>_<epFrom>_<epTo>）。
	return r, fmt.Sprintf("m%d_1_2", r.dramaID), sum
}

// blockMembers 取 uid 当前的成员集：播放面与断言共用同一个入口（store 现算当前分块）。
func blockMembers(t *testing.T, r *rig, uid string) []store.Episode {
	t.Helper()
	_, members, err := r.st.EmbyItemByUID(context.Background(), uid)
	if err != nil {
		t.Fatalf("EmbyItemByUID(%q): %v", uid, err)
	}
	return members
}

// TestBuildPlaylist_TimelineAndDiscontinuity 覆盖 playlist 的纯函数形态：
// 每集一片、EXTINF 取该集时长、集边界恰好一个 DISCONTINUITY、URI 指回 /video。
func TestBuildPlaylist_TimelineAndDiscontinuity(t *testing.T) {
	tl := NewTimeline([]TimelineEpisode{
		{EpisodeID: 11, Dur: 4},
		{EpisodeID: 22, Dur: 5},
	})
	body := string(BuildPlaylist("m7-0123456789ab", tl.slices))

	if !strings.HasPrefix(body, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-PLAYLIST-TYPE:VOD\n") {
		t.Errorf("playlist 头不对：\n%s", body)
	}
	if !strings.HasSuffix(body, "#EXT-X-ENDLIST\n") {
		t.Errorf("playlist 应以 ENDLIST 结束：\n%s", body)
	}
	// TARGETDURATION ≥ 最大片长（5s）。
	if !strings.Contains(body, "#EXT-X-TARGETDURATION:5\n") {
		t.Errorf("TARGETDURATION 应为最大片长向上取整 5：\n%s", body)
	}
	if !strings.Contains(body, "#EXTINF:4.000,\n") || !strings.Contains(body, "#EXTINF:5.000,\n") {
		t.Errorf("EXTINF 应取各集真实时长：\n%s", body)
	}
	// 每个分片一行 EXTINF + 一行 URI，URI 是 /video/{id}/{seq}.m4s。
	for seq := 0; seq < tl.Len(); seq++ {
		want := "/video/m7-0123456789ab/" + strconv.Itoa(seq) + ".m4s\n"
		if !strings.Contains(body, want) {
			t.Errorf("playlist 缺分片 %q：\n%s", want, body)
		}
	}
	if got, want := strings.Count(body, "#EXTINF:"), tl.Len(); got != want {
		t.Errorf("EXTINF 行数 = %d, 期望 %d", got, want)
	}
	// 两集 → 恰一个集边界 DISCONTINUITY。
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
	one := NewTimeline([]TimelineEpisode{{EpisodeID: 11, Dur: 4}})
	if got := strings.Count(string(BuildPlaylist("e1-0123456789ab", one.slices)), "#EXT-X-DISCONTINUITY"); got != 0 {
		t.Errorf("单集 playlist 出现 %d 个 DISCONTINUITY, 期望 0", got)
	}
}

// TestPlaylist_MergedEndToEnd 是合并集的端到端断言：playlist 与分片都由真 handler
// 提供，真 ffprobe 量总时长、真 ffmpeg 逐片回源解码无错。
func TestPlaylist_MergedEndToEnd(t *testing.T) {
	r, uid, wantDuration := mergedRig(t, 6)

	rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8")
	if rec.Code != http.StatusOK {
		t.Fatalf("playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// -test.v 里贴出真实 playlist：AC「合并集 N 片、N-1 个 DISCONTINUITY」的直接证据。
	t.Logf("合并集 playlist（2 集）：\n%s", body)
	if got := strings.Count(body, "#EXT-X-DISCONTINUITY"); got != 1 {
		t.Fatalf("playlist 的 DISCONTINUITY 数 = %d, 期望 1：\n%s", got, body)
	}
	if got := strings.Count(body, "#EXTINF:"); got != 2 {
		t.Fatalf("EXTINF 行数 = %d, 期望 2（每集一片）：\n%s", got, body)
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
	// increasing dts"：集边界的时间戳重置配合 DISCONTINUITY，个别版本仍会提示。
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

// TestMergedSlice_PerEpisode 覆盖跨集分片：第 seq 片就是第 seq 集，时长与那一集一致；
// 越界 seq 与非法合并 uid 都是 404。
func TestMergedSlice_PerEpisode(t *testing.T) {
	r, uid, _ := mergedRig(t, 6)
	// 先请求 playlist：合并块每个成员的时长与通道行由这次请求补齐。
	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}

	members := blockMembers(t, r, uid)
	if len(members) != 2 {
		t.Fatalf("合并块成员数 = %d, 期望 2", len(members))
	}

	for seq, m := range members {
		rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/"+strconv.Itoa(seq)+".m4s")
		if rec.Code != http.StatusOK {
			t.Fatalf("seq=%d 状态码 = %d, 期望 200（body=%s）", seq, rec.Code, rec.Body.String())
		}
		start, exact := sliceStart(members, seq)
		if !exact {
			t.Fatalf("seq=%d 的前置集时长没回填（playlist 应先回填）", seq)
		}
		assertSelfContainedSegment(t, rec.Body.Bytes(), start.Seconds(), float64(m.DurationSec))
	}

	for _, bad := range []string{strconv.Itoa(len(members)), "-1"} {
		if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/"+bad+".m4s"); rec.Code != http.StatusNotFound {
			t.Errorf("越界 seq %s 状态码 = %d, 期望 404", bad, rec.Code)
		}
	}
	// uid 里带 `-` 的场景不在这里：id 形态是 `<uid>-<签名>`，拆出的 uid 就不是原值，
	// 按契约落 401（签名不对）而不是 404。真实 uid 永远不含 `-`
	// （`e<id>` / `m<dramaID>_<from>_<to>`）。
	for _, badUID := range []string{"m0_1_2", "m99999_1_2", "m1_1"} {
		if rec := r.do(t, http.MethodGet, "/video/"+r.id(badUID)+".m3u8"); rec.Code != http.StatusNotFound {
			t.Errorf("非法/未知合并 uid %q 状态码 = %d, 期望 404", badUID, rec.Code)
		}
	}
}

// TestMergedChannelRowMissing 覆盖分片端点的兜底路径：通道行被清掉后直接请求分片
// （没有先取 playlist），这时才现解析一次直链、把行写回去再出流。
func TestMergedChannelRowMissing(t *testing.T) {
	r, uid, _ := mergedRig(t, 6)
	ctx := context.Background()
	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("首次 playlist 状态码 = %d, 期望 200", rec.Code)
	}

	members := blockMembers(t, r, uid)
	// 模拟「行丢了」：直接删掉该成员的通道行。
	cleared := members[1].ID
	if _, err := r.db.ExecContext(ctx, `DELETE FROM segment WHERE episode_id = $1`, cleared); err != nil {
		t.Fatalf("清空通道行: %v", err)
	}

	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/1.m4s"); rec.Code != http.StatusOK {
		t.Fatalf("通道行缺失时分片状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	seg, err := r.st.GetSegment(ctx, cleared)
	if err != nil {
		t.Fatalf("GetSegment（兜底后）: %v", err)
	}
	if seg.Channel != channelWeb {
		t.Errorf("兜底写回的通道 = %q, 期望 %q", seg.Channel, channelWeb)
	}
}

// TestPlaylist_AuthAndUnknownGroup 覆盖合并集的鉴权：坏签名 401（签名绑定 uid）。
func TestPlaylist_AuthAndUnknownGroup(t *testing.T) {
	r, _, _ := mergedRig(t, 6)

	if rec := r.do(t, http.MethodGet, "/video/bogus-signature.m3u8"); rec.Code != http.StatusUnauthorized {
		t.Errorf("坏签名 playlist 状态码 = %d, 期望 401", rec.Code)
	}
}

// TestBuildPlaylist_TenEpisodes 钉住 AC「合并集 10 集 = 10 片 + 9 个 DISCONTINUITY」：
// 纯函数形态，不跑 ffmpeg，所以这里能直接按 10 集算。
func TestBuildPlaylist_TenEpisodes(t *testing.T) {
	eps := make([]TimelineEpisode, 0, 10)
	total := 0.0
	for i := 0; i < 10; i++ {
		dur := float64(45 + i) // 各集时长不同，EXTINF 就不能是常量
		total += dur
		eps = append(eps, TimelineEpisode{EpisodeID: int64(100 + i), Dur: dur})
	}
	tl := NewTimeline(eps)
	body := string(BuildPlaylist("m1-0123456789ab", tl.slices))

	if got := strings.Count(body, "#EXTINF:"); got != 10 {
		t.Errorf("EXTINF 行数 = %d, 期望 10（每集一片）", got)
	}
	if got := strings.Count(body, "#EXT-X-DISCONTINUITY"); got != 9 {
		t.Errorf("DISCONTINUITY 数 = %d, 期望 9（集边界 = 集数-1）", got)
	}
	for seq := 0; seq < 10; seq++ {
		if want := "/video/m1-0123456789ab/" + strconv.Itoa(seq) + ".m4s\n"; !strings.Contains(body, want) {
			t.Errorf("playlist 缺分片 %q", want)
		}
	}
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
	if diff := sum - total; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("ΣEXTINF = %v, Σ各集时长 = %v", sum, total)
	}
	// TARGETDURATION = 最长一集向上取整。
	if !strings.Contains(body, "#EXT-X-TARGETDURATION:54\n") {
		t.Errorf("TARGETDURATION 应为 54（最长一集 54 s）：\n%s", body)
	}
}

// TestTimelineEpisodesAreSegments 钉住「一集一片」的映射不变量：时间轴长度 = 集数，
// 第 n 片的 EpisodeID 与时长就是第 n 集的。
func TestTimelineEpisodesAreSegments(t *testing.T) {
	tl := NewTimeline([]TimelineEpisode{{EpisodeID: 11, Dur: 4}, {EpisodeID: 22, Dur: 6}})
	if tl.Len() != 2 {
		t.Fatalf("Len = %d, 期望 2（每集一片）", tl.Len())
	}
	want := []Slice{
		{Seq: 0, EpisodeID: 11, Dur: 4},
		{Seq: 1, EpisodeID: 22, Dur: 6},
	}
	for _, w := range want {
		got, ok := tl.At(w.Seq)
		if !ok || got != w {
			t.Errorf("At(%d) = (%+v, %v), 期望 %+v", w.Seq, got, ok, w)
		}
	}
	if _, ok := tl.At(2); ok {
		t.Error("At(Len) 应越界")
	}
	if _, ok := tl.At(-1); ok {
		t.Error("At(-1) 应越界")
	}
	if got := tl.Total(); !closeEnough(got, 10) {
		t.Errorf("Total = %v, 期望 10", got)
	}
}

// TestUnitMissingUID 钉住「uid 不在派生表里 → 404」：合法签名 + 库里没有这一集。
func TestUnitMissingUID(t *testing.T) {
	r := newRig(t)
	r.addEpisode(t, 1, 6)

	_, err := r.c.unit(context.Background(), "e999999")
	if !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("未知 uid 的错误 = %v, 期望包 httpx.ErrNotFound（404）", err)
	}
}

// TestMergedPlaylist_DecodesEveryFrame 是 MKACG-652 功能检测那条阻断项的回归钉子：
// **整条 playlist 的 nb_read_frames 必须等于 nb_read_packets**。
//
// 缺陷现场：每片自身的时间戳从 0 起，hls demuxer 遇到 `#EXT-X-DISCONTINUITY` 只打印
// Skip、不重定基准，包 PTS 在集边界回退 → 解码器丢帧（实测 3 集 4/6/3 秒共 325 帧，
// 整条 playlist 只解出 275 帧；每片单独解则满帧）。出流时把 tfdt 平移到「前面各集
// EXTINF 之和」后恢复满帧（`mp4time.go` / ADR-0011 D6'）。
//
// 同时钉住「平移只发生在出站那一步」：缓存文件仍是 0 基（ADR-0011 D3 的缓存键不变）。
func TestMergedPlaylist_DecodesEveryFrame(t *testing.T) {
	const fps = 25 // makeVideo 的夹具帧率
	r, uid, sumSec := mergedRig(t, 6)
	ctx := context.Background()
	id := r.id(uid)

	if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
		t.Fatalf("playlist 状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}

	srv := newPlaybackServer(t, r)
	playlist := srv + "/video/" + id + ".m3u8"
	frames, packets := frameAndPacketCount(t, playlist)
	want := int(math.Round(sumSec * fps))
	t.Logf("整条 playlist：frames=%d packets=%d（期望 ≥%d）", frames, packets, want)
	if frames != packets {
		t.Errorf("frames=%d ≠ packets=%d：集边界时间戳回退导致丢帧（tfdt 没平移？）", frames, packets)
	}
	if frames < want-2 {
		t.Errorf("解码帧数 = %d, 期望约 %d（%.2f 秒 × %d fps）", frames, want, sumSec, fps)
	}

	// 出站字节：第 0 片起点 0（零拷贝），第 1 片起点 = 第 1 集的时长。
	if got := firstVideoPTS(t, srv+"/video/"+id+"/0.m4s"); got > 1 {
		t.Errorf("第 0 片首包 PTS = %.3f, 期望 ≈0（首片不平移）", got)
	}
	if got := firstVideoPTS(t, srv+"/video/"+id+"/1.m4s"); math.Abs(got-6) > 0.5 {
		t.Errorf("第 1 片首包 PTS = %.3f, 期望 ≈6（= 第 1 集时长）", got)
	}

	// 缓存文件仍是 0 基：字节只由（集, 通道）决定，平移是出站时才做的。
	eps := blockMembers(t, r, uid)
	key := cache.SegmentKey(episodeUID(eps[1].ID), channelWeb)
	row, err := r.st.ArtifactByKey(ctx, store.ArtifactKindSegment, key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if got := firstVideoPTS(t, row.Path); got > 1 {
		t.Errorf("缓存文件里第 2 集的 PTS 已被平移（%.3f）：缓存应保持 0 基", got)
	}
	// 出站那一片与缓存同长、片头仍是 ftyp（自包含结构不变）。
	served := r.do(t, http.MethodGet, "/video/"+id+"/1.m4s")
	cached, err := os.ReadFile(row.Path)
	if err != nil {
		t.Fatalf("读缓存文件: %v", err)
	}
	if served.Body.Len() != len(cached) {
		t.Errorf("出站 %d 字节 ≠ 缓存 %d 字节（平移只改 4/8 字节字段，长度必须一致）", served.Body.Len(), len(cached))
	}
	if boxes := topLevelBoxes(t, served.Body.Bytes()); len(boxes) < 2 || boxes[0] != "ftyp" || boxes[1] != "moov" {
		t.Errorf("出站片段的顶层 box = %v, 期望以 ftyp,moov 开头", boxes)
	}
}

// TestSegmentDirectRequestFillsDuration 是功能检测第二个阻断项的回归钉子：
// **不先取 playlist、直接请求分片**时，前置集的 `duration_sec` 还是 0，时间轴起点必须现补齐
// 再平移 —— 否则出站片首 PTS 恒 0，拼成 playlist 后 ffmpeg 系播放端又会丢帧
// （检测现场：4 片首 PTS 全 0，1550 包只解出 800 帧）。
func TestSegmentDirectRequestFillsDuration(t *testing.T) {
	r, uid, _ := mergedRig(t, 6) // 两集各 6 秒；此时还没取过 playlist，时长都还是 0
	ctx := context.Background()

	members := blockMembers(t, r, uid)
	for _, m := range members {
		if m.DurationSec != 0 {
			t.Fatalf("前置条件不成立：ep%d duration_sec = %d, 期望 0（还没取过 playlist）",
				m.EpNo, m.DurationSec)
		}
	}

	// 直接请求第 2 片（第 1 片的前置集是第 1 集，时长待补）：首 PTS 必须落在 6 s。
	rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/1.m4s")
	if rec.Code != http.StatusOK {
		t.Fatalf("直接请求分片状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	assertSelfContainedSegment(t, rec.Body.Bytes(), 6, 6)

	// 补齐落库：第 1 集的时长被 ffprobe 回填，后续请求不再外呼。
	first, err := r.st.EpisodeByID(ctx, members[0].ID)
	if err != nil {
		t.Fatalf("EpisodeByID: %v", err)
	}
	if first.DurationSec != 6 {
		t.Errorf("补齐后第 1 集 duration_sec = %d, 期望 6（ffprobe 实测）", first.DurationSec)
	}
	resolvesBefore := r.resolves.Load()
	second := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/1.m4s")
	if second.Code != http.StatusOK {
		t.Fatalf("第二次状态码 = %d, 期望 200", second.Code)
	}
	if got := r.resolves.Load(); got != resolvesBefore {
		t.Errorf("补齐后第二次请求仍解析了 %d 次直链, 期望 0（时长与通道行都已在库）", got-resolvesBefore)
	}
	if !bytes.Equal(rec.Body.Bytes(), second.Body.Bytes()) {
		t.Error("第二次出站字节与第一次不同（平移只依赖库里的时长，必须稳定）")
	}
}

// frameAndPacketCount 读一条 playlist（或一个文件）的可解码帧数与包数。
// 两者不等意味着播放端会「读到包但解不出画面」——正是集边界时间戳回退的症状。
func frameAndPacketCount(t *testing.T, src string) (frames, packets int) {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-count_frames", "-count_packets",
		"-show_entries", "stream=nb_read_frames,nb_read_packets",
		"-of", "csv=p=0", src).Output()
	if err != nil {
		t.Fatalf("ffprobe 统计 %s: %v", src, err)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d,%d", &frames, &packets); err != nil {
		t.Fatalf("解析 ffprobe 计数 %q: %v", out, err)
	}
	return frames, packets
}

// firstVideoPTS 读第一条视频包的 PTS（秒）。
func firstVideoPTS(t *testing.T, src string) float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=pts_time", "-of", "csv=p=0", src).Output()
	if err != nil {
		t.Fatalf("ffprobe 取首包 PTS %s: %v", src, err)
	}
	first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	v, err := strconv.ParseFloat(strings.TrimSpace(first), 64)
	if err != nil {
		t.Fatalf("解析首包 PTS %q: %v", first, err)
	}
	return v
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

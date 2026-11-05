package segment

// MKACG-686：冷路径边生成边出流的验收测试。
//
// 这里钉住四条不属于「能跑就行」的行为：
//   - 首个分片（完整的 moof+mdat）先于整集生成完就到达客户端；
//   - 5 个客户端同时播同一集，服务端只起 1 个 ffmpeg（不是「通常只起一个」）；
//   - 边出流的字节与 remux 直出逐字节一致（sha256），成品可解码；
//   - HEAD / Range 在冷路径上仍然给出精确长度与精确区间（硬约束 3）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
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

	"github.com/justforlxz/drama-hub/internal/cache"
	"github.com/justforlxz/drama-hub/internal/store"
	"github.com/justforlxz/drama-hub/internal/stream"
)

// firstMoofEnd 返回 buf 中第一个完整 moof box 的结束偏移（0 = 还没到齐）。
func firstMoofEnd(buf []byte) int {
	off := 0
	for off+8 <= len(buf) {
		size := int(uint32(buf[off])<<24 | uint32(buf[off+1])<<16 | uint32(buf[off+2])<<8 | uint32(buf[off+3]))
		if size < 8 || off+size > len(buf) {
			return 0
		}
		if string(buf[off+4:off+8]) == "moof" {
			return off + size
		}
		off += size
	}
	return 0
}

// byteReader 每次只交 1 个字节，逼出「box 头被读断」的边界。
type byteReader struct{ r io.Reader }

func (b *byteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return b.r.Read(p[:1])
}

// TestShiftStream_MatchesShiftTimeline：增量平移必须与整块平移逐字节一致——
// 出流路径换了实现，客户端拿到的字节不能变。一半是「一次读一大堆」，一半是
// 逐字节读（box 头与 moof 被拆开的最坏情形）。
func TestShiftStream_MatchesShiftTimeline(t *testing.T) {
	r := newRig(t)
	ep := r.addEpisode(t, 1, 6)
	src := stream.Direct{URL: r.cdn[ep.ID].url()}
	body, err := remux(context.Background(), r.c.logger, src, r.c.fetchHeaders(src))
	if err != nil {
		t.Fatalf("remux: %v", err)
	}
	for _, offset := range []time.Duration{0, 1500 * time.Millisecond, 5 * time.Second} {
		want, err := shiftTimeline(append([]byte(nil), body...), offset)
		if err != nil {
			t.Fatalf("offset %s 整块平移: %v", offset, err)
		}
		for name, in := range map[string]io.Reader{
			"整块读":  bytes.NewReader(body),
			"逐字节读": &byteReader{r: bytes.NewReader(body)},
		} {
			got, err := io.ReadAll(shiftStream(in, offset))
			if err != nil {
				t.Fatalf("offset %s %s: %v", offset, name, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("offset %s %s：增量平移与整块平移不一致（%d vs %d 字节）", offset, name, len(got), len(want))
			}
		}
	}
}

// throttleBody 把回源吞吐压到 rate 字节/秒：本地无限速时整集只要几十毫秒，
// 看不出「是不是等整集」。
type throttleBody struct {
	rc    io.ReadCloser
	rate  int
	read  int64
	start time.Time
}

func (b *throttleBody) Read(p []byte) (int, error) {
	if b.start.IsZero() {
		b.start = time.Now()
	}
	n, err := b.rc.Read(p)
	b.read += int64(n)
	want := time.Duration(float64(b.read) / float64(b.rate) * float64(time.Second))
	if el := time.Since(b.start); want > el {
		time.Sleep(want - el)
	}
	return n, err
}

func (b *throttleBody) Close() error { return b.rc.Close() }

// slowTransport 给回源加上限速（模拟真实源站带宽）。只用于拉分片：
// playlist 请求走 http.DefaultClient，不带这个 transport。
type slowTransport struct {
	rate int
	base http.RoundTripper
}

func (s *slowTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := s.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &throttleBody{rc: resp.Body, rate: s.rate}
	return resp, nil
}

// TestSegment_FirstFragmentBeforeRemuxCompletes 是本次改造的核心验收：源站只有
// 1.5MB/s 时整集要两秒多才下完，但第一个可解码分片必须远远先到。
func TestSegment_FirstFragmentBeforeRemuxCompletes(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "src.mp4")
	makeVideoSize(t, path, 90, "320x180")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 fixture: %v", err)
	}
	id, err := r.st.UpsertEpisode(ctx, store.Episode{
		DramaID: r.dramaID, EpNo: 1, Source: "web", Vid: "v1",
	})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}
	r.cdn[id] = newFakeCDN(t, body)
	ep, err := r.st.EpisodeByID(ctx, id)
	if err != nil {
		t.Fatalf("EpisodeByID: %v", err)
	}
	base := newPlaybackServer(t, r)
	uid := "e" + strconv.FormatInt(ep.ID, 10)

	// 预热 playlist：时长回填后 ffmpeg 不需要 ffprobe。
	resp, err := http.Get(base + "/video/" + r.id(uid) + ".m3u8")
	if err != nil {
		t.Fatalf("playlist: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	client := &http.Client{
		Timeout:   2 * time.Minute,
		Transport: &slowTransport{rate: 1500000},
	}
	t0 := time.Now()
	resp2, err := client.Get(base + "/video/" + r.id(uid) + "/0.m4s")
	if err != nil {
		t.Fatalf("分片请求: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", resp2.StatusCode)
	}
	if cl := resp2.Header.Get("Content-Length"); cl != "" {
		t.Fatalf("冷路径 Content-Length = %q, 期望不设（边生成边出流）", cl)
	}

	// 读到第一个完整 moof 为止。
	var acc bytes.Buffer
	buf := make([]byte, 8*1024)
	var firstFragment time.Duration
	for firstFragment == 0 {
		n, rerr := resp2.Body.Read(buf)
		acc.Write(buf[:n])
		if firstMoofEnd(acc.Bytes()) > 0 {
			firstFragment = time.Since(t0)
		}
		if rerr != nil {
			t.Fatalf("读首片: %v（已收 %d 字节）", rerr, acc.Len())
		}
	}
	rest, err := io.Copy(io.Discard, resp2.Body)
	if err != nil {
		t.Fatalf("读完: %v", err)
	}
	full := time.Since(t0)
	total := int64(acc.Len()) + rest
	t.Logf("首片 %s / 整片 %s（响应 %d 字节，限速 1.5MB/s）", firstFragment, full, total)
	if full < 1500*time.Millisecond {
		t.Fatalf("整片只用 %s，限速没生效，这条测试无法证明「首片先到」", full)
	}
	if firstFragment > full/3 {
		t.Fatalf("首片 %s 不早于整片 %s 的 1/3：仍然是等整集", firstFragment, full)
	}
	// 整集字节仍必须完整（chunked 只是不分段给长度，不是截断）。
	if total < int64(len(body))/2 {
		t.Fatalf("响应只有 %d 字节，源 %d 字节", total, len(body))
	}
}

// TestSegment_ConcurrentPlaybackOneFFmpeg：5 个客户端同时播同一集，服务端只起
// 1 个 ffmpeg。判据是真的数进程启动次数（PATH 上放一个转发脚本），不是看日志。
func TestSegment_ConcurrentPlaybackOneFFmpeg(t *testing.T) {
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("PATH 上没有 ffmpeg")
	}
	r := newRig(t)
	ep := r.addEpisode(t, 1, 12) // 建 fixture 自己也要跑 ffmpeg，必须在装 shim 之前
	target := "/video/" + r.id("e"+strconv.FormatInt(ep.ID, 10)) + "/0.m4s"

	bin := t.TempDir()
	counter := filepath.Join(bin, "ffmpeg.count")
	script := "#!/bin/sh\necho x >> " + counter + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatalf("写 ffmpeg shim: %v", err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	const clients = 5
	bodies := make([][]byte, clients)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			r.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("客户端 %d 状态码 = %d", i, rec.Code)
			}
			bodies[i] = rec.Body.Bytes()
		}(i)
	}
	close(start)
	wg.Wait()

	count, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("读 ffmpeg 计数: %v", err)
	}
	if got := strings.Count(string(count), "x"); got != 1 {
		t.Fatalf("并发生成起了 %d 个 ffmpeg, 期望 1", got)
	}
	for i := 1; i < clients; i++ {
		if !bytes.Equal(bodies[i], bodies[0]) {
			t.Fatalf("客户端 %d 拿到的字节与客户端 0 不同", i)
		}
	}
	assertSelfContainedSegment(t, bodies[0], 0, 12)
}

// TestSegment_StreamedBytesEqualDirectRemux：冷路径出流的字节必须与改造前
// `remux` 直出的字节完全一致——换了出流方式，不许换内容。
func TestSegment_StreamedBytesEqualDirectRemux(t *testing.T) {
	r := newRig(t)
	ep := r.addEpisode(t, 1, 12)
	uid := "e" + strconv.FormatInt(ep.ID, 10)

	rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/0.m4s")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（body=%s）", rec.Code, rec.Body.String())
	}
	src := stream.Direct{URL: r.cdn[ep.ID].url()}
	raw, err := remux(context.Background(), r.c.logger, src, r.c.fetchHeaders(src))
	if err != nil {
		t.Fatalf("remux: %v", err)
	}
	gotSum := sha256.Sum256(rec.Body.Bytes())
	wantSum := sha256.Sum256(raw)
	if hex.EncodeToString(gotSum[:]) != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("出流字节与 remux 直出不一致：%d vs %d 字节", rec.Body.Len(), len(raw))
	}
	// 落盘的缓存文件也必须是同一份字节（重启后靠它服务）。
	artifacts, err := r.st.ListArtifactsOldest(context.Background(), store.ArtifactKindSegment, 10)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("缓存行 = %v, err = %v", artifacts, err)
	}
	onDisk, err := os.ReadFile(artifacts[0].Path)
	if err != nil {
		t.Fatalf("读缓存文件: %v", err)
	}
	if !bytes.Equal(onDisk, rec.Body.Bytes()) {
		t.Fatal("缓存文件与出流字节不一致")
	}
}

// TestSegment_ColdHeadAndRangeExact：冷路径的 HEAD 与 Range 仍必须精确
// （硬约束 3）。它们走阻塞路径：长度/区间只有整集生成完才知道。
func TestSegment_ColdHeadAndRangeExact(t *testing.T) {
	r := newRig(t)
	headEp := r.addEpisode(t, 1, 12)
	rangeEp := r.addEpisode(t, 2, 12)

	headTarget := "/video/" + r.id("e"+strconv.FormatInt(headEp.ID, 10)) + "/0.m4s"
	headRec := r.do(t, http.MethodHead, headTarget)
	if headRec.Code != http.StatusOK {
		t.Fatalf("HEAD 状态码 = %d", headRec.Code)
	}
	n, err := strconv.ParseInt(headRec.Header().Get("Content-Length"), 10, 64)
	if err != nil || n <= 0 {
		t.Fatalf("冷路径 HEAD Content-Length = %q", headRec.Header().Get("Content-Length"))
	}
	// 同一集 GET 的完整长度要与 HEAD 一致。
	full := r.do(t, http.MethodGet, headTarget)
	if full.Code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d", full.Code)
	}
	if int64(full.Body.Len()) != n {
		t.Fatalf("GET %d 字节 != HEAD %d", full.Body.Len(), n)
	}

	// Range 落在冷路径上：206 + 精确 Content-Range。
	rangeTarget := "/video/" + r.id("e"+strconv.FormatInt(rangeEp.ID, 10)) + "/0.m4s"
	req := httptest.NewRequest(http.MethodGet, rangeTarget, nil)
	req.Header.Set("Range", "bytes=0-187")
	rec := httptest.NewRecorder()
	r.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("冷路径 Range 状态码 = %d, 期望 206", rec.Code)
	}
	if rec.Body.Len() != 188 {
		t.Fatalf("Range body = %d 字节, 期望 188", rec.Body.Len())
	}
	rangeFull := r.do(t, http.MethodGet, rangeTarget)
	if rangeFull.Code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d", rangeFull.Code)
	}
	wantRange := "bytes 0-187/" + strconv.Itoa(rangeFull.Body.Len())
	if cr := rec.Header().Get("Content-Range"); cr != wantRange {
		t.Fatalf("Content-Range = %q, 期望 %q", cr, wantRange)
	}
	// 区间内容就是完整分片的前缀。
	if !bytes.Equal(rec.Body.Bytes(), rangeFull.Body.Bytes()[:188]) {
		t.Fatal("Range 返回的字节与完整分片的前缀不一致")
	}
}

// TestSegment_RetriesAfterLeaderCancel：领跑者是一次后台预热、随后被前台 preempt 取消。
// 还挂在这份在途生成上的播放请求一个字节都还没拿到，不能拿到 500 —— 换一个新领跑者
// 重试一次，正常出完整分片（MKACG-686 代码审查 blocker）。
func TestSegment_RetriesAfterLeaderCancel(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ep := r.addEpisode(t, 1, 2)
	base := newPlaybackServer(t, r)
	uid := "e" + strconv.FormatInt(ep.ID, 10)

	// 先取一次 playlist：回填时长，后面的分片请求不再 ffprobe 回源。
	resp, err := http.Get(base + "/video/" + r.id(uid) + ".m3u8")
	if err != nil {
		t.Fatalf("playlist: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	channel, _, _, err := r.c.episodeChannel(ctx, ep)
	if err != nil {
		t.Fatalf("episodeChannel: %v", err)
	}
	spec := cache.Spec{
		Kind: store.ArtifactKindSegment,
		Key:  cache.SegmentKey(episodeUID(ep.ID), channel),
		Ext:  segmentSuffix,
	}

	// 假领跑者：占住这一集的在途表，等被取消（模拟预热被 preempt）。
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	started := make(chan struct{})
	if _, err := r.c.caches.GetOrStream(leaderCtx, spec, func(lctx context.Context, _ io.Writer) error {
		close(started)
		<-lctx.Done()
		return lctx.Err()
	}, true); err != nil {
		t.Fatalf("GetOrStream（领跑者）: %v", err)
	}
	<-started

	type result struct {
		code int
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Get(base + "/video/" + r.id(uid) + "/0.m4s")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		done <- result{code: resp.StatusCode, body: body, err: err}
	}()

	// 给播放请求一点时间挂到这次生成上，再取消领跑者。
	time.Sleep(150 * time.Millisecond)
	cancelLeader()

	res := <-done
	if res.err != nil {
		t.Fatalf("分片请求: %v", res.err)
	}
	if res.code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（领跑者被取消不该变成 500）", res.code)
	}
	if len(res.body) == 0 {
		t.Fatal("分片响应为空")
	}
	assertSelfContainedSegment(t, res.body, 0, 2)
}

// TestSegment_ReleadBudgetExhaustedWritesError：连续两次领跑者都被取消（第二次仍是
// 并发预热抢先登记的后台生成）时，重领预算耗尽也必须写出明确错误。
//
// 这条钉的是「响应退化成 200 + 空 body」：一个字节都没有的分片会让播放端按解码失败
// 处理，比 5xx 更难定位（AC-4：不得返回长度错误的响应）。
func TestSegment_ReleadBudgetExhaustedWritesError(t *testing.T) {
	r := newRig(t)
	ep := r.addEpisode(t, 1, 2)

	channel, _, _, err := r.c.episodeChannel(context.Background(), ep)
	if err != nil {
		t.Fatalf("episodeChannel: %v", err)
	}
	spec := cache.Spec{
		Kind: store.ArtifactKindSegment,
		Key:  cache.SegmentKey(episodeUID(ep.ID), channel),
		Ext:  segmentSuffix,
	}

	// 每次 fetch 都放一个「等被取消」的领跑者：读者挂上 150ms 后撤掉它，模拟预热被
	// 前台 preempt 取消。
	fetches := 0
	fetch := func() (*cache.Stream, error) {
		fetches++
		lctx, cancelLeader := context.WithCancel(context.Background())
		s, err := r.c.caches.GetOrStream(lctx, spec, func(lctx context.Context, _ io.Writer) error {
			<-lctx.Done()
			return lctx.Err()
		}, true)
		if err != nil {
			cancelLeader()
			return nil, err
		}
		time.AfterFunc(150*time.Millisecond, cancelLeader)
		return s, nil
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/video/x/0.m4s", nil)
	r.c.streamSegment(rec, req, fetch, "e1", 0, ep, channel, 0, time.Now())

	if fetches != segmentReleads+1 {
		t.Errorf("领跑者领取次数 = %d, 期望 %d（重领预算应为 %d）", fetches, segmentReleads+1, segmentReleads)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("重领预算耗尽后状态码 = 200, 期望明确错误（body=%q）", rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Fatal("重领预算耗尽后响应为空：必须给出明确错误体，不能是空 200")
	}
}

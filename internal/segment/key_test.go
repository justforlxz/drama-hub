package segment

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
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
	"github.com/justforlxz/drama-hub/internal/stream"
)

// testKey 是通道测试用的内容密钥（16 字节 AES-128，CENC-AES-CTR 的内容密钥）。
var testKey = bytes.Repeat([]byte{0x2a}, 16)

// makeEncryptedVideo 现场造一份 CENC-AES-CTR 密文 MP4，返回它的明文对照源
// （同参数生成的明文，供「解密后是否等价」比较）。加密命令与 MKACG-626 的实测
// 命令一致；容器里的 ffmpeg 就是运行时那个二进制。
func makeEncryptedVideo(t *testing.T, path string, seconds int, size string, key []byte) (plain string) {
	t.Helper()
	plain = filepath.Join(t.TempDir(), "plain.mp4")
	makeVideoSize(t, plain, seconds, size)
	kid := bytes.Repeat([]byte{0x11}, 16)
	cmd := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-i", plain, "-c", "copy",
		"-encryption_scheme", "cenc-aes-ctr",
		"-encryption_key", hex.EncodeToString(key),
		"-encryption_kid", hex.EncodeToString(kid),
		"-movflags", "+faststart",
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成密文样本失败: %v（%s）", err, out)
	}
	return plain
}

// argIndex 返回 argv 里第一个等于 want 的下标；没有返回 -1。
func argIndex(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// frameHash 用 ffmpeg 解出自 TS 的视频帧并算 MD5：两条取法（明文 / 密文+key）
// 解出的帧一致，才算「原生解密真的生效」。
func frameHash(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path,
		"-map", "0:v:0", "-f", "hash", "-hash", "md5", "-").Output()
	if err != nil {
		t.Fatalf("算解码帧哈希 %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

// writeTemp 把字节写进临时文件（ffprobe / ffmpeg 只认路径）。
func writeTemp(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("写临时文件 %s: %v", path, err)
	}
	return path
}

// TestCutWithKey 覆盖 AC「带 key 的流真的能切出分片」：
//   - argv：`-decryption_key <hex>` 出现一次且落在 `-i` 之前；明文流的 argv 里
//     完全没有这个 flag（传空值 ffmpeg 会当非法参数）；
//   - 真切片：密文源 + 正确 key 切出来的 TS 能解码，帧与同一区间的明文切片一致。
func TestCutWithKey(t *testing.T) {
	plain := stream.Direct{URL: "https://cdn.example/ep.mp4"}
	cipher := stream.Direct{URL: "https://cdn.example/ep.mp4", Key: testKey}

	// 1) argv 顺序：ffmpeg 只把 `-i` 之前的选项当输入选项，放后面会被静默忽略。
	args := cutArgs(cipher, 4, 4, 0, "")
	keyAt, inAt := argIndex(args, "-decryption_key"), argIndex(args, "-i")
	if keyAt < 0 {
		t.Fatalf("带 key 的切片 argv 没有 -decryption_key: %v", args)
	}
	if keyAt > inAt {
		t.Fatalf("-decryption_key 落在 -i 之后（会被当输出选项忽略）: %v", args)
	}
	if got := args[keyAt+1]; got != hex.EncodeToString(testKey) {
		t.Fatalf("-decryption_key 的值 = %q, 期望 %x", got, hex.EncodeToString(testKey))
	}
	for i, a := range args {
		if a == "-decryption_key" && i != keyAt {
			t.Fatalf("-decryption_key 出现多次: %v", args)
		}
	}
	for _, a := range cutArgs(plain, 4, 4, 0, "") {
		if strings.Contains(a, "decryption") {
			t.Fatalf("明文流的 argv 里不该有解密参数: %v", cutArgs(plain, 4, 4, 0, ""))
		}
	}

	// 2) 探针同理：解密参数必须在源地址（最后一个参数）之前。
	pargs := probeArgs(cipher, "")
	keyAt = argIndex(pargs, "-decryption_key")
	if keyAt < 0 || keyAt >= len(pargs)-1 {
		t.Fatalf("-decryption_key 不在源地址之前: %v", pargs)
	}
	if last := pargs[len(pargs)-1]; last != cipher.URL {
		t.Fatalf("探针 argv 末位 = %q, 期望源地址 %q（参数顺序错了）", last, cipher.URL)
	}

	// 3) 真切片：密文源 + 正确 key，解出的帧必须与明文同区间切片一致。
	encPath := filepath.Join(t.TempDir(), "enc.mp4")
	plainPath := makeEncryptedVideo(t, encPath, 12, "160x120", testKey)
	encBody, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatalf("读取密文样本: %v", err)
	}
	cdn := newFakeCDN(t, encBody)

	fromCipher, err := cut(context.Background(), stream.Direct{URL: cdn.url(), Key: testKey}, 4, 4, 0, "")
	if err != nil {
		t.Fatalf("密文源切片失败: %v", err)
	}
	if len(fromCipher) == 0 || fromCipher[0] != 0x47 {
		t.Fatalf("密文源切出来不是 MPEG-TS（%d 字节，首字节 0x%02x）", len(fromCipher), fromCipher[0])
	}
	if d := probeDuration(t, writeTemp(t, "cipher.ts", fromCipher)); d < 3.5 || d > 4.5 {
		t.Errorf("密文切片时长 = %v, 期望约 4 秒", d)
	}

	fromPlain, err := cut(context.Background(), stream.Direct{URL: plainPath}, 4, 4, 0, "")
	if err != nil {
		t.Fatalf("明文源切片失败: %v", err)
	}
	cipherHash := frameHash(t, writeTemp(t, "decrypted.ts", fromCipher))
	plainHash := frameHash(t, writeTemp(t, "plain.ts", fromPlain))
	if cipherHash == "" || cipherHash != plainHash {
		t.Fatalf("解密后帧哈希 %q ≠ 明文同区间 %q（-decryption_key 没生效）", cipherHash, plainHash)
	}
}

// TestProbeRejectsBadKey 覆盖 AC「坏 key 时 ffprobe 失败被识别为错误，而不是产出空分片」。
//
// 为什么必须有这条：key 错时 ffprobe 常常**以 0 退出**、只往 stderr 吐
// `non-existing PPS …`，拿不到关键帧也不算失败 —— 那种「成功」会把空索引写进库、
// 把垃圾分片写进缓存并被永久命中（MKACG-626 实测）。
func TestProbeRejectsBadKey(t *testing.T) {
	encPath := filepath.Join(t.TempDir(), "enc.mp4")
	makeEncryptedVideo(t, encPath, 12, "160x120", testKey)
	body, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatalf("读取密文样本: %v", err)
	}
	cdn := newFakeCDN(t, body)

	// 正确 key：探针成功，关键帧、时长、编码签名都拿得到。
	info, err := probe(context.Background(), stream.Direct{URL: cdn.url(), Key: testKey}, "")
	if err != nil {
		t.Fatalf("正确 key 的探测失败: %v", err)
	}
	if len(info.keyframes) == 0 || info.durationSec < 11 || info.codecSig == "" {
		t.Fatalf("探测结果不完整：keyframes=%v duration=%v sig=%q",
			info.keyframes, info.durationSec, info.codecSig)
	}

	// 错 key：必须报错（包 httpx.ErrUpstream → 502），不能返回一份「成功但空」的探测结果。
	wrong := bytes.Repeat([]byte{0x5a}, 16)
	if _, err := probe(context.Background(), stream.Direct{URL: cdn.url(), Key: wrong}, ""); err == nil {
		t.Fatal("错 key 的探测返回成功：空索引会被当成合法索引写进库")
	} else if !errors.Is(err, httpx.ErrUpstream) {
		t.Fatalf("err = %v, 期望包 httpx.ErrUpstream（边界映射 502）", err)
	}

	// 首播整条链路：错 key → 502，且 segment 表与分片缓存都不留任何东西。
	r := newRig(t, "4")
	ep := r.addEpisode(t, 1, 12)
	enc2Path := filepath.Join(t.TempDir(), "enc2.mp4")
	makeEncryptedVideo(t, enc2Path, 12, "160x120", testKey)
	enc2, err := os.ReadFile(enc2Path)
	if err != nil {
		t.Fatalf("读取密文样本: %v", err)
	}
	r.cdn[ep.ID].setBody(enc2)
	r.keys[ep.ID] = wrong
	uid := "e" + strconv.FormatInt(ep.ID, 10)
	if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusBadGateway {
		t.Fatalf("错 key 首播状态码 = %d, 期望 502（body=%s）", rec.Code, rec.Body.String())
	}
	ctx := context.Background()
	if segs, err := r.st.ListSegments(ctx, ep.ID); err != nil || len(segs) != 0 {
		t.Fatalf("错 key 后 segment 行 = (%v, %v), 期望空", segs, err)
	}
	if arts, err := r.st.ListArtifactsOldest(ctx, store.ArtifactKindSegment, 10); err != nil || len(arts) != 0 {
		t.Fatalf("错 key 后分片缓存行 = (%v, %v), 期望空", arts, err)
	}
}

// TestChannelChangesSig 覆盖 AC「同一集在 web 与 app 两个通道下 codec_sig 不同 →
// 缓存不串味、索引不沿用」与「旧签名行被替换，不产生孤儿缓存」。
func TestChannelChangesSig(t *testing.T) {
	t.Run("同一编码签名的两个通道给出不同分片键", func(t *testing.T) {
		// 签名只有 codec/profile/level/pix_fmt/tag/time_base：不含分辨率与时长，
		// 所以两个通道可能算出同一串 —— 通道段必须由播放面补上。
		const sig = "h264/High/40/yuv420p/avc1/1/12800"
		web, app := channelSig(channelWeb, sig), channelSig(channelApp, sig)
		if web == app {
			t.Fatalf("两个通道拼出同一串签名 %q", web)
		}
		if sigChannel(web) != channelWeb || sigChannel(app) != channelApp {
			t.Fatalf("通道段取不出来：web=%q app=%q", sigChannel(web), sigChannel(app))
		}
		if got := sigChannel(sig); got != "" {
			t.Fatalf("没有通道段的老签名判成 %q, 期望空串（一律当失效）", got)
		}
		// 分片键消费的就是这个签名：两通道的键必须不同（否则缓存串味）。
		if cache.SegmentKey("e1", 0, web) == cache.SegmentKey("e1", 0, app) {
			t.Fatal("两个通道算出同一个分片键")
		}
		// 索引失效判据：老签名、别的通道都要重建；同通道不重建（否则每播一次就重建索引）。
		if !indexStale([]store.Segment{{CodecSig: sig}}, channelWeb) {
			t.Error("没有通道段的老索引没被判失效")
		}
		if !indexStale([]store.Segment{{CodecSig: web}}, channelApp) {
			t.Error("web 通道的索引在 app 请求下没被判失效")
		}
		if indexStale([]store.Segment{{CodecSig: web}, {CodecSig: web}}, channelWeb) {
			t.Error("同通道的索引被判失效（会反复重建）")
		}
	})

	t.Run("换通道后索引重建、分片不串味", func(t *testing.T) {
		r := newRig(t, "4")
		ep := r.addEpisode(t, 1, 12) // web 明文 12 秒 → 3 片
		uid := "e" + strconv.FormatInt(ep.ID, 10)
		id := r.id(uid)
		ctx := context.Background()

		// 1) web 通道首播：按需建索引，签名带 web 段。
		if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
			t.Fatalf("web 首播状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
		}
		webSegs, err := r.st.ListSegments(ctx, ep.ID)
		if err != nil {
			t.Fatalf("ListSegments: %v", err)
		}
		if len(webSegs) != 3 {
			t.Fatalf("web 索引片数 = %d, 期望 3", len(webSegs))
		}
		for i, s := range webSegs {
			if got := sigChannel(s.CodecSig); got != channelWeb {
				t.Fatalf("web 索引第 %d 片通道 = %q, 期望 %q（sig=%q）", i, got, channelWeb, s.CodecSig)
			}
		}
		webSig := webSegs[0].CodecSig
		webKey := cache.SegmentKey(uid, 0, webSig)

		// 播一片，让 web 通道的分片真的落进缓存。
		webRec := r.do(t, http.MethodGet, "/video/"+id+"/0.ts")
		if webRec.Code != http.StatusOK {
			t.Fatalf("web 分片状态码 = %d, 期望 200（body=%s）", webRec.Code, webRec.Body.String())
		}

		// 2) 同一集换成 App 通道：密文 20 秒、分辨率也不同（字节流不同，签名却可能相同）。
		encPath := filepath.Join(t.TempDir(), "enc.mp4")
		makeEncryptedVideo(t, encPath, 20, "176x144", testKey)
		encBody, err := os.ReadFile(encPath)
		if err != nil {
			t.Fatalf("读取密文样本: %v", err)
		}
		r.cdn[ep.ID].setBody(encBody)
		r.keys[ep.ID] = testKey

		// 3) playlist 现解析直链 → 判出 app 通道 → 重建索引：片数按新源算（20s/4s = 5）。
		if rec := r.do(t, http.MethodGet, "/video/"+id+".m3u8"); rec.Code != http.StatusOK {
			t.Fatalf("app 首播状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
		}
		appSegs, err := r.st.ListSegments(ctx, ep.ID)
		if err != nil {
			t.Fatalf("ListSegments: %v", err)
		}
		if len(appSegs) != 5 {
			t.Fatalf("换通道后片数 = %d, 期望 5（20s / 目标 4s；沿用旧索引会是 3）", len(appSegs))
		}
		for i, s := range appSegs {
			if got := sigChannel(s.CodecSig); got != channelApp {
				t.Fatalf("app 索引第 %d 片通道 = %q, 期望 %q（sig=%q）", i, got, channelApp, s.CodecSig)
			}
			if s.CodecSig == webSig {
				t.Fatalf("app 索引沿用了 web 的签名 %q", webSig)
			}
		}
		if appKey := cache.SegmentKey(uid, 0, appSegs[0].CodecSig); appKey == webKey {
			t.Fatal("app 通道分片算出与 web 相同的缓存键（会命中旧预览分片）")
		}

		// 4) web 索引下不存在的第 5 片，app 索引下可切 —— 索引真的换过了。
		if rec := r.do(t, http.MethodGet, "/video/"+id+"/4.ts"); rec.Code != http.StatusOK {
			t.Fatalf("app 第 4 片状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
		}
		// 5) 同一 seq 取到的是 app 通道的内容，不是缓存里 web 通道的那份。
		appRec := r.do(t, http.MethodGet, "/video/"+id+"/0.ts")
		if appRec.Code != http.StatusOK {
			t.Fatalf("app 分片状态码 = %d, 期望 200（body=%s）", appRec.Code, appRec.Body.String())
		}
		if bytes.Equal(appRec.Body.Bytes(), webRec.Body.Bytes()) {
			t.Fatal("换通道后分片返回了 web 通道的缓存内容（串味）")
		}
	})

	t.Run("老索引被替换且不留孤儿缓存", func(t *testing.T) {
		r := newRig(t, "4")
		ep := r.addEpisode(t, 1, 12)
		uid := "e" + strconv.FormatInt(ep.ID, 10)
		ctx := context.Background()

		// 本任务之前写下的索引行：codec_sig 没有通道段，切点也是旧口径（整片 10 秒）。
		if err := r.st.ReplaceSegments(ctx, ep.ID, []store.Segment{
			{EpisodeID: ep.ID, StartSec: 0, Dur: 10, Keyframe: true, Mode: "copy", CodecSig: "h264/High/40"},
		}); err != nil {
			t.Fatalf("写入老索引: %v", err)
		}

		if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+".m3u8"); rec.Code != http.StatusOK {
			t.Fatalf("老索引下首播状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
		}
		segs, err := r.st.ListSegments(ctx, ep.ID)
		if err != nil {
			t.Fatalf("ListSegments: %v", err)
		}
		if len(segs) != 3 {
			t.Fatalf("重建后片数 = %d, 期望 3（老索引只有 1 片，被沿用说明没重建）", len(segs))
		}
		for i, s := range segs {
			if got := sigChannel(s.CodecSig); got != channelWeb {
				t.Errorf("重建后第 %d 片通道 = %q, 期望 %q", i, got, channelWeb)
			}
			if s.Dur == 10 {
				t.Errorf("重建后第 %d 片还是老切点（Dur=10）", i)
			}
		}
		// 重建后的实际行（-test.v 可见）：AC「旧签名行被替换」的证据。
		for _, s := range segs {
			t.Logf("segment 行: start=%.1f dur=%.1f sig=%s", s.StartSec, s.Dur, s.CodecSig)
		}
		// 不产生孤儿缓存：落盘的分片行只对应现存索引里的键。
		if rec := r.do(t, http.MethodGet, "/video/"+r.id(uid)+"/0.ts"); rec.Code != http.StatusOK {
			t.Fatalf("分片状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
		}
		reachable := map[string]bool{}
		segs, _ = r.st.ListSegments(ctx, ep.ID)
		for _, s := range segs {
			reachable[cache.SegmentKey(uid, 0, s.CodecSig)] = true
		}
		arts, err := r.st.ListArtifactsOldest(ctx, store.ArtifactKindSegment, 100)
		if err != nil {
			t.Fatalf("ListArtifactsOldest: %v", err)
		}
		if len(arts) != 1 {
			t.Fatalf("分片缓存行数 = %d, 期望 1（只切了第 0 片）", len(arts))
		}
		for _, a := range arts {
			if !reachable[a.Key] {
				t.Errorf("孤儿分片缓存：键 %q 不在现存索引的分片键里", a.Key)
			}
			// 缓存行的实际内容（-test.v 可见）：AC「不产生孤儿缓存」的证据。
			t.Logf("artifact 行: kind=%s key=%s path=%s bytes=%d", a.Kind, a.Key, a.Path, a.Bytes)
		}
	})
}

// SPDX-License-Identifier: GPL-3.0-only
// 向量来自 jackotom/hongguo-drama 参考实现（固定 Math.random / Date.now 后跑出来的
// 逐字节结果），见 MKACG-676「单测：各签名函数固定输入→固定输出向量」。

package sign

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedRand 按参考实现里的取值序列返回 32 位随机数：
// JS 是 Math.floor(x * 0x100000000)，x 由 printf 序列给出。
type fixedRand struct {
	values []float64
	index  int
}

func newFixedRand(seeds ...int) *fixedRand {
	r := &fixedRand{}
	for _, v := range seeds {
		r.values = append(r.values, float64(v%100000)/100000)
	}
	return r
}

func (r *fixedRand) Float64() float64 {
	v := r.values[r.index%len(r.values)]
	r.index++
	return v
}

// testSigner 造一个确定性的签名器：随机序列、nonce、启动时刻、当前时刻全固定。
func testSigner(seeds ...int) *Signer {
	return &Signer{
		Rand:    newFixedRand(seeds...),
		Nonce:   "FIXEDNONCE123456",
		Started: 1700000000123,
		Now:     func() time.Time { return time.UnixMilli(1700000000123) },
	}
}

func TestSM3KnownVector(t *testing.T) {
	// GB/T 32905 标准向量：SM3("abc")。
	got := hex.EncodeToString(SM3([]byte("abc")))
	const want = "66c7f0f462eeedd9d1f2d46bdc10e4e24167c4875cf2f7a2297da02b8f4ba8e0"
	if got != want {
		t.Fatalf("SM3(abc) = %s, want %s", got, want)
	}
}

const (
	vectorQuery   = "ac=wifi&aid=8662&ts=1700000000&_rticket=1700000000000"
	vectorBase    = "https://api5-normal-sinfonlineb.fqnovel.com/novel/player/multi_video_model/v1/"
	vectorKhronos = uint32(1700000000)
	vectorBodyHex = "7b2262697a5f706172616d223a7b226465766963655f6c6576656c223a332c226e6565645f616c6c5f766964656f5f646566696e6974696f6e223a747275652c22766964656f5f706c6174666f726d223a313032347d2c226d697865645f766964656f5f69645f6d6170223a7b2231303034223a5b2237363130363039343933373334333239333638225d7d7d"
)

func vectorBody(t *testing.T) []byte {
	t.Helper()
	body, err := hex.DecodeString(vectorBodyHex)
	if err != nil {
		t.Fatalf("解码向量 body: %v", err)
	}
	return body
}

func vectorIdentity() Identity {
	return Identity{
		DeviceID:    "1234567890123456789",
		IID:         "9876543210987654321",
		CDID:        "0f0e0d0c-0b0a-4090-8070-605040302010",
		VersionName: "7.1.3.32",
	}
}

func TestXGorgonVector(t *testing.T) {
	got := xGorgon(vectorQuery, vectorBody(t), vectorKhronos, 4660)
	const want = "84043412400122fd4ae0856d43bb1364e71df6105a46126993ae"
	if got != want {
		t.Fatalf("x-gorgon = %s, want %s", got, want)
	}
}

func TestXGorgonEmptyBodyDiffers(t *testing.T) {
	// GET 形态（无 body）走全零摘要，必须与有 body 的结果不同：
	// 否则签名覆盖不到 body，上游会拒。
	withBody := xGorgon(vectorQuery, vectorBody(t), vectorKhronos, 4660)
	without := xGorgon(vectorQuery, nil, vectorKhronos, 4660)
	if withBody == without {
		t.Fatal("有无 body 的 x-gorgon 相同，签名没有覆盖 body")
	}
}

func TestBranchOfVector(t *testing.T) {
	url := vectorBase + "?" + vectorQuery
	if got := branchOf(url, vectorBody(t), vectorKhronos); got != 2 {
		t.Fatalf("branchOf = %d, want 2", got)
	}
}

func TestHeliosVector(t *testing.T) {
	got := testSigner(111).helios(vectorKhronos)
	const want = "tb5IABcx8cbGav1xbJu9ywVEIgGzGoaKU1DzZbhcWfCIUIyP"
	if got != want {
		t.Fatalf("x-helios = %s, want %s", got, want)
	}
}

func TestMedusaVector(t *testing.T) {
	url := vectorBase + "?" + vectorQuery
	s := testSigner(222, 333, 444, 555, 666)
	got := s.medusa(url, vectorBody(t), vectorKhronos, vectorIdentity())
	want := "A/FTZfcZDJ/XJo9e1tubFVc+Mn2MuQABIDVCfLQJAQaFGCGew+z9RdjPCRZ5M9gZGiAouAGEgKEe0ApViFq9AcNG8XET7hYDIe3ArDUUIo7Q/Smuszehbkq6V5SNLNmkwdhnqg/6JKZ3vmNbGDhZLiR7SGXXygGpu0P1aHa+W3msAtHY5RZKfgw3I8OP80+CxjEBsYI41Bd1r+nfwuvTAbB2EBf8Xudhapy1pyDLzyUSoh1vtDdmKcITla+W1Undn1IPw3dXeripbWFMWUskVqCs5qrdPcYnaDYWtEcD9kY8qfTy/yEjPfLkiGJEe0Tg3f8EecZtOQmGXWT2+45tU8Qqjo+ZvYts6pvRrWaR3yvOKSkI6xq2zRLHLgPysKxVVVTUZxQAdCIicY+CkF1h2veJAy8b+nKwn5xIjfIhy7p82QN2bJC9V0vMNd2SGnpd9aTGP5/AtH4z6n1XIAL/btIMSDJxXnzgJLI5NT6ni9f5MbBPdsQyg4D0fN+6mknIAPxegOLU47V0NAmx4v4vS2jKmd7eAJkwkiiFD29zLrzUpuxSPtFlb2WpYNz6x1LsxsbeIf0lEdN4DEcuHI7rc2ikb36ecuv65/k+P0XyHyiNcNZebfx4tZH25UHNmERYclKmQ5WYdCGR7tSE8WZX0iOiYPXuIUA5jWa0Hx3k93AJbLVJy48sKvn3P3kF73dH16vrTgfoyDEdjzLIVehlR/TTNgqDV0LH6tNcNw9rxGzTbZttKgRLSSf/4ZGZMLOa1bFZKuSa0VeE2Eid+b65h7LIWKtE6Zhpj73wDPyc4M4DEwMjotxMMnOs9jfTzOth7xEYAzbNIH9HNj4bWmxn9d98c40vxy+BoNyGhN7WHsXMC1NCJVOsp4OnSA1Fr6NLeNoG7uWLcWvTH4vP5J1yv9GFSuPeQk0/U/8cA/qLZYbvo/ilrCofckaav2bNFcXy45je9zg14rKTXEVeku3156eW//ynlv68awE="
	if got != want {
		t.Fatalf("x-medusa 不匹配\n got=%s\nwant=%s", got, want)
	}
}

func TestEncodeQueryVector(t *testing.T) {
	got := encodeQuery([]KV{
		{"a", "1 2"},
		{"b", "x!y'z(w)v*~"},
		{"c", "中"},
	})
	// 与参考实现 pythonUrlEncode 一致：!/'/(/) 也编码，* 与 ~ 原样，空格是 %20。
	const want = "a=1%202&b=x%21y%27z%28w%29v*~&c=%E4%B8%AD"
	if got != want {
		t.Fatalf("encodeQuery = %s, want %s", got, want)
	}
}

func TestPlaybackSignsWhatItSends(t *testing.T) {
	s := testSigner(111, 222, 333, 444, 555, 666)
	body := []byte(`{"biz_param":{"device_level":3},"mixed_video_id_map":{"1004":["123"]}}`)
	params := []KV{{"ac", "wifi"}, {"aid", "8662"}, {"version_name", "7.1.3.32"}}

	req, err := s.Playback(vectorBase, params, vectorIdentity(), body, "com.phoenix.read/71532 test")
	if err != nil {
		t.Fatalf("Playback: %v", err)
	}
	if !strings.HasPrefix(req.URL, vectorBase+"?") {
		t.Fatalf("URL 前缀不符: %s", req.URL)
	}
	// AC：签名覆盖 query，落点必须是分支 0。
	if got := branchOf(req.URL, body, vectorKhronos); got != 0 {
		t.Fatalf("签名落点 branchOf=%d，want 0（URL=%s）", got, req.URL)
	}
	for _, name := range []string{
		"x-gorgon", "x-ladon", "x-argus", "x-helios", "x-medusa",
		"x-khronos", "x-ss-req-ticket", "sdk-version", "x-xs-from-web",
		"x-tt-store-region", "User-Agent", "Content-Type",
	} {
		if req.Headers[name] == "" {
			t.Fatalf("缺少签名头 %s", name)
		}
	}
	if req.Headers["x-khronos"] != "1700000000" {
		t.Fatalf("x-khronos = %s, want 1700000000", req.Headers["x-khronos"])
	}
	// ladon/argus 是 khronos 的大端 / 小端 base64（参考向量）。
	if req.Headers["x-ladon"] != "ZVPxAA==" || req.Headers["x-argus"] != "APFTZQ==" {
		t.Fatalf("ladon/argus = %s / %s", req.Headers["x-ladon"], req.Headers["x-argus"])
	}
	query := queryOf(req.URL)
	for _, want := range []string{"ts=1700000000", "device_id=1234567890123456789", "iid=9876543210987654321"} {
		if !strings.Contains(query, want) {
			t.Fatalf("查询串缺少 %s：%s", want, query)
		}
	}
	if !strings.Contains(query, "_rticket=") {
		t.Fatalf("查询串缺少 _rticket：%s", query)
	}
	// x-ss-req-ticket 必须与 query 里的 _rticket 一致。
	ticket := strings.SplitN(query, "_rticket=", 2)[1]
	if req.Headers["x-ss-req-ticket"] != ticket {
		t.Fatalf("x-ss-req-ticket=%s 与 query 的 _rticket=%s 不一致",
			req.Headers["x-ss-req-ticket"], ticket)
	}
}

func TestPlaybackRetriesUntilBranchZero(t *testing.T) {
	// 假分支判定：前两次不命中，第三次命中。真实散列不可控，这里用窄缝验证重试。
	s := testSigner(1, 2, 3, 4, 5, 6)
	calls := 0
	s.branch = func(string, []byte, uint32) int {
		calls++
		if calls < 3 {
			return 1
		}
		return 0
	}
	req, err := s.Playback(vectorBase, []KV{{"aid", "8662"}}, vectorIdentity(), []byte("{}"), "ua")
	if err != nil {
		t.Fatalf("Playback: %v", err)
	}
	if calls != 3 {
		t.Fatalf("重试次数 = %d, want 3", calls)
	}
	// ticket 随重试递增：第 3 次是 base+2。
	query := queryOf(req.URL)
	wantTicket := "1700000000125"
	if !strings.Contains(query, "_rticket="+wantTicket) {
		t.Fatalf("_rticket 未随重试递增：%s", query)
	}
}

func TestPlaybackFailsWhenBranchNeverHits(t *testing.T) {
	s := testSigner(1)
	s.branch = func(string, []byte, uint32) int { return 1 }
	_, err := s.Playback(vectorBase, []KV{{"aid", "8662"}}, vectorIdentity(), []byte("{}"), "ua")
	if !errors.Is(err, ErrBranchTimeout) {
		t.Fatalf("err = %v, want ErrBranchTimeout（不得静默回退）", err)
	}
}

func TestRequestBodyMarshalMatchesVector(t *testing.T) {
	// 参考实现的 body 序列化顺序（biz_param 在前、mixed_video_id_map 在后）；
	// 调用方必须保持同一份字节，这里只是把「哪些字节」钉死。
	body := map[string]any{
		"biz_param": map[string]any{
			"device_level":              3,
			"need_all_video_definition": true,
			"video_platform":            1024,
		},
		"mixed_video_id_map": map[string][]string{"1004": {"7610609493734329368"}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if hex.EncodeToString(raw) != vectorBodyHex {
		t.Fatalf("body 字节与向量不一致:\n got=%s", hex.EncodeToString(raw))
	}
}

// TestPlaybackConcurrent 是并发安全的回归用例：Signer 是进程级单例，
// Playback 必须能被多个播放请求同时调用（`go test -race` 下不得报数据竞争）。
func TestPlaybackConcurrent(t *testing.T) {
	s := NewSigner()
	ident := Identity{DeviceID: "7001", IID: "7002", CDID: "7003"}
	body := []byte(`{"biz_param":{"device_level":3}}`)

	const workers, rounds = 8, 40
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				req, err := s.Playback("https://example.com/v1/", []KV{{"aid", "8662"}}, ident, body, "UA")
				if err != nil {
					errs <- err
					return
				}
				if req.URL == "" || req.Headers["x-gorgon"] == "" || req.Headers["x-medusa"] == "" {
					errs <- errors.New("并发 Playback 产出缺签名头或空 URL")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发 Playback: %v", err)
	}
}

package segment

import (
	"context"
	"math"
	"strings"
	"testing"
)

// TestParseSeqFile 覆盖分片文件名解析：只认 `<纯十进制数字>.m4s`。
func TestParseSeqFile(t *testing.T) {
	ok := map[string]int{"0.m4s": 0, "1.m4s": 1, "000123.m4s": 123, "42.m4s": 42}
	for name, want := range ok {
		got, valid := parseSeqFile(name)
		if !valid || got != want {
			t.Errorf("parseSeqFile(%q) = (%d, %v), 期望 (%d, true)", name, got, valid, want)
		}
	}
	bad := []string{"", "1", ".m4s", "x.m4s", "-1.m4s", "1.m3u8", "1.m4s.bak", "1.5.m4s", "+1.m4s", "0.ts"}
	for _, name := range bad {
		if got, valid := parseSeqFile(name); valid {
			t.Errorf("parseSeqFile(%q) = (%d, true), 期望 false", name, got)
		}
	}
}

// TestEpisodeUID 锁定单集播放身份形态：`e<id>`（DESIGN D3）。合法性判定已统一到
// store.PlaybackEpisodes（docs/adr/0014），本包只负责把集 id 拼成缓存键里的身份。
func TestEpisodeUID(t *testing.T) {
	cases := map[int64]string{
		0:                "e0",
		1:                "e1",
		42:               "e42",
		9007199254740993: "e9007199254740993",
	}
	for id, want := range cases {
		if got := episodeUID(id); got != want {
			t.Errorf("episodeUID(%d) = %q, 期望 %q", id, got, want)
		}
	}
}

// TestSafeHost 锁定「日志不落完整直链」：只保留 host。
func TestSafeHost(t *testing.T) {
	cases := map[string]string{
		"https://cdn.example.com/video/public/v1/360p.mp4?sign=SECRET": "cdn.example.com",
		"http://127.0.0.1:8080/a/b":                                    "127.0.0.1:8080",
		"/local/path.mp4":                                              "/local/path.mp4",
		"https://host":                                                 "host",
	}
	for in, want := range cases {
		if got := safeHost(in); got != want {
			t.Errorf("safeHost(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestFFmpegSlots 锁定并发闸上限：min(4, max(1, NumCPU/2))。
func TestFFmpegSlots(t *testing.T) {
	n := ffmpegSlots()
	if n < 1 || n > 4 {
		t.Fatalf("ffmpegSlots() = %d, 期望落在 [1,4]", n)
	}
}

// TestCutter_Gate 覆盖并发闸语义：占满后在 acquire 处排队，释放后放行，
// ctx 取消时放弃排队（不泄漏、不丢弃）。
func TestCutter_Gate(t *testing.T) {
	c := &Cutter{gate: newFFmpegGate(1)}
	c.acquire(t.Context())
	// 闸已占满：第二次 acquire 应排队并因子 ctx 取消而放弃（不阻塞、不丢弃）。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.acquire(ctx); err == nil {
		t.Fatal("闸占满时 acquire 应排队并因 ctx 取消而返回错误")
	}
	c.release()
	if err := c.acquire(t.Context()); err != nil {
		t.Fatalf("释放后 acquire 失败: %v", err)
	}
	c.release()
}

func closeEnough(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// TestFailureClass 锁定「客户端只看得到稳定类别」：stderr 里的直链、签名参数、
// 上游措辞一个字符都不进返回值（MKACG-639）。403 / Forbidden 单列成 upstream
// rejected —— 它对应「换直链/换身份」而不是「换源」。
func TestFailureClass(t *testing.T) {
	cases := []struct {
		name     string
		fallback string
		stderr   string
		want     string
	}{
		{"ffprobe 撞 403 带签名直链", classProbeFailed,
			"http://cdn.example.com/v.mp4?n80_tk=SECRET: Server returned 403 Forbidden (access denied)", classRejected},
		{"ffmpeg 撞 403", classRemuxFailed,
			"Error opening input file http://cdn.example.com/v.mp4?n80_tk=SECRET.\nError opening input files: Server returned 403 Forbidden (access denied)", classRejected},
		{"坏源", classProbeFailed, "Invalid data found when processing input", classProbeFailed},
		{"空 stderr 用调用方的默认类别", classRemuxFailed, "", classRemuxFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := failureClass(tc.fallback, []byte(tc.stderr))
			if got != tc.want {
				t.Errorf("failureClass = %q, 期望 %q", got, tc.want)
			}
			for _, bad := range []string{"SECRET", "n80_tk", "http", "?", "&", "Server returned"} {
				if strings.Contains(got, bad) {
					t.Errorf("类别里漏出了 stderr 片段 %q：%q", bad, got)
				}
			}
		})
	}
}

// TestLoggableStderr 锁定「stderr 只在确实不含地址/查询串时进日志」：被 200 字节
// 截断后可能只剩查询串中段（`6110…&n80=1&n80_tk=…`），那也必须整条丢弃 ——
// 日志禁止记带签名的播放直链（docs/DEV-STANDARDS.md「日志」）。
func TestLoggableStderr(t *testing.T) {
	cases := []struct{ name, tail, want string }{
		{"空", "", ""},
		{"只有措辞", "Invalid data found when processing input", "Invalid data found when processing input"},
		{"解密自检", "non-existing PPS 0 referenced", "non-existing PPS 0 referenced"},
		{"完整直链", "http://cdn.example.com/v.mp4: Server returned 403 Forbidden", withheldDetail},
		{"截断后只剩查询串中段", "6110116410888CE9&n80=1&n80_tk=SECRET: Server returned 403", withheldDetail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := loggableStderr(tc.tail)
			if got != tc.want {
				t.Errorf("loggableStderr(%q) = %q, 期望 %q", tc.tail, got, tc.want)
			}
			if strings.Contains(got, "SECRET") || strings.Contains(got, "n80_tk") || strings.Contains(got, "://") {
				t.Errorf("日志备注里漏出了直链片段：%q", got)
			}
		})
	}
}

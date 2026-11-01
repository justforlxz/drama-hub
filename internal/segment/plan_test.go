package segment

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/store"
)

// TestPlanSegments 覆盖 M6 的切点规则（纯函数，不碰磁盘/ffmpeg）。
//
// 关键约束：切点必须对齐关键帧（否则 `-c copy` 会漂移）；总时长必须等于源时长。
func TestPlanSegments(t *testing.T) {
	cases := []struct {
		name      string
		keyframes []float64
		duration  float64
		target    float64
		want      [][2]float64 // [起点, 时长]
	}{
		{
			name:      "每 1s 一个关键帧，目标 4s",
			keyframes: []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
			duration:  12,
			target:    4,
			want:      [][2]float64{{0, 4}, {4, 4}, {8, 4}},
		},
		{
			// DESIGN §2 实测「目标 10s 实际 10.66s」就是这条规则：切点取第一个
			// `>= 起点+目标` 的关键帧，因此片长略大于目标而不是任意值。
			name:      "目标落在关键帧之间：取第一个 ≥ 目标的切点",
			keyframes: []float64{0, 3.7, 7.4, 11.1},
			duration:  15,
			target:    4,
			want:      [][2]float64{{0, 7.4}, {7.4, 7.6}},
		},
		{
			// 对齐关键帧的固有代价：关键帧间隔略小于目标时，跳过下一个切点会让
			// 片长接近两倍目标。这是「不漂移」换来的，由 EXTINF 如实反映。
			name:      "关键帧间隔略小于目标：片长接近两倍目标（对齐代价）",
			keyframes: []float64{0, 3.9, 7.8, 11.7},
			duration:  11.7,
			target:    4,
			want:      [][2]float64{{0, 7.8}, {7.8, 3.9}},
		},
		{
			name:      "末片短于目标：真实尾部保留",
			keyframes: []float64{0, 4, 8, 10},
			duration:  10.5,
			target:    4,
			want:      [][2]float64{{0, 4}, {4, 4}, {8, 2.5}},
		},
		{
			name:      "首片：源首个关键帧不在 0 时仍从 0 起，整集覆盖",
			keyframes: []float64{0.04, 2.04, 4.04},
			duration:  4.5,
			target:    2,
			want:      [][2]float64{{0, 2.04}, {2.04, 2.0}, {4.04, 0.46}},
		},
		{
			name:      "关键帧间隔全部大于目标：每片就是相邻两个关键帧",
			keyframes: []float64{0, 5, 10},
			duration:  12,
			target:    4,
			want:      [][2]float64{{0, 5}, {5, 5}, {10, 2}},
		},
		{
			name:      "无关键帧信息：退化为整段一片",
			keyframes: nil,
			duration:  9.5,
			target:    4,
			want:      [][2]float64{{0, 9.5}},
		},
		{
			name:      "源短于目标：单片",
			keyframes: []float64{0},
			duration:  3,
			target:    10,
			want:      [][2]float64{{0, 3}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planSegments(sourceInfo{
				keyframes: tc.keyframes, durationSec: tc.duration, codecSig: "sig",
			}, 7, tc.target)

			if len(got) != len(tc.want) {
				t.Fatalf("分片数 = %d, 期望 %d（got=%v）", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				if !closeEnough(got[i].StartSec, want[0]) || !closeEnough(got[i].Dur, want[1]) {
					t.Errorf("片 %d = (%v, %v), 期望 (%v, %v)", i, got[i].StartSec, got[i].Dur, want[0], want[1])
				}
				if got[i].EpisodeID != 7 {
					t.Errorf("片 %d episode_id = %d, 期望 7", i, got[i].EpisodeID)
				}
				if !got[i].Keyframe || got[i].Mode != "copy" {
					t.Errorf("片 %d 应标为关键帧 copy，实际 keyframe=%v mode=%q", i, got[i].Keyframe, got[i].Mode)
				}
			}

			// 不变量 1：Σ Dur == 源时长（±浮点）。
			sum := 0.0
			for _, s := range got {
				sum += s.Dur
			}
			if math.Abs(sum-tc.duration) > 1e-6 {
				t.Errorf("ΣDur = %v, 期望 %v", sum, tc.duration)
			}
			// 不变量 2：首片从 0 起、片与片无缝衔接、末片收到源时长。
			if len(got) > 0 {
				if got[0].StartSec != 0 {
					t.Errorf("首片起点 = %v, 期望 0", got[0].StartSec)
				}
				for i := 1; i < len(got); i++ {
					if !closeEnough(got[i-1].StartSec+got[i-1].Dur, got[i].StartSec) {
						t.Errorf("片 %d 与片 %d 之间有缝：%v != %v", i-1, i, got[i-1].StartSec+got[i-1].Dur, got[i].StartSec)
					}
				}
				last := got[len(got)-1]
				if !closeEnough(last.StartSec+last.Dur, tc.duration) {
					t.Errorf("末片结束 = %v, 期望源时长 %v", last.StartSec+last.Dur, tc.duration)
				}
			}
		})
	}
}

// TestPlanSegments_CutsAreKeyframes 锁定核心约束：除首片的 0 起点外，每个切点
// 都必须是探测到的关键帧之一（否则 `-c copy` 会在片内漂移）。
func TestPlanSegments_CutsAreKeyframes(t *testing.T) {
	kfs := []float64{0, 3.7, 7.4, 11.1, 14.8}
	got := planSegments(sourceInfo{keyframes: kfs, durationSec: 18}, 1, 4)
	for i, s := range got {
		if i == 0 {
			continue // 首片固定从 0 起（见 planSegments 注释）
		}
		found := false
		for _, kf := range kfs {
			if closeEnough(kf, s.StartSec) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("片 %d 的切点 %v 不是关键帧（关键帧=%v）", i, s.StartSec, kfs)
		}
	}
}

// TestTimeline 覆盖 seq → (集号, 集内秒) 映射与首片/末片/跨集边界。
func TestTimeline(t *testing.T) {
	// 两集：第 1 集 3 片、第 2 集 2 片，seq 连续（0..4）。
	tl := NewTimeline([]EpisodeSegments{
		{EpisodeID: 11, EpNo: 1, Segments: []store.Segment{
			{StartSec: 0, Dur: 4}, {StartSec: 4, Dur: 4}, {StartSec: 8, Dur: 2},
		}},
		{EpisodeID: 22, EpNo: 2, Segments: []store.Segment{
			{StartSec: 0, Dur: 5}, {StartSec: 5, Dur: 3},
		}},
	})

	if tl.Len() != 5 {
		t.Fatalf("Len = %d, 期望 5", tl.Len())
	}
	// 总时长 = 各集之和（10 + 8）。
	if got := tl.Total(); !closeEnough(got, 18) {
		t.Errorf("Total = %v, 期望 18", got)
	}

	want := []Slice{
		{Seq: 0, EpisodeID: 11, EpNo: 1, StartSec: 0, Dur: 4, OffsetSec: 0}, // 首片
		{Seq: 1, EpisodeID: 11, EpNo: 1, StartSec: 4, Dur: 4, OffsetSec: 4},
		{Seq: 2, EpisodeID: 11, EpNo: 1, StartSec: 8, Dur: 2, OffsetSec: 8},  // 第 1 集末片
		{Seq: 3, EpisodeID: 22, EpNo: 2, StartSec: 0, Dur: 5, OffsetSec: 10}, // 跨集边界：第 2 集首片
		{Seq: 4, EpisodeID: 22, EpNo: 2, StartSec: 5, Dur: 3, OffsetSec: 15}, // 末片
	}
	for _, w := range want {
		got, ok := tl.At(w.Seq)
		if !ok {
			t.Fatalf("At(%d) 越界", w.Seq)
		}
		if got != w {
			t.Errorf("At(%d) = %+v, 期望 %+v", w.Seq, got, w)
		}
	}

	// 越界：负数与 Len。
	if _, ok := tl.At(-1); ok {
		t.Error("At(-1) 应越界")
	}
	if _, ok := tl.At(5); ok {
		t.Error("At(Len) 应越界")
	}
}

// TestTimeline_EmptyEpisode 锁定空集行为：无分片的集不在时间轴上占位（seq 连续、
// 不出现空洞）——由 M7 在编排层决定是否 503。
func TestTimeline_EmptyEpisode(t *testing.T) {
	tl := NewTimeline([]EpisodeSegments{
		{EpisodeID: 1, EpNo: 1, Segments: []store.Segment{{StartSec: 0, Dur: 2}}},
		{EpisodeID: 2, EpNo: 2}, // 索引未就绪
		{EpisodeID: 3, EpNo: 3, Segments: []store.Segment{{StartSec: 0, Dur: 3}}},
	})
	if tl.Len() != 2 {
		t.Fatalf("Len = %d, 期望 2", tl.Len())
	}
	if got, _ := tl.At(1); got.EpisodeID != 3 {
		t.Errorf("At(1).EpisodeID = %d, 期望 3（seq 连续，空集不占位）", got.EpisodeID)
	}
	if got := tl.Total(); !closeEnough(got, 5) {
		t.Errorf("Total = %v, 期望 5", got)
	}
}

// TestParseSeqFile 覆盖分片文件名解析。
func TestParseSeqFile(t *testing.T) {
	ok := map[string]int{"0.ts": 0, "1.ts": 1, "000123.ts": 123, "42.ts": 42}
	for name, want := range ok {
		got, valid := parseSeqFile(name)
		if !valid || got != want {
			t.Errorf("parseSeqFile(%q) = (%d, %v), 期望 (%d, true)", name, got, valid, want)
		}
	}
	bad := []string{"", "1", ".ts", "x.ts", "-1.ts", "1.m3u8", "1.ts.bak", "1.5.ts", "+1.ts"}
	for _, name := range bad {
		if got, valid := parseSeqFile(name); valid {
			t.Errorf("parseSeqFile(%q) = (%d, true), 期望 false", name, got)
		}
	}
}

// TestParseEpisodeUID 覆盖单集 uid 解析；合并集 uid 必须不匹配（DESIGN D3）。
func TestParseEpisodeUID(t *testing.T) {
	ok := map[string]int64{"e1": 1, "e42": 42, "e9007199254740993": 9007199254740993}
	for uid, want := range ok {
		got, valid := parseEpisodeUID(uid)
		if !valid || got != want {
			t.Errorf("parseEpisodeUID(%q) = (%d, %v), 期望 (%d, true)", uid, got, valid, want)
		}
	}
	bad := []string{"", "e", "42", "E42", "e1x", "e-1", "m1", "e99999999999999999999", "e 1"}
	for _, uid := range bad {
		if got, valid := parseEpisodeUID(uid); valid {
			t.Errorf("parseEpisodeUID(%q) = (%d, true), 期望 false", uid, got)
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
	c := &Cutter{gate: make(chan struct{}, 1)}
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
		{"ffmpeg 撞 403", classCutFailed,
			"Error opening input file http://cdn.example.com/v.mp4?n80_tk=SECRET.\nError opening input files: Server returned 403 Forbidden (access denied)", classRejected},
		{"坏源", classProbeFailed, "Invalid data found when processing input", classProbeFailed},
		{"空 stderr 用调用方的默认类别", classCutFailed, "", classCutFailed},
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

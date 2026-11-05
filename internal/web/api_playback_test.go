package web

// 后台播放详情页的数据面（MKACG-683）：GET /api/playback 的响应契约。
//
// 会话采集由 internal/playback 的单测与 cmd 的端到端覆盖；这里只钉住管理面
// 「空表怎么回、多条会话怎么排、汇总字段怎么算」。

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/justforlxz/drama-hub/internal/playback"
)

// TestAPI_PlaybackEmpty 覆盖 AC-3：没有播放时 sessions 是 [] 而不是 null，
// 汇总字段全 0，端点仍然 200。
func TestAPI_PlaybackEmpty(t *testing.T) {
	srv, _, _ := newTestServer(t)

	rec := do(t, srv, http.MethodGet, "/api/playback", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"sessions":[]`) {
		t.Errorf("空态响应体没有字面 %q：%s", `"sessions":[]`, rec.Body.String())
	}

	got := decode[playbackView](t, rec)
	if got.Streams != 0 || got.Clients != 0 || got.Bytes != 0 || len(got.Sessions) != 0 {
		t.Errorf("空态汇总 = streams:%d clients:%d bytes:%d sessions:%d, 期望全 0",
			got.Streams, got.Clients, got.Bytes, len(got.Sessions))
	}
	if got.Now == "" {
		t.Error("now 为空，期望 RFC3339 时间")
	}
}

// TestAPI_PlaybackSessions 覆盖 AC-2：两部剧各一条 + 同剧第二个客户端一条，
// 三条互不串；汇总按客户端去重、字节求和；列表按最近活跃降序。
func TestAPI_PlaybackSessions(t *testing.T) {
	srv, _, _ := newTestServer(t)

	// 假钟让「最近活跃」与 last_seen 可断言，不依赖 sleep。
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := base
	srv.playback = playback.NewTracker(func() time.Time { return clock })

	clA := playback.Client{IP: "10.0.0.1", UA: "player-A"}
	clB := playback.Client{IP: "10.0.0.1", UA: "player-B"}

	// 同一个客户端先后拉两部剧。
	srv.playback.Open(clA, playback.Session{
		UID: "m1_1_3", DramaID: 1, Title: "甲剧", Unit: "第 1–3 集",
		Slices: 3, TotalSec: 18,
	}, time.Minute)
	clock = base.Add(10 * time.Second)
	srv.playback.Open(clA, playback.Session{
		UID: "e9", DramaID: 2, Title: "乙剧", Unit: "第 3 集",
		Slices: 1, TotalSec: 6,
	}, time.Minute)
	clock = base.Add(20 * time.Second)
	// 同一部剧的第二个客户端（UA 不同 = 另一条流）。
	srv.playback.Open(clB, playback.Session{
		UID: "m1_1_3", DramaID: 1, Title: "甲剧", Unit: "第 1–3 集",
		Slices: 3, TotalSec: 18,
	}, time.Minute)
	clock = base.Add(30 * time.Second)
	srv.playback.Progress(clA, "m1_1_3", 2, 12, 3000, 250*time.Millisecond, time.Minute)

	rec := do(t, srv, http.MethodGet, "/api/playback", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	got := decode[playbackView](t, rec)

	if got.Streams != 3 {
		t.Errorf("streams = %d, 期望 3", got.Streams)
	}
	if got.Clients != 2 {
		t.Errorf("clients = %d, 期望 2（同一 IP 不同 UA 算两个客户端）", got.Clients)
	}
	if got.Bytes != 3000 {
		t.Errorf("bytes = %d, 期望 3000", got.Bytes)
	}
	if len(got.Sessions) != 3 {
		t.Fatalf("sessions = %d 条, 期望 3", len(got.Sessions))
	}

	first := got.Sessions[0]
	if first.UID != "m1_1_3" || first.ClientIP != "10.0.0.1" || first.UserAgent != "player-A" {
		t.Errorf("最近活跃的第一条 = %+v, 期望甲剧的 player-A", first)
	}
	if first.DramaID != 1 || first.Title != "甲剧" || first.Unit != "第 1–3 集" {
		t.Errorf("第一条剧目字段 = %+v, 期望甲剧/第 1–3 集", first)
	}
	if first.Seq != 2 || first.Slices != 3 || first.PositionSec != 12 || first.TotalSec != 18 {
		t.Errorf("第一条进度 = seq:%d slices:%d position:%d total:%d, 期望 2/3/12/18",
			first.Seq, first.Slices, first.PositionSec, first.TotalSec)
	}
	if first.BytesServed != 3000 {
		t.Errorf("第一条 bytes_served = %d, 期望 3000", first.BytesServed)
	}
	if first.TimeToFirstSegmentMS != 250 {
		t.Errorf("第一条 time_to_first_segment_ms = %d, 期望 250", first.TimeToFirstSegmentMS)
	}
	if first.LastSeen != base.Add(30*time.Second).Format(time.RFC3339) {
		t.Errorf("第一条 last_seen = %q, 期望 %q", first.LastSeen, base.Add(30*time.Second).Format(time.RFC3339))
	}

	// 第二活跃的是 clB 在 +20s 打开的甲剧（clA 拉乙剧发生在 +10s）。
	second := got.Sessions[1]
	if second.UID != "m1_1_3" || second.Title != "甲剧" || second.UserAgent != "player-B" {
		t.Errorf("第二条 = %+v, 期望甲剧的 player-B", second)
	}
	third := got.Sessions[2]
	if third.UID != "e9" || third.Title != "乙剧" || third.UserAgent != "player-A" {
		t.Errorf("第三条 = %+v, 期望乙剧的 player-A", third)
	}
	// 同一 uid 的两条会话各自累计自己的字节，不能互相串。
	if second.BytesServed != 0 || third.BytesServed != 0 {
		t.Errorf("未拉分片的会话 bytes_served = %d/%d, 期望 0", second.BytesServed, third.BytesServed)
	}
	if second.TimeToFirstSegmentMS != 0 {
		t.Errorf("未拉分片的会话 time_to_first_segment_ms = %d, 期望 0", second.TimeToFirstSegmentMS)
	}
}

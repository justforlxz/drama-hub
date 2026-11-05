package playback

import (
	"strconv"
	"testing"
	"time"
)

// fakeClock 是可控时钟：测试用 t.Step 推进，不 sleep。
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time       { return c.now }
func (c *fakeClock) Step(d time.Duration) { c.now = c.now.Add(d) }

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func TestTrackerOpenAndProgress(t *testing.T) {
	clock := newClock()
	tr := NewTracker(clock.Now)
	ttl := 120 * time.Second
	cl := Client{IP: "10.0.0.1", UA: "Emby/1.0"}

	tr.Open(cl, Session{UID: "u1", Title: "剧一", Unit: "第 11–20 集", DramaID: 7, Slices: 10, TotalSec: 600}, ttl)
	if got := tr.Snapshot(ttl); len(got.Sessions) != 1 || got.Sessions[0].Title != "剧一" {
		t.Fatalf("Open 后快照 = %+v，期望 1 条带剧名的会话", got)
	}

	clock.Step(5 * time.Second)
	tr.Progress(cl, "u1", 2, 120, 1000, 0, ttl)
	clock.Step(5 * time.Second)
	tr.Progress(cl, "u1", 3, 180, 2000, 0, ttl)

	got := tr.Snapshot(ttl)
	if len(got.Sessions) != 1 {
		t.Fatalf("Session 数 = %d，期望 1", len(got.Sessions))
	}
	s := got.Sessions[0]
	if s.Seq != 3 || s.PositionSec != 180 || s.BytesServed != 3000 {
		t.Fatalf("进度 = seq %d / pos %d / bytes %d，期望 3 / 180 / 3000", s.Seq, s.PositionSec, s.BytesServed)
	}
	if got.Bytes != 3000 || got.Clients != 1 {
		t.Fatalf("汇总 = bytes %d / clients %d，期望 3000 / 1", got.Bytes, got.Clients)
	}
	if !s.FirstSeen.Equal(clock.now.Add(-10*time.Second)) || !s.LastSeen.Equal(clock.now) {
		t.Fatalf("first/last = %v / %v，期望 %v / %v", s.FirstSeen, s.LastSeen, clock.now.Add(-10*time.Second), clock.now)
	}
}

// Open 续接不清进度：播放端重取 playlist（seek、重连）后仍在同一会话上继续。
func TestTrackerOpenKeepsProgress(t *testing.T) {
	clock := newClock()
	tr := NewTracker(clock.Now)
	ttl := 120 * time.Second
	cl := Client{IP: "10.0.0.1", UA: "UA"}

	tr.Open(cl, Session{UID: "u1", Title: "剧一"}, ttl)
	tr.Progress(cl, "u1", 4, 240, 500, 0, ttl)
	clock.Step(time.Second)
	tr.Open(cl, Session{UID: "u1", Title: "剧一改名", Unit: "第 1–10 集", Slices: 10, TotalSec: 600}, ttl)

	s := tr.Snapshot(ttl).Sessions[0]
	if s.Seq != 4 || s.BytesServed != 500 {
		t.Fatalf("续接后进度 = seq %d / bytes %d，期望保持 4 / 500", s.Seq, s.BytesServed)
	}
	if s.Title != "剧一改名" || s.Unit != "第 1–10 集" {
		t.Fatalf("续接后描述字段 = %q / %q，期望刷新为最新值", s.Title, s.Unit)
	}
}

// Progress 先到（分片早于 playlist）时建骨架，随后 Open 补齐描述字段。
func TestTrackerProgressSkeleton(t *testing.T) {
	clock := newClock()
	tr := NewTracker(clock.Now)
	ttl := 120 * time.Second
	cl := Client{IP: "10.0.0.2", UA: "UA"}

	tr.Progress(cl, "u9", 0, 0, 100, 0, ttl)
	s := tr.Snapshot(ttl).Sessions[0]
	if s.UID != "u9" || s.Title != "" || s.DramaID != 0 || s.BytesServed != 100 {
		t.Fatalf("骨架 = %+v，期望只有 uid 与字节", s)
	}

	tr.Open(cl, Session{UID: "u9", Title: "剧二", Unit: "第 3 集", DramaID: 8, Slices: 1}, ttl)
	s = tr.Snapshot(ttl).Sessions[0]
	if s.Title != "剧二" || s.DramaID != 8 || s.BytesServed != 100 {
		t.Fatalf("Open 补齐后 = %+v，期望补描述且字节不清零", s)
	}
}

// 客户端隔离：同一播放单元、不同客户端各自一条；不同单元互不串。
func TestTrackerKeyIsolation(t *testing.T) {
	clock := newClock()
	tr := NewTracker(clock.Now)
	ttl := 120 * time.Second

	tr.Open(Client{IP: "10.0.0.1", UA: "A"}, Session{UID: "u1", Title: "剧一"}, ttl)
	tr.Open(Client{IP: "10.0.0.1", UA: "B"}, Session{UID: "u1", Title: "剧一"}, ttl)
	tr.Open(Client{IP: "10.0.0.2", UA: "A"}, Session{UID: "u2", Title: "剧二"}, ttl)

	got := tr.Snapshot(ttl)
	if len(got.Sessions) != 3 || got.Clients != 3 {
		t.Fatalf("= %d 条 / %d 客户端，期望 3 / 3", len(got.Sessions), got.Clients)
	}
}

// 最近活跃降序：页面按这个顺序展示。
func TestTrackerOrderedByLastSeen(t *testing.T) {
	clock := newClock()
	tr := NewTracker(clock.Now)
	ttl := 120 * time.Second

	tr.Open(Client{IP: "1", UA: "A"}, Session{UID: "old"}, ttl)
	clock.Step(time.Second)
	tr.Open(Client{IP: "2", UA: "B"}, Session{UID: "new"}, ttl)
	clock.Step(time.Second)
	tr.Progress(Client{IP: "1", UA: "A"}, "old", 1, 60, 1, 0, ttl)

	got := tr.Snapshot(ttl)
	if got.Sessions[0].UID != "old" {
		t.Fatalf("首条 = %q，期望最近活跃的 old", got.Sessions[0].UID)
	}
}

// AC-5：50 次播放后表受 TTL 约束、不无限增长；过期清空后重新播立即只有 1 条。
// 全程假钟推进，不 sleep。
func TestTrackerBoundedByTTL(t *testing.T) {
	clock := newClock()
	tr := NewTracker(clock.Now)
	ttl := 120 * time.Second

	for i := 0; i < 50; i++ {
		cl := Client{IP: "10.0.0." + strconv.Itoa(i), UA: "UA"}
		tr.Open(cl, Session{UID: "u" + strconv.Itoa(i), Title: "剧"}, ttl)
		clock.Step(time.Second)
	}
	if got := tr.Snapshot(ttl); len(got.Sessions) != 50 {
		t.Fatalf("t=49s 时 %d 条，期望 50", len(got.Sessions))
	}

	clock.Step(51 * time.Second) // t=100s，全部在 TTL 窗口内
	if got := tr.Snapshot(ttl); len(got.Sessions) != 50 {
		t.Fatalf("t=100s 时 %d 条，期望 50", len(got.Sessions))
	}

	clock.Step(100 * time.Second) // t=200s，全部过期
	if got := tr.Snapshot(ttl); len(got.Sessions) != 0 {
		t.Fatalf("t=200s 时 %d 条，期望 0", len(got.Sessions))
	}

	tr.Open(Client{IP: "10.0.0.1", UA: "UA"}, Session{UID: "u1"}, ttl)
	if got := tr.Snapshot(ttl); len(got.Sessions) != 1 {
		t.Fatalf("重新播放后 %d 条，期望 1", len(got.Sessions))
	}
}

// 空表快照的 Sessions 必须是空切片而非 nil：/api/playback 要序列化成 []。
func TestTrackerEmptySnapshotNonNil(t *testing.T) {
	tr := NewTracker(newClock().Now)
	got := tr.Snapshot(120 * time.Second)
	if got.Sessions == nil {
		t.Fatal("Sessions 为 nil，期望空切片")
	}
	if len(got.Sessions) != 0 || got.Clients != 0 || got.Bytes != 0 {
		t.Fatalf("空表快照 = %+v，期望零值", got)
	}
}

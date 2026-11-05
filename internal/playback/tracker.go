// Package playback 是「正在播放」的内存会话表（MKACG-683）。
//
// 只做一件事：把播放面的请求（拉播放列表、取分片）折成「哪台客户端正在播哪部剧
// 的哪一个播放单元、播到第几片、出了多少字节」。表只在内存里，进程重启即丢；
// 不落库、不参与出流决策 —— 采集失败绝不影响出流（AGENTS.md 硬约束 1/9）。
//
// 过期回收不靠后台 goroutine：Open / Progress / Snapshot 每次入口先按 TTL 扫一遍，
// 表的大小恒等于「TTL 窗口内有活动的 (客户端, 播放单元) 数」。
package playback

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// 会话状态（MKACG-687）：只取过播放列表、还没出过一个分片的会话是「探测」
// （ffprobe 读 m3u8 之类），不该被算成正在播放。
const (
	StateProbing = "probing"
	StatePlaying = "playing"
)

// Client 是一次播放请求的客户端身份。只有两件事实：来源 IP 与 User-Agent。
type Client struct {
	IP string
	UA string
}

// Session 是一条播放会话，字段即展示形态：/api/playback 直接用，不再回查库。
type Session struct {
	UID   string // 播放单元 id（单集或合并块）
	Title string // 剧名；空 = 未知
	// State 是快照时派生的会话状态：StateProbing / StatePlaying。
	State string
	// Unit 是人读的块描述（「第 3 集」/「第 11–20 集」）；空 = 未知。
	Unit    string
	DramaID int64 // 0 = 未知
	// Seq 是已请求到第几片（0 基），Slices 是播放单元的总片数。
	Seq, Slices int
	// PositionSec 是已出流部分的时长起点（本片在单元时间轴上的起点），
	// TotalSec 是整个播放单元的时长。
	PositionSec, TotalSec int
	BytesServed           int64
	// TimeToFirstSegment 是本次会话最近一次冷启动的首片耗时（MKACG-686）：
	// 从分片请求进入到第一个字节写出。命中缓存时为 0（没有等待）。
	TimeToFirstSegment  time.Duration
	ClientIP            string
	UserAgent           string
	FirstSeen, LastSeen time.Time

	// played 表示这条会话出过至少一个分片；只有 Open 的会话是探测。
	played bool
}

// Snapshot 是一次快照：会话按最近活跃降序，另附汇总。
//
// Streams 的语义由 /api/playback 决定：Playing 是出过分片的会话数，Probing 是
// 只取过播放列表的会话数。Clients 只统计 Playing 会话的去重客户端。
type Snapshot struct {
	Sessions []Session
	Playing  int
	Probing  int
	Clients  int
	Bytes    int64
}

// Tracker 是并发安全的会话表。now 由构造方注入，测试用假钟验证 TTL。
type Tracker struct {
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewTracker 构造会话表。now 不可为 nil（生产的装配根传 time.Now）。
func NewTracker(now func() time.Time) *Tracker {
	return &Tracker{now: now, sessions: make(map[string]*Session)}
}

// Open 建立或续接一条会话：播放列表请求意味着「从这个单元开始播」。
//
// 已存在的会话只刷新 LastSeen 与描述字段，进度（Seq / PositionSec / BytesServed）
// 保持不动 —— 播放端重取 playlist（seek、重连）不该把进度清零。
func (t *Tracker) Open(cl Client, s Session, ttl time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	t.sweepLocked(now, ttl)

	key := sessionKey(cl, s.UID)
	cur, ok := t.sessions[key]
	if !ok {
		cur = &Session{UID: s.UID, FirstSeen: now}
		t.sessions[key] = cur
	}
	cur.Title = s.Title
	cur.Unit = s.Unit
	cur.DramaID = s.DramaID
	cur.Slices = s.Slices
	cur.TotalSec = s.TotalSec
	cur.ClientIP = cl.IP
	cur.UserAgent = cl.UA
	cur.LastSeen = now
}

// Progress 记录一次分片出流。会话不存在时建骨架（播放端直接取分片、playlist 还没到），
// 描述字段留空由随后的 Open 补齐。
func (t *Tracker) Progress(cl Client, uid string, seq, positionSec int, bytes int64, firstSegment, ttl time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	t.sweepLocked(now, ttl)

	key := sessionKey(cl, uid)
	cur, ok := t.sessions[key]
	if !ok {
		cur = &Session{UID: uid, FirstSeen: now, ClientIP: cl.IP, UserAgent: cl.UA}
		t.sessions[key] = cur
	}
	cur.Seq = seq
	cur.PositionSec = positionSec
	cur.BytesServed += bytes
	cur.played = true
	if firstSegment > 0 {
		cur.TimeToFirstSegment = firstSegment
	}
	cur.ClientIP = cl.IP
	cur.UserAgent = cl.UA
	cur.LastSeen = now
}

// Snapshot 先按 TTL 回收再返回当前会话（按最近活跃降序）。
//
// 返回值是副本，调用方随便改；Sessions 恒为非 nil 切片。
func (t *Tracker) Snapshot(ttl time.Duration) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	t.sweepLocked(now, ttl)

	out := Snapshot{Sessions: make([]Session, 0, len(t.sessions))}
	clients := make(map[string]struct{}, len(t.sessions))
	for _, s := range t.sessions {
		cp := *s
		if cp.played {
			cp.State = StatePlaying
			out.Playing++
			clients[cp.ClientIP+"\x00"+cp.UserAgent] = struct{}{}
		} else {
			cp.State = StateProbing
			out.Probing++
		}
		out.Sessions = append(out.Sessions, cp)
		out.Bytes += cp.BytesServed
	}
	// 最近活跃优先；同一时刻按 UID、再按客户端定序，保证输出稳定。
	sort.SliceStable(out.Sessions, func(i, j int) bool {
		a, b := out.Sessions[i], out.Sessions[j]
		if !a.LastSeen.Equal(b.LastSeen) {
			return a.LastSeen.After(b.LastSeen)
		}
		if a.UID != b.UID {
			return a.UID < b.UID
		}
		return a.ClientIP < b.ClientIP
	})
	out.Clients = len(clients)
	return out
}

// sweepLocked 删除 last_seen 早于 TTL 的会话。ttl <= 0 视为「不回收」，
// 避免配置异常时把整张表清空。
func (t *Tracker) sweepLocked(now time.Time, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	for key, s := range t.sessions {
		if now.Sub(s.LastSeen) > ttl {
			delete(t.sessions, key)
		}
	}
}

// sessionKey 是（客户端, 播放单元）的键：定长 hash 前缀把 IP/UA 折成 16 个十六进制
// 字符，避免 UA 把键撑长（UA 最长可到 256 字符上限）。
func sessionKey(cl Client, uid string) string {
	sum := sha256.Sum256([]byte(cl.IP + "\x00" + cl.UA))
	return hex.EncodeToString(sum[:])[:16] + "|" + uid
}

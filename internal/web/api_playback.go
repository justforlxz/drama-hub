package web

import (
	"net/http"
	"time"

	"github.com/justforlxz/drama-hub/internal/playback"
	"github.com/justforlxz/drama-hub/internal/store"
)

// playbackView 是 GET /api/playback 的响应体（MKACG-683）：当前会话 + 汇总。
//
// 页面按 this 直接渲染，不需要再拼别的接口；空表时 sessions 是 []，不是 null。
type playbackView struct {
	// Now 是本次快照时间（UTC RFC3339），页面用它算「刚刚 / N 分钟前」。
	Now      string                `json:"now"`
	Streams  int                   `json:"streams"` // 会话数
	Clients  int                   `json:"clients"` // 去重后的客户端数
	Bytes    int64                 `json:"bytes"`   // 累计出流字节
	Sessions []playbackSessionView `json:"sessions"`
}

// playbackSessionView 是一条播放会话。时间写成 UTC RFC3339 文本，零值写空串，
// 时区转换归页面 JS（与 /api/status 同一约定）。
type playbackSessionView struct {
	UID         string `json:"uid"`
	DramaID     int64  `json:"drama_id"`
	Title       string `json:"title"`
	Unit        string `json:"unit"`
	Seq         int    `json:"seq"`
	Slices      int    `json:"slices"`
	PositionSec int    `json:"position_sec"`
	TotalSec    int    `json:"total_sec"`
	BytesServed int64  `json:"bytes_served"`
	ClientIP    string `json:"client_ip"`
	UserAgent   string `json:"user_agent"`
	FirstSeen   string `json:"first_seen"`
	LastSeen    string `json:"last_seen"`
}

// handlePlayback 处理 GET /api/playback：只读快照，不改会话表。
//
// 会话表本来就在内存里，这个端点不查库；TTL 每请求现读 settings，读不到按
// 「不回收」处理（宁可多显示一条，也不要 500 把整块页面打挂）。
func (s *Server) handlePlayback(w http.ResponseWriter, r *http.Request) {
	var snap playback.Snapshot
	var ttl time.Duration
	if s.playback != nil {
		if sec, err := s.settings.Int(r.Context(), store.KeyPlaySessionTTL); err == nil {
			ttl = time.Duration(sec) * time.Second
		}
		snap = s.playback.Snapshot(ttl)
	}

	view := playbackView{
		Now:      formatTime(time.Now()),
		Streams:  len(snap.Sessions),
		Clients:  snap.Clients,
		Bytes:    snap.Bytes,
		Sessions: make([]playbackSessionView, 0, len(snap.Sessions)),
	}
	for _, sess := range snap.Sessions {
		view.Sessions = append(view.Sessions, playbackSessionView{
			UID:         sess.UID,
			DramaID:     sess.DramaID,
			Title:       sess.Title,
			Unit:        sess.Unit,
			Seq:         sess.Seq,
			Slices:      sess.Slices,
			PositionSec: sess.PositionSec,
			TotalSec:    sess.TotalSec,
			BytesServed: sess.BytesServed,
			ClientIP:    sess.ClientIP,
			UserAgent:   sess.UserAgent,
			FirstSeen:   formatTime(sess.FirstSeen),
			LastSeen:    formatTime(sess.LastSeen),
		})
	}
	writeJSON(w, http.StatusOK, view)
}

package segment

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/justforlxz/drama-hub/internal/playback"
	"github.com/justforlxz/drama-hub/internal/store"
)

// uaMaxRunes 是会话表里 User-Agent 的保留上限（MKACG-683 设计契约 SHOULD）。
// 截断发生在采集侧，截断后的值既用于展示，也参与 playback 包里的会话键计算
// —— 超过上限的 UA 只有前缀不同时会被并成一条流（可接受：同一个客户端）。
const uaMaxRunes = 256

// playbackTTL 现读「播放会话保留时长」（settings 键 play_session_ttl_sec）。
// 读不到就不记：采集失败绝不影响出流（MUST）。
func (c *Cutter) playbackTTL(ctx context.Context) (time.Duration, bool) {
	if c.playback == nil {
		return 0, false
	}
	sec, err := c.settings.Int(ctx, store.KeyPlaySessionTTL)
	if err != nil || sec <= 0 {
		return 0, false
	}
	return time.Duration(sec) * time.Second, true
}

// recordOpen 在播放列表写出后建立/续接会话。剧名查库失败只留空标题，
// 其余字段照样记 —— 会话表是展示用的，不参与出流。
//
// Slices / TotalSec 取**刚生成 playlist 的那个时间轴**，不重新累加 eps：
// 后台「正在播放」的「本单元总时长」必须与 m3u8 的 EXTINF 总和同口径（MKACG-689）。
// 时间轴生成时已保证每个成员都有真实时长（探测失败就不出 playlist），所以这里
// 记下的总值不会是「部分之和」。
func (c *Cutter) recordOpen(ctx context.Context, r *http.Request, uid string, eps []store.Episode, tl *Timeline) {
	ttl, ok := c.playbackTTL(ctx)
	if !ok {
		return
	}
	s := playback.Session{UID: uid}
	if tl != nil {
		s.Slices = tl.Len()
		s.TotalSec = int(math.Round(tl.Total()))
	}
	if len(eps) > 0 {
		s.DramaID = eps[0].DramaID
		s.Unit = playUnitText(eps)
		if d, err := c.st.DramaByID(ctx, eps[0].DramaID); err == nil {
			s.Title = d.Title
		}
	}
	c.playback.Open(clientOf(r), s, ttl)
}

// recordProgress 在分片写出后更新会话：第几片、本片在单元时间轴上的起点、本次出流字节，
// 以及冷启动首片耗时（MKACG-686；命中缓存时为 0）。
func (c *Cutter) recordProgress(ctx context.Context, r *http.Request, uid string, seq int, positionSec int, bytes int64, firstSegment time.Duration) {
	ttl, ok := c.playbackTTL(ctx)
	if !ok {
		return
	}
	c.playback.Progress(clientOf(r), uid, seq, positionSec, bytes, firstSegment, ttl)
}

// clientOf 只认 r.RemoteAddr 的 host；不解析 X-Forwarded-For（设计契约明确不做）。
func clientOf(r *http.Request) playback.Client {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = host
	}
	ua := r.UserAgent()
	if utf8.RuneCountInString(ua) > uaMaxRunes {
		ua = string([]rune(ua)[:uaMaxRunes])
	}
	return playback.Client{IP: ip, UA: ua}
}

// prefetchWaitsForSegment 报告预热是否应等到首个分片请求之后再触发（settings 键
// prefetch_on，MKACG-687）。读不到或值非 "playlist" 都按默认 "first_segment" 处理：
// 提前预热是「多烧一次 remux」的副作用，宁可晚一点也不要白烧。
func (c *Cutter) prefetchWaitsForSegment(ctx context.Context) bool {
	v, err := c.settings.String(ctx, store.KeyPrefetchOn)
	if err != nil {
		return true
	}
	return v != store.PrefetchOnPlaylist
}

// startPlaybackWarmup 在首个分片出流之后起预热（MKACG-687）。此时前台已经拿到字节，
// 预热不会抢走它需要的 ffmpeg 闸位。prefetch_on=playlist 时预热点在播放列表，
// 这里不重复触发（kick* 自身也有单飞保护，重复调用无害）。
func (c *Cutter) startPlaybackWarmup(ctx context.Context, uid string, eps []store.Episode) {
	if len(eps) == 0 || !c.prefetchWaitsForSegment(ctx) {
		return
	}
	c.kickPrefetch(eps[0].DramaID, eps)
	c.kickSegmentPrefetch(uid, eps)
}

// logPlaylistRequest 记录一次播放列表请求的来源（MKACG-687）。会话表只按「IP + UA」
// 合并，多出来的那条 0 B 会话（探测）只能靠这条日志认出是谁。
func (c *Cutter) logPlaylistRequest(r *http.Request, uid string) {
	cl := clientOf(r)
	c.logger.InfoContext(r.Context(), "playlist requested",
		"uid", uid, "remote", cl.IP, "ua", cl.UA)
}

// playUnitText 把播放单元成员折成一句人读的块描述：「第 3 集」或「第 11–20 集」。
func playUnitText(eps []store.Episode) string {
	if len(eps) == 0 {
		return ""
	}
	first, last := eps[0].EpNo, eps[len(eps)-1].EpNo
	if first == last {
		return fmt.Sprintf("第 %d 集", first)
	}
	return fmt.Sprintf("第 %d–%d 集", first, last)
}

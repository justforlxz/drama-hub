package segment

import (
	"github.com/justforlxz/drama-hub/internal/store"
)

// EpisodeSegments 是时间轴上的一集：分片索引（按 start_sec 升序）。供合并集
// （M7）按 merge_item.ord 组装成连续 seq 空间时传入。
type EpisodeSegments struct {
	EpisodeID int64
	EpNo      int
	Segments  []store.Segment
}

// Slice 是 seq 解析出的一个分片：它属于哪一集、集内起点与真实时长。
//
// Seq 是合并时间轴上的全局序号（0 起、连续）；EpNo 是集号（原 episode.ep_no，
// 不是合并集内的第几段）；StartSec/Dur 以秒为单位。M7 的播放列表按它写
// EXTINF 并在集边界插 EXT-X-DISCONTINUITY。
type Slice struct {
	Seq       int
	EpisodeID int64
	EpNo      int
	StartSec  float64
	Dur       float64
	// OffsetSec 是该分片在合并时间轴上的绝对起点（前一集总时长起累计）。
	// 切片时传给 ffmpeg 的 -output_ts_offset，保证跨集 DTS 连续、播放器不
	// 因时间戳回退而丢帧（EXT-X-DISCONTINUITY 之外的第二道保险）。
	OffsetSec float64
}

// Timeline 是把若干集的分片按给定顺序拼成的合并时间轴。seq 从 0 起连续，
// 跨集不断号。纯内存结构（不落库）；At 是切片下标取值，O(1)。
type Timeline struct {
	slices []Slice
}

// NewTimeline 按 eps 的顺序拼接分片成时间轴，返回 seq 从 0 起的映射。
//
// 每集的 Segments 必须按 start_sec 升序（store.ListSegments 保证）；空集（无
// 分片）不在时间轴上贡献任何片段 —— 播放时该 seq 区间缺失，由调用方在组装前
// 决定跳过整集还是 503（M7 编排粒度）。
func NewTimeline(eps []EpisodeSegments) *Timeline {
	t := &Timeline{}
	var offset float64
	for _, ep := range eps {
		for _, seg := range ep.Segments {
			t.slices = append(t.slices, Slice{
				Seq:       len(t.slices),
				EpisodeID: ep.EpisodeID,
				EpNo:      ep.EpNo,
				StartSec:  seg.StartSec,
				Dur:       seg.Dur,
				OffsetSec: offset,
			})
			offset += seg.Dur
		}
	}
	return t
}

// Len 返回时间轴上的分片总数。
func (t *Timeline) Len() int { return len(t.slices) }

// At 返回 seq 对应的分片；seq 越界（< 0 或 ≥ Len）返回 ok=false。
func (t *Timeline) At(seq int) (Slice, bool) {
	if seq < 0 || seq >= len(t.slices) {
		return Slice{}, false
	}
	return t.slices[seq], true
}

// Total 返回时间轴总时长：各片 Dur 之和 = 各集源时长之和（±一帧）。
func (t *Timeline) Total() float64 {
	total := 0.0
	for _, s := range t.slices {
		total += s.Dur
	}
	return total
}

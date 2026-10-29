package store

import (
	"context"
	"fmt"
)

// Segment 是 segment 表的一行：某集的一个分片索引。
//
// StartSec / Dur 是秒（切点对齐关键帧后的真实时长），Dur 之和 = 该集源时长。
// Keyframe 为 true 表示切点在关键帧上，可以 `-c copy` 现切。CodecSig 是探测到的
// 解码器签名（集间不一致时由上层决定转码，M6 不做该判定）。Mode 取 copy 或
// transcode（schema CHECK 约束），M6 只产出 copy。
type Segment struct {
	ID        int64
	EpisodeID int64
	StartSec  float64
	Dur       float64
	Keyframe  bool
	CodecSig  string
	Mode      string
}

// segmentColumns 是 segment 表的显式列清单，顺序必须与 scanSegment 一致。
const segmentColumns = `SELECT id, episode_id, start_sec, dur, keyframe, codec_sig, mode FROM segment`

// scanSegment 按 segmentColumns 的顺序读出一行。
func scanSegment(scan func(dest ...any) error) (Segment, error) {
	var s Segment
	if err := scan(&s.ID, &s.EpisodeID, &s.StartSec, &s.Dur, &s.Keyframe, &s.CodecSig, &s.Mode); err != nil {
		return Segment{}, err
	}
	return s, nil
}

// ReplaceSegments 用一个新索引整体替换某集的分片记录。
//
// 同一事务内先删后插：索引是「一次预扫、整体重算」的派生数据，替换语义保证重复
// 索引幂等（不会残留上一轮的切点），失败整体回滚、不留半套索引。segs 为空等价于
// 清空该集索引。唯一/主键约束冲突返回 ErrConflict。
func (s *Store) ReplaceSegments(ctx context.Context, episodeID int64, segs []Segment) error {
	return s.InTx(ctx, func(tx *Store) error {
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM segment WHERE episode_id = ?`, episodeID); err != nil {
			return wrapWriteErr("清空旧索引", err)
		}
		for _, seg := range segs {
			if _, err := tx.db.ExecContext(ctx,
				`INSERT INTO segment(episode_id, start_sec, dur, keyframe, codec_sig, mode)
				 VALUES(?, ?, ?, ?, ?, ?)`,
				episodeID, seg.StartSec, seg.Dur, seg.Keyframe, seg.CodecSig, seg.Mode,
			); err != nil {
				return wrapWriteErr("写入索引", err)
			}
		}
		return nil
	})
}

// ListSegments 返回某集的分片索引，按 start_sec 升序（即片内序）。
//
// (episode_id, start_sec) 唯一，因此顺序确定。无行返回空切片而不是 ErrNotFound：
// 「该集还没有索引」是正常状态（索引任务未跑完），由调用方按 503 处理。
func (s *Store) ListSegments(ctx context.Context, episodeID int64) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx,
		segmentColumns+` WHERE episode_id = ? ORDER BY start_sec`, episodeID)
	if err != nil {
		return nil, fmt.Errorf("列索引 episode_id=%d: %w", episodeID, err)
	}
	defer rows.Close()

	out := make([]Segment, 0)
	for rows.Next() {
		seg, err := scanSegment(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("读取索引行: %w", err)
		}
		out = append(out, seg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历索引 episode_id=%d: %w", episodeID, err)
	}
	return out, nil
}

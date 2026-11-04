package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EpisodeStream 是 episode_stream 表的一行：某集某通道的直链快照。
//
// 直链是短时凭据（docs/adr/0016-episode-stream-cache.md）：存下来是为了按
// expires_at 判定复用，而不是把它当永久链接。URL 与 Key 同生命周期 —— 一起失效、
// 一起重取；Key 为 nil 表示明文源。
type EpisodeStream struct {
	EpisodeID int64
	Channel   string
	URL       string
	Key       []byte
	ExpiresAt string // UTC RFC3339：到这个时刻之后不再复用
	FetchedAt string // UTC RFC3339：解析落库时刻
	LastOKAt  string // UTC RFC3339：上次确认能用；空串 = 还没有确认过
}

// GetEpisodeStream 返回某集最近一次落库的直链。无行返回 ErrNotFound。
//
// 一集可能有多个通道的行（通道翻转的历史），取最近解析的那一条：调用方按
// expires_at 判定能不能复用，不能复用就重解析并在同通道上覆盖。
func (s *Store) GetEpisodeStream(ctx context.Context, episodeID int64) (EpisodeStream, error) {
	var es EpisodeStream
	err := s.db.QueryRowContext(ctx,
		`SELECT episode_id, channel, url, key, expires_at, fetched_at, last_ok_at
		   FROM episode_stream WHERE episode_id = $1
		  ORDER BY fetched_at DESC, channel LIMIT 1`, episodeID).
		Scan(&es.EpisodeID, &es.Channel, &es.URL, &es.Key,
			&es.ExpiresAt, &es.FetchedAt, &es.LastOKAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EpisodeStream{}, ErrNotFound
	}
	if err != nil {
		return EpisodeStream{}, fmt.Errorf("读取直链 episode_id=%d: %w", episodeID, err)
	}
	return es, nil
}

// UpsertEpisodeStream 写入或刷新某集某通道的直链。
//
// key 为 NULL 表示明文源；url 与 key 一起覆盖，绝不出现「新 URL 配旧 key」。
//
// last_ok_at 只在调用方显式给了非空值时覆盖：解析落库（stream.Resolver.persist）
// 根本不知道这条新链能不能用，不能把「上次确认能用」擦掉 —— 每刷新一次直链就清一次，
// 等于这个字段永远是空串（MKACG-680 遗留缺陷，MKACG-681 修正）。
func (s *Store) UpsertEpisodeStream(ctx context.Context, es EpisodeStream) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO episode_stream(episode_id, channel, url, key, expires_at, fetched_at, last_ok_at)
		 VALUES($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT(episode_id, channel) DO UPDATE SET
		     url = excluded.url, key = excluded.key,
		     expires_at = excluded.expires_at, fetched_at = excluded.fetched_at,
		     last_ok_at = CASE WHEN excluded.last_ok_at = ''
		                       THEN episode_stream.last_ok_at
		                       ELSE excluded.last_ok_at END`,
		es.EpisodeID, es.Channel, es.URL, es.Key, es.ExpiresAt, es.FetchedAt, es.LastOKAt,
	); err != nil {
		return wrapWriteErr("写入直链", err)
	}
	return nil
}

// InvalidateEpisodeStream 作废某集的直链（所有通道）。
//
// 只作废这一集：一次 403 只说明这条链接不能用了，不能因为一条失败清掉整部剧
// （docs/adr/0016-episode-stream-cache.md「失败自愈」）。
func (s *Store) InvalidateEpisodeStream(ctx context.Context, episodeID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM episode_stream WHERE episode_id = $1`, episodeID,
	); err != nil {
		return wrapWriteErr("作废直链", err)
	}
	return nil
}

// TouchEpisodeStream 记录某集最近一次确认能用的时刻。
//
// at 是与 fetched_at 同格式的 UTC RFC3339 文本；不改变 url/key/expires_at。
func (s *Store) TouchEpisodeStream(ctx context.Context, episodeID int64, at string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE episode_stream SET last_ok_at = $2 WHERE episode_id = $1`,
		episodeID, at,
	); err != nil {
		return wrapWriteErr("更新直链可用时间", err)
	}
	return nil
}

// StreamFailure 是 episode_stream_fail 表的一行：某集最近一次预取失败与下次可重试时刻。
//
// 失败状态与直链分表（docs/adr/0017-prefetch-and-stream-health.md）：失败时通常还没有
// 可信的通道（web / app），而 episode_stream 的主键带通道、url NOT NULL。前台播放路径
// 从不读这张表 —— 它只是后台预取的退避依据。
type StreamFailure struct {
	EpisodeID  int64
	Reason     string // stream.FailureReason 的固定取值
	FailedAt   string // UTC RFC3339
	RetryAfter string // UTC RFC3339：到这个时刻之后才允许再预取
}

// RecordStreamFailure 记下一次预取失败（同集覆盖，保留最近一次）。
//
// reason / failedAt / retryAfter 都由调用方（internal/segment 的预取器）算好：
// 分类判据在 internal/stream.FailureReason，时间格式与 0001 的业务时间列一致。
func (s *Store) RecordStreamFailure(ctx context.Context, episodeID int64, reason, failedAt, retryAfter string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO episode_stream_fail(episode_id, reason, failed_at, retry_after)
		 VALUES($1, $2, $3, $4)
		 ON CONFLICT(episode_id) DO UPDATE SET
		     reason = excluded.reason,
		     failed_at = excluded.failed_at,
		     retry_after = excluded.retry_after`,
		episodeID, reason, failedAt, retryAfter,
	); err != nil {
		return wrapWriteErr("写入预取失败记录", err)
	}
	return nil
}

// ClearStreamFailures 删除若干集的预取失败记录（预取成功后调用）。
//
// 批量删而不是逐条：一个合并块的成员一起成功，一条 SQL 比 N 条更省往返。
func (s *Store) ClearStreamFailures(ctx context.Context, episodeIDs []int64) error {
	if len(episodeIDs) == 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM episode_stream_fail WHERE episode_id = ANY($1)`, episodeIDs,
	); err != nil {
		return wrapWriteErr("清除预取失败记录", err)
	}
	return nil
}

// StreamFailures 返回某部剧全部集的预取失败记录，按集主键索引。
//
// 预取计划按剧一次取出（而不是逐集查）：判定「这一集还在退避期内」只读这一张表，
// 不碰 episode_stream，也不改变前台的任何行为。
func (s *Store) StreamFailures(ctx context.Context, dramaID int64) (map[int64]StreamFailure, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT f.episode_id, f.reason, f.failed_at, f.retry_after
		   FROM episode_stream_fail f
		   JOIN episode e ON e.id = f.episode_id
		  WHERE e.drama_id = $1`, dramaID)
	if err != nil {
		return nil, fmt.Errorf("读取预取失败记录 drama_id=%d: %w", dramaID, err)
	}
	defer rows.Close()

	out := make(map[int64]StreamFailure)
	for rows.Next() {
		var f StreamFailure
		if err := rows.Scan(&f.EpisodeID, &f.Reason, &f.FailedAt, &f.RetryAfter); err != nil {
			return nil, fmt.Errorf("读取预取失败行: %w", err)
		}
		out[f.EpisodeID] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历预取失败记录 drama_id=%d: %w", dramaID, err)
	}
	return out, nil
}

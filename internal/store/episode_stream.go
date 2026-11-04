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
func (s *Store) UpsertEpisodeStream(ctx context.Context, es EpisodeStream) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO episode_stream(episode_id, channel, url, key, expires_at, fetched_at, last_ok_at)
		 VALUES($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT(episode_id, channel) DO UPDATE SET
		     url = excluded.url, key = excluded.key,
		     expires_at = excluded.expires_at, fetched_at = excluded.fetched_at,
		     last_ok_at = excluded.last_ok_at`,
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

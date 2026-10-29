package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Drama 是 drama 表的一行，对应上游剧目。
//
// ID 是输出字段：UpsertDrama 忽略入参 ID，读回时才有意义。SyncAt 是不透明
// TEXT，调用方按 docs/DEV-STANDARDS.md 写 UTC RFC3339，store 不解析、不格式化、
// 不校验。
type Drama struct {
	ID           int64
	Provider     string
	ProviderID   string
	Title        string
	Year         int
	Plot         string
	CoverURL     string
	EpisodeTotal int
	SyncAt       string
}

// Episode 是 episode 表的一行，对应某部剧的一集。
//
// ID 是输出字段：UpsertEpisode 忽略入参 ID。PlayURLAt 是不透明 TEXT（UTC
// RFC3339），语义同 Drama.SyncAt。
type Episode struct {
	ID          int64
	DramaID     int64
	EpNo        int
	Title       string
	DurationSec int
	Source      string
	Vid         string
	PlayURL     string
	PlayURLAt   string
}

// dbtx 是 Store 实际使用的 SQL 句柄：*sql.DB（根 Store）与 *sql.Tx（事务内
// Store）都满足，因此同一批方法在事务内外共用一份实现。
type dbtx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Store 是 drama / episode 两张表的读写入口。
//
// 与 Settings 同构：构造注入 *sql.DB、不缓存。drama 与 episode 的读写天生跨表
// （删剧级联、M2 批量入库、M3 一次枚举剧与集），因此共用一个 receiver。
type Store struct {
	db   dbtx
	root *sql.DB // 只有根 Store 非 nil；InTx 用它开事务
}

// NewStore 构造 Store。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, root: db}
}

// InTx 在同一事务内执行 fn：fn 返回错误即回滚并把该错误原样返回，返回 nil 则提交。
//
// 事务内的 *Store 复用同一批读写方法，SQL 与语义与根 Store 完全一致。fn 里不得做
// 网络或外部进程 I/O：SQLite 单写者，长事务会持锁阻塞其它写入（同步的取数阶段
// 必须在 InTx 之外完成）。
func (s *Store) InTx(ctx context.Context, fn func(tx *Store) error) error {
	if s.root == nil {
		return errors.New("store: 不支持嵌套事务")
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务: %w", err)
	}
	if err := fn(&Store{db: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务: %w", err)
	}
	return nil
}

// dramaColumns 是 drama 表的显式列清单，顺序必须与 scanDrama 的 Scan 顺序一致。
const dramaColumns = `SELECT id, provider, provider_id, title, year, plot, cover_url, episode_total, sync_at FROM drama`

// episodeColumns 是 episode 表的显式列清单，顺序必须与 scanEpisode 的 Scan 顺序一致。
const episodeColumns = `SELECT id, drama_id, ep_no, title, duration_sec, source, vid, play_url, play_url_at FROM episode`

// scanDrama 按 dramaColumns 的顺序读出一行；scan 传 *sql.Row.Scan 或 *sql.Rows.Scan。
func scanDrama(scan func(dest ...any) error) (Drama, error) {
	var d Drama
	if err := scan(&d.ID, &d.Provider, &d.ProviderID, &d.Title, &d.Year, &d.Plot, &d.CoverURL, &d.EpisodeTotal, &d.SyncAt); err != nil {
		return Drama{}, err
	}
	return d, nil
}

// scanEpisode 按 episodeColumns 的顺序读出一行。
func scanEpisode(scan func(dest ...any) error) (Episode, error) {
	var e Episode
	if err := scan(&e.ID, &e.DramaID, &e.EpNo, &e.Title, &e.DurationSec, &e.Source, &e.Vid, &e.PlayURL, &e.PlayURLAt); err != nil {
		return Episode{}, err
	}
	return e, nil
}

// UpsertDrama 以 (provider, provider_id) 为身份写入或更新一部剧，返回行 id。
//
// 冲突时只更新可变态（title / year / plot / cover_url / episode_total / sync_at），
// provider 与 provider_id 是身份、不被覆盖。入参 ID 被忽略，避免调用方陈旧 id
// 造成主键冲突。唯一/主键约束冲突返回 ErrConflict。
func (s *Store) UpsertDrama(ctx context.Context, d Drama) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO drama(provider, provider_id, title, year, plot, cover_url, episode_total, sync_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(provider, provider_id) DO UPDATE
		 SET title = excluded.title,
		     year = excluded.year,
		     plot = excluded.plot,
		     cover_url = excluded.cover_url,
		     episode_total = excluded.episode_total,
		     sync_at = excluded.sync_at
		 RETURNING id`,
		d.Provider, d.ProviderID, d.Title, d.Year, d.Plot, d.CoverURL, d.EpisodeTotal, d.SyncAt,
	).Scan(&id)
	if err != nil {
		return 0, wrapWriteErr("写入剧目", err)
	}
	return id, nil
}

// DramaByProviderID 按 (provider, provider_id) 查一部剧。
//
// 无行返回 ErrNotFound；其余错误带上下文返回。
func (s *Store) DramaByProviderID(ctx context.Context, provider, providerID string) (Drama, error) {
	row := s.db.QueryRowContext(ctx,
		dramaColumns+` WHERE provider = ? AND provider_id = ?`, provider, providerID)
	d, err := scanDrama(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Drama{}, ErrNotFound
	}
	if err != nil {
		return Drama{}, fmt.Errorf("按 provider_id 查剧目: %w", err)
	}
	return d, nil
}

// DramaByID 按主键查一部剧。
//
// 无行返回 ErrNotFound；其余错误带上下文返回。
func (s *Store) DramaByID(ctx context.Context, id int64) (Drama, error) {
	row := s.db.QueryRowContext(ctx, dramaColumns+` WHERE id = ?`, id)
	d, err := scanDrama(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Drama{}, ErrNotFound
	}
	if err != nil {
		return Drama{}, fmt.Errorf("查剧目 id=%d: %w", id, err)
	}
	return d, nil
}

// ListDramas 返回全部剧目，按 id 升序（只保证确定性，呈现排序归上层）。
func (s *Store) ListDramas(ctx context.Context) ([]Drama, error) {
	rows, err := s.db.QueryContext(ctx, dramaColumns+` ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("列剧目: %w", err)
	}
	defer rows.Close()

	out := make([]Drama, 0)
	for rows.Next() {
		d, err := scanDrama(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("读取剧目行: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历剧目: %w", err)
	}
	return out, nil
}

// DeleteDrama 删除一部剧，其 episode 由 schema 的 ON DELETE CASCADE 一并删除。
//
// 级联依赖 foreign_keys=ON，由 Open 的 DSN 施加。无行受影响返回 ErrNotFound。
func (s *Store) DeleteDrama(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM drama WHERE id = ?`, id)
	if err != nil {
		return wrapWriteErr("删除剧目", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("删除剧目 id=%d: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpsertEpisode 以 (drama_id, ep_no) 为身份写入或更新一集，返回行 id。
//
// 冲突时只更新可变态（title / duration_sec / source / vid / play_url / play_url_at），
// drama_id 与 ep_no 是身份、不被覆盖。入参 ID 被忽略。唯一/主键约束冲突返回
// ErrConflict。
func (s *Store) UpsertEpisode(ctx context.Context, e Episode) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO episode(drama_id, ep_no, title, duration_sec, source, vid, play_url, play_url_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(drama_id, ep_no) DO UPDATE
		 SET title = excluded.title,
		     duration_sec = excluded.duration_sec,
		     source = excluded.source,
		     vid = excluded.vid,
		     play_url = excluded.play_url,
		     play_url_at = excluded.play_url_at
		 RETURNING id`,
		e.DramaID, e.EpNo, e.Title, e.DurationSec, e.Source, e.Vid, e.PlayURL, e.PlayURLAt,
	).Scan(&id)
	if err != nil {
		return 0, wrapWriteErr("写入集", err)
	}
	return id, nil
}

// EpisodeByID 按主键查一集。
//
// 无行返回 ErrNotFound；其余错误带上下文返回。
func (s *Store) EpisodeByID(ctx context.Context, id int64) (Episode, error) {
	row := s.db.QueryRowContext(ctx, episodeColumns+` WHERE id = ?`, id)
	e, err := scanEpisode(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Episode{}, ErrNotFound
	}
	if err != nil {
		return Episode{}, fmt.Errorf("查集 id=%d: %w", id, err)
	}
	return e, nil
}

// ListEpisodes 返回某部剧的全部集，按 ep_no 升序。
//
// (drama_id, ep_no) 唯一，因此顺序确定。
func (s *Store) ListEpisodes(ctx context.Context, dramaID int64) ([]Episode, error) {
	rows, err := s.db.QueryContext(ctx, episodeColumns+` WHERE drama_id = ? ORDER BY ep_no`, dramaID)
	if err != nil {
		return nil, fmt.Errorf("列集 drama_id=%d: %w", dramaID, err)
	}
	defer rows.Close()

	out := make([]Episode, 0)
	for rows.Next() {
		e, err := scanEpisode(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("读取集行: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历集 drama_id=%d: %w", dramaID, err)
	}
	return out, nil
}

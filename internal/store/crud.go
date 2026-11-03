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
	// Genre 是主类型（上游声明的第一个标签），WebDAV 的 `/<类型>/` 一级目录按它分组；
	// 空串表示上游未声明，目录兜底为「其他」。
	Genre string
	// Tags 是上游全部标签的 JSON 数组（UTF-8 TEXT，保序去重）。store 把它当不透明
	// 文本：不解析、不校验，只为「刷新下来的数据全部入库」的保真。
	Tags string
}

// Episode 是 episode 表的一行，对应某部剧的一集。
// ID 是输出字段：UpsertEpisode 忽略入参 ID。直链不落库（0004 已删 play_url /
// play_url_at），播放时由出流按需向插件解析。
type Episode struct {
	ID          int64
	DramaID     int64
	EpNo        int
	Title       string
	DurationSec int
	Source      string
	Vid         string
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
// （M2 批量入库、M3 一次枚举剧与集），因此共用一个 receiver。
type Store struct {
	db   dbtx
	root *sql.DB // 只有根 Store 非 nil；InTx 用它开事务

	// dirty 记录事务内是否发生过「可能改树结构」的写入；只有事务内的 Store 用它，
	// 由 InTx 在提交成功后递增版本号（见 changed 与 docs/adr/0015）。
	dirty bool
}

// NewStore 构造 Store。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, root: db}
}

// InTx 在同一事务内执行 fn：fn 返回错误即回滚并把该错误原样返回，返回 nil 则提交。
//
// 事务内的 *Store 复用同一批读写方法，SQL 与语义与根 Store 完全一致。fn 里不得做
// 网络或外部进程 I/O：长事务会持行锁、挡住同剧的其它写入（同步的取数阶段
// 必须在 InTx 之外完成）。
func (s *Store) InTx(ctx context.Context, fn func(tx *Store) error) error {
	if s.root == nil {
		return errors.New("store: 不支持嵌套事务")
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务: %w", err)
	}
	txStore := &Store{db: tx}
	if err := fn(txStore); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务: %w", err)
	}
	if txStore.dirty {
		bumpTreeRevision()
	}
	return nil
}

// changed 标记「树结构可能已变」。
//
// 根 Store（自动提交）的写入落库即生效，立即递增版本号；事务内的 Store 只打标记，
// 由 InTx 在提交成功后递增 —— 提交前递增会把「提交前读到的旧数据」存到新版本号下，
// 变成永久旧值（docs/adr/0015「失效模型」）。
func (s *Store) changed() {
	if s.root != nil {
		bumpTreeRevision()
		return
	}
	s.dirty = true
}

// dramaColumns 是 drama 表的显式列清单，顺序必须与 scanDrama 的 Scan 顺序一致。
const dramaColumns = `SELECT id, provider, provider_id, title, year, plot, cover_url, episode_total, sync_at, genre, tags FROM drama`

// episodeColumns 是 episode 表的显式列清单，顺序必须与 scanEpisode 的 Scan 顺序一致。
const episodeColumns = `SELECT id, drama_id, ep_no, title, duration_sec, source, vid FROM episode`

// scanDrama 按 dramaColumns 的顺序读出一行；scan 传 *sql.Row.Scan 或 *sql.Rows.Scan。
func scanDrama(scan func(dest ...any) error) (Drama, error) {
	var d Drama
	if err := scan(&d.ID, &d.Provider, &d.ProviderID, &d.Title, &d.Year, &d.Plot, &d.CoverURL, &d.EpisodeTotal, &d.SyncAt, &d.Genre, &d.Tags); err != nil {
		return Drama{}, err
	}
	return d, nil
}

// scanEpisode 按 episodeColumns 的顺序读出一行。
func scanEpisode(scan func(dest ...any) error) (Episode, error) {
	var e Episode
	if err := scan(&e.ID, &e.DramaID, &e.EpNo, &e.Title, &e.DurationSec, &e.Source, &e.Vid); err != nil {
		return Episode{}, err
	}
	return e, nil
}

// UpsertDrama 以 (provider, provider_id) 为身份写入或更新一部剧，返回行 id。
//
// 冲突时只更新可变态（title / year / plot / cover_url / episode_total / sync_at /
// genre / tags），provider 与 provider_id 是身份、不被覆盖。
//
// 入参 ID 被忽略，避免调用方陈旧 id 造成主键冲突。唯一/主键约束冲突返回 ErrConflict。
func (s *Store) UpsertDrama(ctx context.Context, d Drama) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO drama(provider, provider_id, title, year, plot, cover_url, episode_total, sync_at, genre, tags)
		 VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT(provider, provider_id) DO UPDATE
		 SET title = excluded.title,
		     year = excluded.year,
		     plot = excluded.plot,
		     cover_url = excluded.cover_url,
		     episode_total = excluded.episode_total,
		     sync_at = excluded.sync_at,
		     genre = excluded.genre,
		     tags = excluded.tags
		 RETURNING id`,
		d.Provider, d.ProviderID, d.Title, d.Year, d.Plot, d.CoverURL, d.EpisodeTotal, d.SyncAt,
		d.Genre, d.Tags,
	).Scan(&id)
	if err != nil {
		return 0, wrapWriteErr("写入剧目", err)
	}
	s.changed()
	return id, nil
}

// DramaByProviderID 按 (provider, provider_id) 查一部剧。
//
// 无行返回 ErrNotFound；其余错误带上下文返回。
func (s *Store) DramaByProviderID(ctx context.Context, provider, providerID string) (Drama, error) {
	row := s.db.QueryRowContext(ctx,
		dramaColumns+` WHERE provider = $1 AND provider_id = $2`, provider, providerID)
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
	row := s.db.QueryRowContext(ctx, dramaColumns+` WHERE id = $1`, id)
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

// ExistingProviderIDs 返回某个源已在库的 provider_id 集合。
//
// 一轮「新剧灌库」用它算差集（枚举到的标识里哪些库里还没有）：一次查询取全量身份，
// 不做「每部剧一次 SELECT」。集合是只读快照，调用方不要改它。
func (s *Store) ExistingProviderIDs(ctx context.Context, provider string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider_id FROM drama WHERE provider = $1`, provider)
	if err != nil {
		return nil, fmt.Errorf("列 %s 的已有作品标识: %w", provider, err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读取作品标识行: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 %s 的作品标识: %w", provider, err)
	}
	return out, nil
}

// StaleProviderIDs 返回某个源里 sync_at 最旧的 limit 部作品的 provider_id，按 sync_at
// 升序（同一时间按 id 升序保证顺序确定）。
//
// 一轮「存量轮转」用它挑这一批要刷新的剧。limit <= 0 返回空切片：refresh_batch=0
// 表示关闭存量轮转，不是报错。
func (s *Store) StaleProviderIDs(ctx context.Context, provider string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider_id FROM drama WHERE provider = $1 ORDER BY sync_at, id LIMIT $2`,
		provider, limit)
	if err != nil {
		return nil, fmt.Errorf("列 %s 最旧的 %d 部作品: %w", provider, limit, err)
	}
	defer rows.Close()

	out := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读取待刷新作品标识: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 %s 最旧作品: %w", provider, err)
	}
	return out, nil
}

// UpsertEpisode 以 (drama_id, ep_no) 为身份写入或更新一集，返回行 id。
//
// 冲突时只更新可变态（title / duration_sec / source / vid），drama_id 与 ep_no 是
// 身份、不被覆盖。入参 ID 被忽略。唯一/主键约束冲突返回 ErrConflict。
//
// duration_sec 只在插件给了非零值时覆盖：插件批量拿不到时长（红果恒为 0），而首播
// 建索引时回填的是 ffprobe 实测值（BackfillEpisodeDuration）—— 刷新若把它写回 0，
// 回填就每轮被冲掉一次（`.nfo` 的 `<runtime>` 回到 0）。
func (s *Store) UpsertEpisode(ctx context.Context, e Episode) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO episode(drama_id, ep_no, title, duration_sec, source, vid)
		 VALUES($1, $2, $3, $4, $5, $6)
		 ON CONFLICT(drama_id, ep_no) DO UPDATE
		 SET title = excluded.title,
		     duration_sec = CASE WHEN excluded.duration_sec > 0
		                         THEN excluded.duration_sec
		                         ELSE episode.duration_sec END,
		     source = excluded.source,
		     vid = excluded.vid
		 RETURNING id`,
		e.DramaID, e.EpNo, e.Title, e.DurationSec, e.Source, e.Vid,
	).Scan(&id)
	if err != nil {
		return 0, wrapWriteErr("写入集", err)
	}
	s.changed()
	return id, nil
}

// BackfillEpisodeDuration 用实测时长回填 episode.duration_sec。
//
// 只填库里的 0（首播建索引时 ffprobe 的实测那次）：已有非零值一律不动，重复播放与
// 索引重建都不覆盖更权威的来源。durationSec <= 0（探测不到时长的异常源）视为没有
// 实测值，不写库。返回是否真的写了行。
//
// 不回写任何派生数据：对外集数的 runtime 由 ListEmbyItems 派生时按当前 episode
// 现算（单集取本集时长、合并集取组内之和），所以回填后下一次读取就是真实值
// （docs/adr/0014）。
func (s *Store) BackfillEpisodeDuration(ctx context.Context, episodeID int64, durationSec int) (bool, error) {
	if durationSec <= 0 {
		return false, nil
	}
	changed := false
	err := s.InTx(ctx, func(tx *Store) error {
		// 锁序必须是 drama → episode：先读所属剧目（普通读，不加锁），再锁 drama，
		// 最后才 UPDATE episode。反过来（先改集、再锁剧目）会与同步事务构成环：
		// 同步持 drama 等某集，回填持该集等 drama → 40P01 死锁。
		var dramaID int64
		err := tx.db.QueryRowContext(ctx,
			`SELECT drama_id FROM episode WHERE id = $1`, episodeID).Scan(&dramaID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // 该集不存在：与「已有非零值」一样不动
		}
		if err != nil {
			return fmt.Errorf("查回填集所属剧目 episode_id=%d: %w", episodeID, err)
		}
		if err := tx.lockDrama(ctx, dramaID); err != nil {
			return err
		}
		res, err := tx.db.ExecContext(ctx,
			`UPDATE episode SET duration_sec = $1 WHERE id = $2 AND duration_sec = 0`,
			durationSec, episodeID)
		if err != nil {
			return wrapWriteErr("回填集时长", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("回填集时长 episode_id=%d: %w", episodeID, err)
		}
		if n == 0 {
			return nil // 已有非零值或该集已被删：都不动
		}
		changed = true
		tx.changed()
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// EpisodeByID 按主键查一集。
//
// 无行返回 ErrNotFound；其余错误带上下文返回。
func (s *Store) EpisodeByID(ctx context.Context, id int64) (Episode, error) {
	row := s.db.QueryRowContext(ctx, episodeColumns+` WHERE id = $1`, id)
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
	return s.listEpisodes(ctx,
		fmt.Sprintf("列集 drama_id=%d", dramaID),
		episodeColumns+` WHERE drama_id = $1 ORDER BY ep_no`, dramaID)
}

// listEpisodes 是 ListEpisodes 的实现入口：what 只用于错误文本。
func (s *Store) listEpisodes(ctx context.Context, what, query string, args ...any) ([]Episode, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
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
		return nil, fmt.Errorf("遍历%s: %w", what, err)
	}
	return out, nil
}

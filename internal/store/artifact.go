package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// artifact 表的 kind 取值（docs/DATA-MODEL.md「0003」的 CHECK 约束）。
const (
	ArtifactKindCover   = "cover"   // 海报：key = <provider>/<provider_id>
	ArtifactKindIndex   = "index"   // 关键帧索引：key = uid
	ArtifactKindSegment = "segment" // 视频分片：key = sha256(uid \x1f seq \x1f codec_sig)
)

// Artifact 是 artifact 表的一行：一份本地缓存资产（海报 / 关键帧索引 / 分片）。
//
// Path 是相对或绝对的本地文件路径，Bytes 是文件大小（供淘汰算总量），SHA256 是
// 内容哈希（分片用），LastUsedAt 是不透明 TEXT（UTC RFC3339），语义同 Drama.SyncAt。
// 表里不存上游 URL：key 是内容/身份哈希、path 是本地路径，换链不会留下痕迹。
type Artifact struct {
	Kind       string
	Key        string
	Path       string
	Bytes      int64
	SHA256     string
	LastUsedAt string
}

// artifactColumns 是 artifact 的显式列清单，顺序必须与 scanArtifact 一致。
const artifactColumns = `SELECT kind, key, path, bytes, sha256, last_used_at FROM artifact`

// scanArtifact 按 artifactColumns 的顺序读出一行。
func scanArtifact(scan func(dest ...any) error) (Artifact, error) {
	var a Artifact
	if err := scan(&a.Kind, &a.Key, &a.Path, &a.Bytes, &a.SHA256, &a.LastUsedAt); err != nil {
		return Artifact{}, err
	}
	return a, nil
}

// ArtifactByKey 按 (kind, key) 查一份缓存资产的索引行。
//
// 无行返回 ErrNotFound —— 调用方据此决定「回源还是直接用本地文件」。
func (s *Store) ArtifactByKey(ctx context.Context, kind, key string) (Artifact, error) {
	row := s.db.QueryRowContext(ctx, artifactColumns+` WHERE kind = ? AND key = ?`, kind, key)
	a, err := scanArtifact(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("查缓存资产 %s/%s: %w", kind, key, err)
	}
	return a, nil
}

// PutArtifact 以 (kind, key) 为身份写入或更新一份缓存资产的索引行。
//
// 冲突时覆盖 path / bytes / sha256 / last_used_at（同键的内容只会是同一个内容，
// 覆盖等价于刷新使用时间）。kind 不在三个合法取值内时由 schema 的 CHECK 拒绝。
func (s *Store) PutArtifact(ctx context.Context, a Artifact) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO artifact(kind, key, path, bytes, sha256, last_used_at)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(kind, key) DO UPDATE
		 SET path = excluded.path,
		     bytes = excluded.bytes,
		     sha256 = excluded.sha256,
		     last_used_at = excluded.last_used_at`,
		a.Kind, a.Key, a.Path, a.Bytes, a.SHA256, a.LastUsedAt,
	); err != nil {
		return wrapWriteErr("写入缓存资产 "+a.Kind+"/"+a.Key, err)
	}
	return nil
}

// TouchArtifact 把一份缓存资产的 last_used_at 更新为 at（「命中即 touch」）。
//
// 淘汰按 last_used_at 排序，不做 touch 会把仍在用的条目当最久未用先淘汰
// （docs/CACHE-MODEL.md「淘汰」）。该行不存在返回 ErrNotFound —— 调用方通常忽略：
// 丢一次 touch 只让这条更早被淘汰，不影响正确性。
func (s *Store) TouchArtifact(ctx context.Context, kind, key, at string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE artifact SET last_used_at = ? WHERE kind = ? AND key = ?`, at, kind, key)
	if err != nil {
		return fmt.Errorf("更新缓存资产 %s/%s: %w", kind, key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("更新缓存资产 %s/%s: %w", kind, key, err)
	}
	if n == 0 {
		return fmt.Errorf("更新缓存资产 %s/%s: %w", kind, key, ErrNotFound)
	}
	return nil
}

// DeleteArtifact 删除一份缓存资产的索引行。无该行返回 ErrNotFound。
//
// 淘汰「先删文件、再删行」（docs/CACHE-MODEL.md「淘汰」第 3 步），所以行不存在
// 对调用方是自愈情形，不是异常。
func (s *Store) DeleteArtifact(ctx context.Context, kind, key string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM artifact WHERE kind = ? AND key = ?`, kind, key)
	if err != nil {
		return fmt.Errorf("删除缓存资产 %s/%s: %w", kind, key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("删除缓存资产 %s/%s: %w", kind, key, err)
	}
	if n == 0 {
		return fmt.Errorf("删除缓存资产 %s/%s: %w", kind, key, ErrNotFound)
	}
	return nil
}

// SumArtifactBytes 返回某类缓存资产的 bytes 合计，按上限淘汰时用它算总量。
//
// 只按 kind 过滤（分片参与大小淘汰，海报与索引不参与），见
// docs/CACHE-MODEL.md「淘汰」第 2 步。
func (s *Store) SumArtifactBytes(ctx context.Context, kind string) (int64, error) {
	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(bytes), 0) FROM artifact WHERE kind = ?`, kind).Scan(&total); err != nil {
		return 0, fmt.Errorf("统计缓存资产 %s 的字节数: %w", kind, err)
	}
	return total, nil
}

// ListArtifactsOldest 按 last_used_at 升序返回某类缓存资产的最多 limit 行。
//
// 供「超上限」淘汰：先删最久未用的，直到 SUM(bytes) 落到上限内。走
// idx_artifact_kind_last_used，取一批删一批，避免长事务。
func (s *Store) ListArtifactsOldest(ctx context.Context, kind string, limit int) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx,
		artifactColumns+` WHERE kind = ? ORDER BY last_used_at LIMIT ?`, kind, limit)
	if err != nil {
		return nil, fmt.Errorf("列出最久未用的缓存资产 %s: %w", kind, err)
	}
	return collectArtifacts(rows, "最久未用的缓存资产 "+kind)
}

// ListExpiredArtifacts 按 last_used_at 升序返回 last_used_at 早于 before 的最多
// limit 行，三种 kind 都算（过期是「多久没命中」，与类型无关）。
//
// before 是与 last_used_at 同格式的 UTC RFC3339 文本：该列是同格式定长文本，
// 字符串比较即时间比较。
func (s *Store) ListExpiredArtifacts(ctx context.Context, before string, limit int) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx,
		artifactColumns+` WHERE last_used_at < ? ORDER BY last_used_at LIMIT ?`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("列出过期的缓存资产: %w", err)
	}
	return collectArtifacts(rows, "过期的缓存资产")
}

// collectArtifacts 把行游标读完（含 rows.Err），失败时带上 op 上下文。
func collectArtifacts(rows *sql.Rows, op string) ([]Artifact, error) {
	defer func() { _ = rows.Close() }()
	out := make([]Artifact, 0)
	for rows.Next() {
		a, err := scanArtifact(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("读取%s: %w", op, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历%s: %w", op, err)
	}
	return out, nil
}

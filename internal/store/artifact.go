package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// artifact 表的 kind 取值（docs/DATA-MODEL.md「0003」「0006」的 CHECK 约束）。
const (
	ArtifactKindCover   = "cover"   // 海报：key = <provider>/<provider_id>
	ArtifactKindSegment = "segment" // 视频分片：key = sha256("e<episode.id>" \x1f channel)
)

// Artifact 是 artifact 表的一行：一份本地缓存资产（海报 / 分片）。
//
// Path 是相对或绝对的本地文件路径，Bytes 是文件大小（供淘汰算总量），SHA256 是
// 内容哈希（分片用），LastUsedAt 是不透明 TEXT（UTC RFC3339），语义同 Drama.SyncAt。
// 表里不存上游 URL：key 是内容/身份哈希、path 是本地路径，换链不会留下痕迹。
//
// Prefetched 是「后台预热生成、还没有被任何一次播放请求用过」的标记：容量吃紧时
// 这类分片**先被淘汰**，不挤掉用户真正看过的缓存（docs/CACHE-MODEL.md「淘汰」）。
// 前台命中或前台生成成功会把它清回 false（MarkArtifactPlayed）。
type Artifact struct {
	Kind       string
	Key        string
	Path       string
	Bytes      int64
	SHA256     string
	LastUsedAt string
	Prefetched bool
}

// artifactColumns 是 artifact 的显式列清单，顺序必须与 scanArtifact 一致。
const artifactColumns = `SELECT kind, key, path, bytes, sha256, last_used_at, prefetched FROM artifact`

// scanArtifact 按 artifactColumns 的顺序读出一行。
func scanArtifact(scan func(dest ...any) error) (Artifact, error) {
	var a Artifact
	if err := scan(&a.Kind, &a.Key, &a.Path, &a.Bytes, &a.SHA256, &a.LastUsedAt, &a.Prefetched); err != nil {
		return Artifact{}, err
	}
	return a, nil
}

// ArtifactByKey 按 (kind, key) 查一份缓存资产的索引行。
//
// 无行返回 ErrNotFound —— 调用方据此决定「回源还是直接用本地文件」。
func (s *Store) ArtifactByKey(ctx context.Context, kind, key string) (Artifact, error) {
	row := s.db.QueryRowContext(ctx, artifactColumns+` WHERE kind = $1 AND key = $2`, kind, key)
	a, err := scanArtifact(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("查缓存资产 %s/%s: %w", kind, key, err)
	}
	return a, nil
}

// CoverRef 是一部剧的海报身份：artifact 键 `<provider>/<provider_id>` 的结构化形态。
// 键的拼法只在本包（CoverRefs 负责拆），调用方不自己拼字符串。
type CoverRef struct {
	Provider   string
	ProviderID string
}

// CoverRefs 返回全部已登记海报的身份集合（kind='cover'）。
//
// 剧目列表用它一次算出每部剧的 has_cover：一次 kind='cover' 的扫描，代价是该表封面
// 行数，不按剧逐个点查（那会变成 N+1）。行在即「有封面」——文件被淘汰后行仍保留
// （见 internal/cache 的 hitFile 注释），列表不会因为淘汰而闪成占位块。
func (s *Store) CoverRefs(ctx context.Context) (map[CoverRef]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM artifact WHERE kind = $1`, ArtifactKindCover)
	if err != nil {
		return nil, fmt.Errorf("列海报键: %w", err)
	}
	defer rows.Close()
	out := make(map[CoverRef]struct{})
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("读海报键: %w", err)
		}
		// 键恒为 `<provider>/<provider_id>`；provider 是插件名，不含 `/`，
		// 所以按第一个 `/` 切分即可还原。
		provider, providerID, ok := strings.Cut(key, "/")
		if !ok {
			continue
		}
		out[CoverRef{Provider: provider, ProviderID: providerID}] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列海报键: %w", err)
	}
	return out, nil
}

// PutArtifact 以 (kind, key) 为身份写入或更新一份缓存资产的索引行。
//
// 冲突时覆盖 path / bytes / sha256 / last_used_at（同键的内容只会是同一个内容，
// 覆盖等价于刷新使用时间）。kind 不在合法取值内时由 schema 的 CHECK 拒绝。
//
// prefetched 只升不降：`artifact.prefetched AND excluded.prefetched` —— 一条被前台
// 播放过的分片（prefetched=false）再被预热写入时仍是 false，预热不会把「用户看过」
// 降级成「可优先淘汰」（docs/CACHE-MODEL.md「淘汰」）。
func (s *Store) PutArtifact(ctx context.Context, a Artifact) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO artifact(kind, key, path, bytes, sha256, last_used_at, prefetched)
		 VALUES($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT(kind, key) DO UPDATE
		 SET path = excluded.path,
		     bytes = excluded.bytes,
		     sha256 = excluded.sha256,
		     last_used_at = excluded.last_used_at,
		     prefetched = artifact.prefetched AND excluded.prefetched`,
		a.Kind, a.Key, a.Path, a.Bytes, a.SHA256, a.LastUsedAt, a.Prefetched,
	); err != nil {
		return wrapWriteErr("写入缓存资产 "+a.Kind+"/"+a.Key, err)
	}
	if a.Kind == ArtifactKindCover {
		// 封面行同时是「这部剧有封面」的真值：写/删/释放都会改变剧目录里的
		// poster.jpg 是否出现，必须让结构缓存失效。分片不影响树形。
		s.changed()
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
		`UPDATE artifact SET last_used_at = $1 WHERE kind = $2 AND key = $3`, at, kind, key)
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

// MarkArtifactPlayed 把一份缓存资产标记为「前台真正用过」：last_used_at 推到 at，
// 并清掉 prefetched 标记（MKACG-682）。
//
// 前台命中与前台生成成功都走这里 —— 一旦用户真的播到这一片，它就不再是
// 「预热但没播过」的候选，容量吃紧时不会被优先淘汰。无该行返回 ErrNotFound，
// 调用方与 TouchArtifact 一样可以忽略（丢一次只影响淘汰顺序）。
func (s *Store) MarkArtifactPlayed(ctx context.Context, kind, key, at string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE artifact SET last_used_at = $1, prefetched = FALSE WHERE kind = $2 AND key = $3`,
		at, kind, key)
	if err != nil {
		return fmt.Errorf("标记缓存资产 %s/%s: %w", kind, key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("标记缓存资产 %s/%s: %w", kind, key, err)
	}
	if n == 0 {
		return fmt.Errorf("标记缓存资产 %s/%s: %w", kind, key, ErrNotFound)
	}
	return nil
}

// DeleteArtifact 删除一份缓存资产的索引行。无该行返回 ErrNotFound。
//
// 淘汰「先删文件、再删行」（docs/CACHE-MODEL.md「淘汰」第 3 步），所以行不存在
// 对调用方是自愈情形，不是异常。
func (s *Store) DeleteArtifact(ctx context.Context, kind, key string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM artifact WHERE kind = $1 AND key = $2`, kind, key)
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
	if kind == ArtifactKindCover {
		s.changed()
	}
	return nil
}

// ReleaseArtifact 把一份缓存资产降级为「只有登记、没有本地字节」：清空
// path / bytes / sha256，并把 last_used_at 推到 at。行留着。
//
// 只有海报用它（docs/CACHE-MODEL.md「淘汰」第 3 步）：封面行同时是「这部剧有封面」
// 的唯一真值（`vfs` 据此挂出 `poster.jpg`），连行一起删会让节点从树里消失，
// 「淘汰后按需回源」就永远没有发生的入口。清空 path 表达「本地没有字节」，
// 下一次请求按未命中处理并重新落盘（PutArtifact 覆盖回真实值）。
//
// last_used_at 必须推到 at：留着旧值会让过期查询下一轮又选中这一行，淘汰循环原地打转。
// 无该行返回 ErrNotFound。
func (s *Store) ReleaseArtifact(ctx context.Context, kind, key, at string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE artifact
		 SET path = '', bytes = 0, sha256 = '', last_used_at = $1
		 WHERE kind = $2 AND key = $3`, at, kind, key)
	if err != nil {
		return fmt.Errorf("释放缓存资产 %s/%s: %w", kind, key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("释放缓存资产 %s/%s: %w", kind, key, err)
	}
	if n == 0 {
		return fmt.Errorf("释放缓存资产 %s/%s: %w", kind, key, ErrNotFound)
	}
	if kind == ArtifactKindCover {
		s.changed()
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
		`SELECT COALESCE(SUM(bytes), 0) FROM artifact WHERE kind = $1`, kind).Scan(&total); err != nil {
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
		artifactColumns+` WHERE kind = $1 ORDER BY last_used_at LIMIT $2`, kind, limit)
	if err != nil {
		return nil, fmt.Errorf("列出最久未用的缓存资产 %s: %w", kind, err)
	}
	return collectArtifacts(rows, "最久未用的缓存资产 "+kind)
}

// ListArtifactsOldestPrefetched 按 last_used_at 升序返回「预热但没播过」的分片行，
// 最多 limit 行（MKACG-682）。
//
// 供「超上限」淘汰的第一优先级：先清预热出来的分片，再动用户真正看过的缓存。
func (s *Store) ListArtifactsOldestPrefetched(ctx context.Context, kind string, limit int) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx,
		artifactColumns+` WHERE kind = $1 AND prefetched ORDER BY last_used_at LIMIT $2`, kind, limit)
	if err != nil {
		return nil, fmt.Errorf("列出预热的缓存资产 %s: %w", kind, err)
	}
	return collectArtifacts(rows, "预热的缓存资产 "+kind)
}

// ListExpiredArtifacts 按 last_used_at 升序返回 last_used_at 早于 before 的最多
// limit 行，两种 kind 都算（过期是「多久没命中」，与类型无关）。
//
// before 是与 last_used_at 同格式的 UTC RFC3339 文本：该列是同格式定长文本，
// 字符串比较即时间比较。
func (s *Store) ListExpiredArtifacts(ctx context.Context, before string, limit int) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx,
		artifactColumns+` WHERE last_used_at < $1 ORDER BY last_used_at LIMIT $2`, before, limit)
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

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

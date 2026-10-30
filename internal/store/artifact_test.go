package store

import (
	"context"
	"errors"
	"testing"
)

// AC：artifact 索引行按 (kind, key) 读写；未命中原样返回 ErrNotFound，同键 upsert 覆盖。
func TestArtifactRoundTrip(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	if _, err := s.ArtifactByKey(ctx, ArtifactKindCover, "web/p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未命中的错误 = %v, 期望 ErrNotFound", err)
	}

	want := Artifact{
		Kind: ArtifactKindCover, Key: "web/p1",
		Path: "/data/cache/cover/aa/aa.jpg", Bytes: 100,
		SHA256: "deadbeef", LastUsedAt: "2026-10-30T09:00:00Z",
	}
	if err := s.PutArtifact(ctx, want); err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}
	got, err := s.ArtifactByKey(ctx, want.Kind, want.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey: %v", err)
	}
	if got != want {
		t.Fatalf("读回 = %+v, 期望 %+v", got, want)
	}

	// 同键（同内容）重写：覆盖为新的 path/last_used_at，行数不变。
	updated := want
	updated.Bytes = 200
	updated.LastUsedAt = "2026-10-30T10:00:00Z"
	if err := s.PutArtifact(ctx, updated); err != nil {
		t.Fatalf("PutArtifact(覆盖): %v", err)
	}
	got, err = s.ArtifactByKey(ctx, updated.Kind, updated.Key)
	if err != nil {
		t.Fatalf("ArtifactByKey(覆盖后): %v", err)
	}
	if got != updated {
		t.Fatalf("覆盖后读回 = %+v, 期望 %+v", got, updated)
	}
	if n := countRows(t, s.db, "artifact"); n != 1 {
		t.Fatalf("artifact 行数 = %d, 期望 1", n)
	}

	// 同一个 key 不同 kind 是两行（主键是 (kind, key)）。
	index := Artifact{Kind: ArtifactKindIndex, Key: want.Key, Path: "/data/cache/index/aa/aa.json",
		Bytes: 1, LastUsedAt: "2026-10-30T10:00:00Z"}
	if err := s.PutArtifact(ctx, index); err != nil {
		t.Fatalf("PutArtifact(index): %v", err)
	}
	if n := countRows(t, s.db, "artifact"); n != 2 {
		t.Fatalf("artifact 行数 = %d, 期望 2", n)
	}
}

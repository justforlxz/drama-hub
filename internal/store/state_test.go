package store

import (
	"context"
	"errors"
	"testing"
)

// AC：source_state 五列可读写；未跑过的源返回 ErrNotFound。
func TestSourceStateRoundTrip(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	if got, err := s.LoadState(ctx, "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未跑的源 LoadState = (%+v, %v), 期望 ErrNotFound", got, err)
	}

	empty := SourceState{Name: "web"}
	if err := s.SaveState(ctx, empty); err != nil {
		t.Fatalf("SaveState(空状态): %v", err)
	}
	got, err := s.LoadState(ctx, "web")
	if err != nil {
		t.Fatalf("LoadState(空状态): %v", err)
	}
	if got != empty {
		t.Fatalf("空状态读回 = %+v, 期望 %+v", got, empty)
	}

	// 五列全部有值：游标是插件不透明文本、三个时间是 UTC RFC3339 文本。
	want := SourceState{
		Name:      "web",
		Cursor:    "sitemap:page=7",
		LastRunAt: "2026-10-30T09:00:00Z",
		LastOkAt:  "2026-10-30T09:05:00Z",
		LastError: "2026-10-30T10:00:00Z 读取 sitemap 超时",
	}
	if err := s.SaveState(ctx, want); err != nil {
		t.Fatalf("SaveState(全字段): %v", err)
	}
	got, err = s.LoadState(ctx, "web")
	if err != nil {
		t.Fatalf("LoadState(全字段): %v", err)
	}
	if got != want {
		t.Fatalf("读回 = %+v, 期望 %+v", got, want)
	}

	// 成功一轮后清空 last_error：全量覆盖语义让「清空」写成同一行的一次 upsert。
	ok := want
	ok.Cursor = "sitemap:page=8"
	ok.LastOkAt = "2026-10-30T11:00:00Z"
	ok.LastError = ""
	if err := s.SaveState(ctx, ok); err != nil {
		t.Fatalf("SaveState(成功后): %v", err)
	}
	got, err = s.LoadState(ctx, "web")
	if err != nil {
		t.Fatalf("LoadState(成功后): %v", err)
	}
	if got != ok {
		t.Fatalf("成功后读回 = %+v, 期望 %+v", got, ok)
	}

	// 每源一行：另一个源不受影响。
	if err := s.SaveState(ctx, SourceState{Name: "other", Cursor: "c2"}); err != nil {
		t.Fatalf("SaveState(other): %v", err)
	}
	if got, err := s.LoadState(ctx, "web"); err != nil || got != ok {
		t.Fatalf("web 状态被 other 影响: (%+v, %v)", got, err)
	}
	if n := countRows(t, s.db, "source_state"); n != 2 {
		t.Fatalf("source_state 行数 = %d, 期望 2", n)
	}
}

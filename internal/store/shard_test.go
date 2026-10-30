package store

import (
	"context"
	"testing"
)

// 分片是纯函数：同一身份永远同一分片，且落在 [0, ShardCount)。
func TestShardOfDeterministic(t *testing.T) {
	ids := []string{"7503745441910017049", "p1", "full-1", "", "另一部剧"}
	for _, id := range ids {
		first := ShardOf(id)
		if first < 0 || first >= ShardCount {
			t.Fatalf("ShardOf(%q) = %d，越出 [0,%d)", id, first, ShardCount)
		}
		if got := ShardOf(id); got != first {
			t.Fatalf("ShardOf(%q) 两次 = %d / %d，期望稳定", id, first, got)
		}
	}

	// 钉一个实测值：分片号是目录契约的一部分（docs/DIRECTORY-CONTRACT.md「D2」），
	// 算法或 N 变了必须显式改这条断言，而不是让用户目录静默搬家。
	if got := ShardOf("7503745441910017049"); got != 26 {
		t.Fatalf("ShardOf(红果 fixture id) = %d, 期望 26", got)
	}
}

// shard 是派生列：UpsertDrama 按 provider_id 重算，入参 Shard 被忽略。
func TestUpsertDramaDerivesShard(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	id, err := s.UpsertDrama(ctx, Drama{
		Provider: "web", ProviderID: "p1", Title: "剧",
		Shard: 999, // 调用方算错/没算都不影响落库值
	})
	if err != nil {
		t.Fatalf("UpsertDrama: %v", err)
	}
	d, err := s.DramaByID(ctx, id)
	if err != nil {
		t.Fatalf("DramaByID: %v", err)
	}
	if want := ShardOf("p1"); d.Shard != want {
		t.Fatalf("shard = %d, 期望按身份派生 %d", d.Shard, want)
	}
}

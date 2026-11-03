package store

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// TestJellyfinCategorySwitch 锁定分类开关的持久化语义（MKACG-668）：默认全开、
// 关掉即落库（重复关幂等）、重新开启删行、列表按名字升序。
func TestJellyfinCategorySwitch(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	got, err := s.DisabledJellyfinCategories(ctx)
	if err != nil {
		t.Fatalf("空表 DisabledJellyfinCategories: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("空表关闭项 = %v, 期望空（行不存在 = 启用）", got)
	}

	for _, category := range []string{"爱情", "都市"} {
		if err := s.SetJellyfinCategoryEnabled(ctx, category, false); err != nil {
			t.Fatalf("关闭 %s: %v", category, err)
		}
	}
	// 重复关闭是幂等的（upsert 只刷新 updated_at）。
	if err := s.SetJellyfinCategoryEnabled(ctx, "都市", false); err != nil {
		t.Fatalf("重复关闭: %v", err)
	}
	got, err = s.DisabledJellyfinCategories(ctx)
	if err != nil {
		t.Fatalf("DisabledJellyfinCategories: %v", err)
	}
	if want := []string{"爱情", "都市"}; !slices.Equal(got, want) {
		t.Errorf("关闭项 = %v, 期望 %v（升序、不重复）", got, want)
	}

	// 重新开启 = 删行；再开一次也不报错。
	if err := s.SetJellyfinCategoryEnabled(ctx, "都市", true); err != nil {
		t.Fatalf("开启: %v", err)
	}
	if err := s.SetJellyfinCategoryEnabled(ctx, "都市", true); err != nil {
		t.Fatalf("重复开启: %v", err)
	}
	got, err = s.DisabledJellyfinCategories(ctx)
	if err != nil {
		t.Fatalf("DisabledJellyfinCategories: %v", err)
	}
	if want := []string{"爱情"}; !slices.Equal(got, want) {
		t.Errorf("开启后关闭项 = %v, 期望 %v", got, want)
	}
}

// TestJellyfinCategorySwitchEmpty 锁定空分类名：拒绝（ErrInvalidSetting），不写库。
func TestJellyfinCategorySwitchEmpty(t *testing.T) {
	s := NewStore(newCRUDTestDB(t).DB)
	ctx := context.Background()

	if err := s.SetJellyfinCategoryEnabled(ctx, "  ", false); !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("空分类名 = %v, 期望 ErrInvalidSetting", err)
	}
	if got, err := s.DisabledJellyfinCategories(ctx); err != nil || len(got) != 0 {
		t.Errorf("空分类名写入了库：%v, %v", got, err)
	}
}

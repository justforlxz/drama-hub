package lru

import "testing"

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := New[int](2, func(int) int { return 1 })
	c.Put("a", 1)
	c.Put("b", 2)
	if _, ok := c.Get("a"); !ok { // a 变成最近使用
		t.Fatal("a 应命中")
	}
	c.Put("c", 3) // 超容量，淘汰最久未用的 b
	if _, ok := c.Get("b"); ok {
		t.Fatal("b 应被淘汰")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a 应保留")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c 应保留")
	}
}

func TestCacheReplacesSameKey(t *testing.T) {
	c := New[int](4, func(int) int { return 1 })
	c.Put("a", 1)
	c.Put("a", 2)
	if v, ok := c.Get("a"); !ok || v != 2 {
		t.Fatalf("Get(a) = %d, %v; 期望 2, true", v, ok)
	}
}

// 单条成本就超预算时不能把它自己挤掉，否则缓存永远为空。
func TestCacheKeepsOversizeSingleEntry(t *testing.T) {
	c := New[int](3, func(int) int { return 5 })
	c.Put("big", 1)
	if _, ok := c.Get("big"); !ok {
		t.Fatal("单条超预算的条目应保留")
	}
}

// 容量按成本之和算：塞进 10 + 20 超出预算 25，淘汰队尾的 10。
func TestCacheEvictsByCost(t *testing.T) {
	c := New[int](25, func(v int) int { return v })
	c.Put("a", 10)
	c.Put("b", 20)
	if _, ok := c.Get("a"); ok {
		t.Fatal("a 的 10 加 b 的 20 超预算，a 应被淘汰")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b 应保留")
	}
}

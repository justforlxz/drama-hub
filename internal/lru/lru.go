// Package lru 是按「成本」计容量的最近最少使用缓存。
//
// 成本由调用方给（节点条数、响应字节数……）：缓存的上限天然是「内存占用」而不是
// 「条目数」，用条数当上限会让大目录把内存顶穿。命中与写入都把条目移到队首，超限
// 时从队尾淘汰；单个条目自己就超限时仍会保留（没有它缓存就永远不命中，且调用方
// 已为它分配过内存），所以容量是软上限。
//
// 并发安全。存进去的值按不可变对待：Get 返回的是同一个引用，调用方不得修改。
package lru

import (
	"container/list"
	"sync"
)

// Cache 是最近最少使用缓存。零值不可用，用 New 构造。
type Cache[V any] struct {
	mu       sync.Mutex
	capacity int
	cost     func(V) int
	total    int
	order    *list.List // 队首是最新使用，队尾是最久未用
	items    map[string]*list.Element
}

// entry 是队列元素的值。
type entry[V any] struct {
	key   string
	value V
	cost  int
}

// New 构造容量为 capacity 的缓存：所有条目 cost 之和超过 capacity 时从队尾淘汰。
// cost 不可为 nil，必须对同一个值稳定返回非负成本。
func New[V any](capacity int, cost func(V) int) *Cache[V] {
	if capacity < 0 {
		capacity = 0
	}
	return &Cache[V]{
		capacity: capacity,
		cost:     cost,
		order:    list.New(),
		items:    make(map[string]*list.Element),
	}
}

// Get 取一个条目并把它标记为最新使用。
func (c *Cache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*entry[V]).value, true
	}
	var zero V
	return zero, false
}

// Put 写入或替换一个条目，随后按容量淘汰最久未用的条目。
func (c *Cache[V]) Put(key string, value V) {
	n := c.cost(value)
	if n < 0 {
		n = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[V])
		c.total += n - e.cost
		e.value, e.cost = value, n
		c.order.MoveToFront(el)
	} else {
		c.items[key] = c.order.PushFront(&entry[V]{key: key, value: value, cost: n})
		c.total += n
	}
	// 至少留一条：自己就超限的条目不能被自己挤掉。
	for c.total > c.capacity && c.order.Len() > 1 {
		c.removeElement(c.order.Back())
	}
}

// removeElement 摘除一个队列元素；调用方必须持有 c.mu。
func (c *Cache[V]) removeElement(el *list.Element) {
	e := el.Value.(*entry[V])
	c.order.Remove(el)
	delete(c.items, e.key)
	c.total -= e.cost
}

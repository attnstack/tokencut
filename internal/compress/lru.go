// lru.go — concurrency-safe LRU cache used for the compression-result cache.
// Why in-memory instead of SQLite: the cache sits on the hot path with a
// p99 < 1ms target; an in-process LRU gives O(1) with no I/O. Persistence is
// delegated to the JSONL observation log, which is append-only and cheap.
package compress

import (
	"container/list"
	"sync"
)

type lruEntry struct {
	key string
	val any
}

type LRU struct {
	capacity  int
	mu        sync.Mutex
	ll        *list.List
	items     map[string]*list.Element
	Hits      uint64
	Misses    uint64
	Evictions uint64
}

func NewLRU(capacity int) *LRU {
	if capacity < 1 {
		capacity = 1
	}
	return &LRU{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[string]*list.Element, capacity),
	}
}

func (c *LRU) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		c.Hits++
		return el.Value.(*lruEntry).val, true
	}
	c.Misses++
	return nil, false
}

func (c *LRU) Put(key string, val any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*lruEntry).val = val
		return
	}
	c.items[key] = c.ll.PushFront(&lruEntry{key: key, val: val})
	if c.ll.Len() > c.capacity {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*lruEntry).key)
			c.Evictions++
		}
	}
}

func (c *LRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

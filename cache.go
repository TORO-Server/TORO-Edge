package main

import (
	"container/list"
	"sync"
	"sync/atomic"
)

type listElem = list.Element

// EntityCache は大きさの上限があるメモリのキャッシュ (使われていないものから捨てる)。
// 鍵から返事を引くのは、名前の空間 (nsCache) ごとの sync.Map で、ヒットのときは鍵を作り直さず、ロックも取らない。
// 使った順の並びと大きさの合計だけは、1 つのロック (mu) で守る (並べ替えは 2 秒に 1 回までなので、ほとんど取らない)
type EntityCache struct {
	mu        sync.Mutex
	order     *list.List // 先頭が最近使ったもの
	total     int64
	limit     int64
	evictions atomic.Int64
	spaces    []*nsCache
}

// nsCache は名前の空間 (同じパスでも、サイトや使い道が違えば別のものをためるため)
type nsCache struct {
	parent *EntityCache
	m      sync.Map // string → *Entity
}

func newEntityCache(limit int64) *EntityCache {
	return &EntityCache{order: list.New(), limit: limit}
}

// space は新しい名前の空間を作る
func (c *EntityCache) space() *nsCache {
	n := &nsCache{parent: c}
	c.mu.Lock()
	c.spaces = append(c.spaces, n)
	c.mu.Unlock()
	return n
}

func (c *EntityCache) stats() (entries int, bytes int64, evictions int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len(), c.total, c.evictions.Load()
}

func (n *nsCache) get(key string, now int64) *Entity {
	v, ok := n.m.Load(key)
	if !ok {
		return nil
	}
	e := v.(*Entity)
	// 使うたびに並べ替えると無駄なので、少しのあいだは何もしない
	if now-e.lastUsed.Load() > 2000 {
		n.parent.touch(e, now)
	}
	return e
}

func (n *nsCache) set(key string, e *Entity, now int64) {
	c := n.parent
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := n.m.Load(key); ok {
		c.removeLocked(old.(*Entity))
	}
	e.ns, e.key = n, key
	e.owner.Store(n)
	e.lastUsed.Store(now)
	e.elem = c.order.PushFront(e)
	c.total += int64(e.bytes())
	n.m.Store(key, e)
	c.evictLocked()
}

func (c *EntityCache) touch(e *Entity, now int64) {
	c.mu.Lock()
	if e.elem != nil {
		c.order.MoveToFront(e.elem)
		e.lastUsed.Store(now)
	}
	c.mu.Unlock()
}

// removeLocked は mu を持って呼ぶ
func (c *EntityCache) removeLocked(e *Entity) {
	if e.elem == nil {
		return
	}
	c.total -= int64(e.bytes())
	c.order.Remove(e.elem)
	e.elem = nil
	e.ns.m.CompareAndDelete(e.key, e)
}

func (c *EntityCache) evictLocked() {
	for c.total > c.limit {
		el := c.order.Back()
		if el == nil {
			break
		}
		c.removeLocked(el.Value.(*Entity))
		c.evictions.Add(1)
	}
}

// publish は圧縮した版ができたとき。キャッシュに入っていれば、増えた大きさを数える
func (e *Entity) publish(s *slot, v *Variant) {
	ns := e.owner.Load()
	if ns == nil {
		s.v.Store(v)
		return
	}
	c := ns.parent
	c.mu.Lock()
	s.v.Store(v)
	if e.elem != nil {
		c.total += int64(len(v.body) + len(v.head))
		c.evictLocked()
	}
	c.mu.Unlock()
}

func (c *EntityCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.order.Front(); el != nil; el = c.order.Front() {
		c.removeLocked(el.Value.(*Entity))
	}
	c.total = 0
}

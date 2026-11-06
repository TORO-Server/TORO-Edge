package main

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
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
	// 捨てたが、送っている最中の客がいてまだ閉じていないファイルの大きさ (メモリを使っているので、上限に数える)
	closing atomic.Int64
	// sweep の続きの位置
	cursor *listElem
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
	if c.cursor == e.elem {
		c.cursor = e.elem.Prev()
	}
	c.total -= int64(e.bytes())
	c.order.Remove(e.elem)
	e.elem = nil
	e.retire(&c.closing)
	e.ns.m.CompareAndDelete(e.key, e)
}

func (c *EntityCache) evictLocked() {
	for c.total+c.closing.Load() > c.limit {
		el := c.order.Back()
		if el == nil {
			break
		}
		c.removeLocked(el.Value.(*Entity))
		c.evictions.Add(1)
	}
}

// publish は圧縮した版ができたとき。キャッシュに入っていれば、増えた大きさを数える。もう捨てられていたら、ファイルはすぐ手放す
func (e *Entity) publish(s *slot, v *Variant) {
	ns := e.owner.Load()
	if ns == nil {
		s.v.Store(v)
		if e.retired.Load() {
			v.dropOwner(nil)
		}
		return
	}
	c := ns.parent
	c.mu.Lock()
	s.v.Store(v)
	if e.elem != nil {
		c.total += int64(v.mem())
		c.evictLocked()
	} else {
		v.dropOwner(nil)
	}
	c.mu.Unlock()
}

const (
	sweepEvery = time.Second
	// 1 回に見る数 (ロックを長く持たない)
	sweepBatch = 1024
)

// sweep は使われていない側から batch 個まで見て、もう返さないもの (元が壊れていても返せる長さを過ぎたもの) を捨てる。次は続きから見る
func (c *EntityCache) sweep(now int64, batch int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.cursor
	if el == nil {
		el = c.order.Back()
	}
	removed := 0
	for i := 0; i < batch && el != nil; i++ {
		prev := el.Prev()
		e := el.Value.(*Entity)
		if now >= e.errorUntil.Load() && now >= e.staleUntil.Load() {
			c.removeLocked(e)
			removed++
		}
		el = prev
	}
	c.cursor = el
	return removed
}

// sweepLoop は stop が閉じるまで、少しずつ sweep する
func (c *EntityCache) sweepLoop(stop <-chan struct{}) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			c.sweep(nowMs(), sweepBatch)
		}
	}
}

func (c *EntityCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.order.Front(); el != nil; el = c.order.Front() {
		c.removeLocked(el.Value.(*Entity))
	}
	c.total = 0
}

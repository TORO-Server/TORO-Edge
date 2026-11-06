package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// smallEntity は期限を決めた小さい返事
func smallEntity(until int64) *Entity {
	e := newEntity(entityInit{status: 200, typ: "image/png", body: []byte("x"), tier: tierFast})
	e.freshUntil.Store(until)
	e.staleUntil.Store(until)
	e.errorUntil.Store(until)
	return e
}

// compressible は圧縮の対象になる返事
func compressible(body []byte, tier Tier) *Entity {
	return newEntity(entityInit{status: 200, typ: "text/plain", body: body, tier: tier})
}

// withFile はメモリ上のファイルの代わりに一時ファイルを持たせる (memfd は Linux だけなので)
func withFile(t *testing.T, v *Variant) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "body")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	v.file = f
	v.refs.Store(1)
	v.owned.Store(true)
	return f
}

// isClosed は閉じたか (裏で閉じているのを見るので、ロックを通る書き込みで確かめる)
func isClosed(f *os.File) bool {
	_, err := f.Write(nil)
	return err != nil
}

func waitClosed(t *testing.T, f *os.File) {
	t.Helper()
	for i := 0; i < 200 && !isClosed(f); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !isClosed(f) {
		t.Fatal("file must be closed")
	}
}

// 鍵の長さも数える (短い返事に長い鍵を付けて、数えた量より多くのメモリを使わせない)
func TestBytesCountsTheKey(t *testing.T) {
	c := newEntityCache(1 << 30)
	ns := c.space()
	short, long := smallEntity(0), smallEntity(0)
	ns.set("k", short, 0)
	ns.set(strings.Repeat("k", 1001), long, 0)
	if d := long.bytes() - short.bytes(); d != 1000 {
		t.Fatalf("key length must be counted: difference %d", d)
	}
	_, total, _ := c.stats()
	if want := int64(short.bytes() + long.bytes()); total != want {
		t.Fatalf("total = %d, want %d", total, want)
	}
	c.clear()
	if _, total, _ := c.stats(); total != 0 {
		t.Fatalf("total after clear = %d", total)
	}
}

// もう返さないもの (元が壊れていても返せる長さを過ぎたもの) だけを、少しずつ捨てる
func TestSweep(t *testing.T) {
	c := newEntityCache(1 << 30)
	ns := c.space()
	const now = 1000
	keep := smallEntity(now + 1)
	ns.set("keep", keep, 0)
	for i := 0; i < 10; i++ {
		ns.set("old"+strings.Repeat("x", i), smallEntity(now), 0)
	}
	// stale だけ先に切れていても、errorUntil が残っていれば捨てない
	stale := smallEntity(now)
	stale.errorUntil.Store(now + 1)
	ns.set("stale", stale, 0)

	removed := 0
	for i := 0; i < 20; i++ {
		removed += c.sweep(now, 3)
	}
	if removed != 10 {
		t.Fatalf("removed %d, want 10", removed)
	}
	if entries, _, _ := c.stats(); entries != 2 {
		t.Fatalf("entries = %d, want 2", entries)
	}
	if ns.get("keep", now) != keep || ns.get("stale", now) != stale {
		t.Fatal("entries in use must stay")
	}
}

// sweep の続きの位置のものが捨てられても、壊れない
func TestSweepCursorSurvivesRemoval(t *testing.T) {
	c := newEntityCache(1 << 30)
	ns := c.space()
	a, b, d := smallEntity(10), smallEntity(10), smallEntity(10)
	ns.set("a", a, 0)
	ns.set("b", b, 0)
	ns.set("d", d, 0)
	c.sweep(5, 1) // a を見て、続きは b
	if c.cursor != b.elem {
		t.Fatal("cursor must point to the next entry")
	}
	ns.set("b", smallEntity(10), 0) // b を入れ替えて捨てる
	if c.cursor == b.elem || (c.cursor != nil && c.cursor.Value == any(b)) {
		t.Fatal("cursor must not stay on a removed entry")
	}
	if got := c.sweep(20, 10); got == 0 {
		t.Fatal("sweep must continue after the removal")
	}
}

// ファイルは送っている客がいなくなったら閉じる。閉じるまでの大きさは上限に数える
func TestRetiredFileWaitsForSenders(t *testing.T) {
	c := newEntityCache(1 << 30)
	ns := c.space()
	e := compressible(make([]byte, 2048), tierFast)
	f := withFile(t, e.identity)
	ns.set("k", e, 0)

	if !e.identity.ref() {
		t.Fatal("ref must succeed while the file is open")
	}
	c.clear()
	if !e.retired.Load() {
		t.Fatal("entry must be retired")
	}
	if isClosed(f) {
		t.Fatal("file must stay open while it is being sent")
	}
	if got := c.closing.Load(); got != 2048 {
		t.Fatalf("closing = %d, want 2048", got)
	}
	// 客がいるあいだは、ほかの客も使える
	if !e.identity.ref() {
		t.Fatal("ref must succeed while a sender is active")
	}
	e.identity.unref()
	if isClosed(f) {
		t.Fatal("file must stay open until the last sender")
	}
	e.identity.unref()
	if !isClosed(f) {
		t.Fatal("file must be closed by the last sender")
	}
	if got := c.closing.Load(); got != 0 {
		t.Fatalf("closing after close = %d", got)
	}
	if e.identity.ref() {
		t.Fatal("ref must fail after the file is closed")
	}
}

// 送っている客がいなければ、すぐ閉じる
func TestRetiredFileClosesWithoutSenders(t *testing.T) {
	c := newEntityCache(1 << 30)
	ns := c.space()
	e := compressible(make([]byte, 2048), tierFast)
	f := withFile(t, e.identity)
	ns.set("k", e, 0)
	ns.set("k", compressible(make([]byte, 2048), tierFast), 0) // 入れ替えると、前のものは捨てられる
	waitClosed(t, f)
	e.retire(&c.closing) // 2 度捨てても 1 度だけ効く
	for i := 0; i < 200 && c.closing.Load() != 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.closing.Load(); got != 0 {
		t.Fatalf("closing = %d", got)
	}
}

// 閉じていないファイルの分だけ、ためられる量が減る (全体でメモリの上限を守る)
func TestClosingBytesCountAgainstTheLimit(t *testing.T) {
	c := newEntityCache(10_000)
	ns := c.space()
	first := smallEntity(0)
	ns.set("a", first, 0)
	c.closing.Store(10_000)
	ns.set("b", smallEntity(0), 0)
	if entries, _, _ := c.stats(); entries != 0 {
		t.Fatalf("entries = %d, want 0 while closing bytes fill the limit", entries)
	}
	if evicted := c.evictions.Load(); evicted != 2 {
		t.Fatalf("evictions = %d, want 2", evicted)
	}
	c.closing.Store(0)
	ns.set("c", smallEntity(0), 0)
	if entries, _, _ := c.stats(); entries != 1 {
		t.Fatalf("entries = %d, want 1 after closing bytes are gone", entries)
	}
}

// 捨てられたあとにできた圧縮の版は、すぐ手放す
func TestPublishAfterRetire(t *testing.T) {
	c := newEntityCache(1 << 30)
	ns := c.space()
	e := compressible(make([]byte, 2048), tierFast)
	ns.set("k", e, 0)
	c.clear()
	v := &Variant{size: 100}
	f := withFile(t, v)
	e.publish(&e.gz, v)
	waitClosed(t, f)
	if _, total, _ := c.stats(); total != 0 {
		t.Fatalf("total = %d", total)
	}

	// キャッシュに入らないもの (静的なファイルの表から外れた返事) も同じ
	s := compressible(make([]byte, 2048), tierBest)
	s.retire(nil)
	v2 := &Variant{size: 100}
	f2 := withFile(t, v2)
	s.publish(&s.br, v2)
	waitClosed(t, f2)
}

// holdGate は圧縮の枠をふさいで、ジョブを待たせる
func holdGate(t *testing.T, tier Tier) (release func()) {
	t.Helper()
	gate := gates[tier]
	for i := 0; i < cap(gate); i++ {
		gate <- struct{}{}
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		for i := 0; i < cap(gate); i++ {
			<-gate
		}
	}
	t.Cleanup(release)
	return release
}

func waitQueueEmpty(t *testing.T, tier Tier) {
	t.Helper()
	q := &compressQueues[tier]
	for i := 0; i < 2000; i++ {
		q.mu.Lock()
		jobs := q.jobs
		q.mu.Unlock()
		if jobs == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("compress queue did not drain")
}

// 圧縮の待ち行列は数で頭打ちになり、いっぱいなら始めない
func TestCompressQueueJobLimit(t *testing.T) {
	release := holdGate(t, tierFast)
	body := bytes.Repeat([]byte("abc"), 1024)
	var entries []*Entity
	for i := 0; i < maxCompressJobs; i++ {
		e := compressible(body, tierFast)
		if !e.start(&e.gz, false, true) {
			t.Fatalf("job %d must be admitted", i)
		}
		entries = append(entries, e)
	}
	over := compressible(body, tierFast)
	if over.start(&over.gz, false, true) {
		t.Fatal("job over the limit must not start")
	}
	if got := over.ensure(encGzip); got != over.identity {
		t.Fatal("must answer with the identity variant while the queue is full")
	}
	// 始めていないものは、空いたらやり直せる
	release()
	waitQueueEmpty(t, tierFast)
	if got := over.ensure(encGzip); got == over.identity || got.encoding != "gzip" {
		t.Fatalf("must compress once the queue has room: %+v", got)
	}
	for _, e := range entries {
		<-e.gz.done
		if e.gz.v.Load() == nil {
			t.Fatal("admitted job must finish")
		}
	}
	waitQueueEmpty(t, tierFast)
}

// 待っている本文の大きさでも頭打ちになる。空なら大きいものも 1 つは入れる
func TestCompressQueueByteLimit(t *testing.T) {
	release := holdGate(t, tierBest)
	body := bytes.Repeat([]byte("abc"), 16<<20/3)
	entries := []*Entity{compressible(body, tierBest)}
	if !entries[0].start(&entries[0].gz, false, true) {
		t.Fatal("first job must be admitted")
	}
	for (len(entries)+1)*len(body) <= maxCompressBytes {
		e := compressible(body, tierBest)
		if !e.start(&e.gz, false, true) {
			t.Fatalf("job %d must be admitted", len(entries))
		}
		entries = append(entries, e)
	}
	over := compressible(body, tierBest)
	if over.start(&over.gz, false, true) {
		t.Fatal("job over the byte limit must not start")
	}
	// 手元のファイルの圧縮 (warm) は上限を見ない
	static := compressible(body, tierBest)
	static.warm()
	if !static.gz.started.Load() {
		t.Fatal("warm must start without looking at the limit")
	}
	// 大きいものを本当に圧縮すると時間がかかるので、捨ててから流す
	static.retire(nil)
	for _, e := range entries {
		e.retire(nil)
	}
	release()
	waitQueueEmpty(t, tierBest)
	<-static.gz.done
	<-static.br.done
}

// 待っているあいだに捨てられたものは作らない。捨てられたあとは始めない
func TestRetiredEntryIsNotCompressed(t *testing.T) {
	release := holdGate(t, tierFast)
	e := compressible(bytes.Repeat([]byte("abc"), 1024), tierFast)
	if !e.start(&e.gz, false, true) {
		t.Fatal("job must start")
	}
	e.retire(nil)
	release()
	<-e.gz.done
	if e.gz.v.Load() != nil {
		t.Fatal("retired entry must not be compressed")
	}
	if e.start(&e.br, true, true) {
		t.Fatal("must not start a new job for a retired entry")
	}
	if got := e.ensure(encBr); got != e.identity {
		t.Fatal("retired entry must answer with the identity variant")
	}
	waitQueueEmpty(t, tierFast)
}

// fast の圧縮が終わらなくても、客を待たせ続けない
func TestFastCompressWaitIsBounded(t *testing.T) {
	release := holdGate(t, tierFast)
	e := compressible(bytes.Repeat([]byte("abc"), 1024), tierFast)
	start := time.Now()
	if got := e.ensure(encBr); got != e.identity {
		t.Fatal("must answer with the identity variant after the wait")
	}
	if d := time.Since(start); d < fastWait || d > fastWait+2*time.Second {
		t.Fatalf("waited %v", d)
	}
	release()
	<-e.br.done
	waitQueueEmpty(t, tierFast)
}

// 閉じたファイルの版が選ばれても、そのままの版をメモリから返す
func TestSendEntityFallsBackWhenTheFileIsClosed(t *testing.T) {
	body := bytes.Repeat([]byte("hello "), 400)
	e := compressible(body, tierBest)
	closedVariant := e.makeVariant("br", nil, variantEtag(e.etag, "br"))
	closedVariant.size = 10
	f := withFile(t, closedVariant)
	f.Close()
	closedVariant.refs.Store(0)
	e.br.v.Store(closedVariant)

	srv := newServer(func(c *Ctx) { sendEntity(c, e) }, func(*Ctx) {})
	ln := listen(t)
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(200 * time.Millisecond) })
	c, br := dial(t, ln.Addr().String())
	io.WriteString(c, "GET /x HTTP/1.1\r\nHost: a\r\nAccept-Encoding: br\r\n\r\n")
	resp, got := readResp(t, br, "GET")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "" || !bytes.Equal(got, body) {
		t.Fatalf("status %d, encoding %q, %d bytes", resp.StatusCode, resp.Header.Get("Content-Encoding"), len(got))
	}
}

// 新しいものに入れ替えた静的なファイルの返事は、ファイルを閉じる。入れ替えたあとに来た客の分はファイルを作らない
func TestStaticRecRetire(t *testing.T) {
	r := &rec{typ: "image/png", body: []byte("x"), entities: map[*StaticRules]*Entity{}}
	rules := newStaticRules()
	e := r.entity(rules)
	f := withFile(t, e.identity)
	r.retire()
	if !e.retired.Load() {
		t.Fatal("entity must be retired")
	}
	waitClosed(t, f)
	late := r.entity(newStaticRules())
	if late.files {
		t.Fatal("entity created after retire must not attach files")
	}
}

// 管理用の合言葉 (pprof も同じものを通す)
func TestAdminAllowed(t *testing.T) {
	req := func(kv ...string) bool {
		r := httptest.NewRequest("GET", "/stats", nil)
		for i := 0; i+1 < len(kv); i += 2 {
			r.Header.Set(kv[i], kv[i+1])
		}
		return adminAllowed(r)
	}
	t.Setenv("EDGE_ADMIN_TOKEN", "")
	if !req() {
		t.Fatal("no token configured must allow")
	}
	t.Setenv("EDGE_ADMIN_TOKEN", "secret")
	if req() || req("Authorization", "Bearer wrong") || req("X-Admin-Token", "wrong") || req("Authorization", "secret") || req("X-Admin-Token", "secret2") {
		t.Fatal("wrong tokens must be refused")
	}
	if !req("Authorization", "Bearer secret") || !req("X-Admin-Token", "secret") {
		t.Fatal("right token must be accepted")
	}
	called := atomic.Bool{}
	h := adminOnly(func(http.ResponseWriter, *http.Request) { called.Store(true) })
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/debug/pprof/", nil))
	if called.Load() || w.Code != 401 {
		t.Fatalf("pprof without token: called=%v code=%d", called.Load(), w.Code)
	}
}

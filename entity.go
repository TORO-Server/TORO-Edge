package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/andybalholm/brotli"
)

// 圧縮する種類 (画像や、すでに圧縮された本文は対象外)
var compressibleType = regexp.MustCompile(`(?i)^(?:text/|application/(?:json|javascript|x-javascript|xml|wasm|manifest\+json|ld\+json)|image/svg\+xml)`)

const (
	// これより小さいものは圧縮しない (圧縮しても変わらず、手間だけ増える)
	minCompress = 1024
	// best の圧縮を待つ時間。間に合わなければ、そのときできている版 (なければ、そのまま) を返す
	bestWait = 250 * time.Millisecond
	// fast の圧縮を待つ時間。間に合わなければ、そのままの版を返す
	fastWait = 2 * time.Second
	// 圧縮を待っているものの上限 (段ごと)。待っているあいだ本文を抱えているので、数と大きさの両方を見る。いっぱいなら、そのままの版を返す
	maxCompressJobs  = 256
	maxCompressBytes = 128 << 20
	// 本文と頭書きと鍵のほかにメモリに置くものの見積もり (構造体、頭書きの文字列、鍵を引く表と並びの 1 つぶん。実測で 1 KB 弱)
	entityOverhead = 1024
)

// Tier は圧縮の力の入れ方。
// fast: 短いあいだしか持たないもの (毎秒変わる更新など)。brotli 5 / gzip 6 を、最初の 1 回だけ待って作る
// best: 長く持つもの (静的なファイル、版が URL に入るもの)。brotli 11 / gzip 9 を裏で作り、間に合わないあいだは軽い版を返す
type Tier int

const (
	tierFast Tier = iota
	tierBest
)

var gates = [2]chan struct{}{make(chan struct{}, 4), make(chan struct{}, 2)}

// compressQueue は圧縮を待っているものの数と大きさ (作っている最中のものも含む)
type compressQueue struct {
	mu    sync.Mutex
	jobs  int
	bytes int64
}

var compressQueues [2]compressQueue

// admit は待ち行列に入れてよいか。空のときは、大きくても 1 つは入れる
func (q *compressQueue) admit(n int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.jobs > 0 && (q.jobs >= maxCompressJobs || q.bytes+n > maxCompressBytes) {
		return false
	}
	q.jobs++
	q.bytes += n
	return true
}

func (q *compressQueue) leave(n int64) {
	q.mu.Lock()
	q.jobs--
	q.bytes -= n
	q.mu.Unlock()
}

// sendfileMin はこの大きさ以上の本文を、メモリ上のファイルに置いて sendfile で送る (0 なら使わない。EDGE_SENDFILE_MIN)
var sendfileMin = 64 << 10

// Variant は 1 つの表現 (そのまま、gzip、brotli)
type Variant struct {
	encoding string
	// file があるときの gzip / brotli の版は、本文をここに持たない (file だけに置く)
	body []byte
	size int
	etag string
	// 状態の行から頭書きまで (Date と最後の空行は送るときに付ける)。そのまま writev に渡す
	head []byte
	// 大きい本文のメモリ上のファイル (sendfile で送る)。なければ nil
	file   *os.File
	fileRC syscall.RawConn
	// ファイルを使っている数 (送っている客と、持ち主の 1)。0 になったら閉じる
	refs atomic.Int32
	// 持ち主 (キャッシュや表) の 1 がまだあるか
	owned atomic.Bool
	// 捨てたあと、閉じるまでの大きさを数える先 (EntityCache の closing)。なければ nil
	closing *atomic.Int64
}

// mem はこの版がメモリに置いている大きさ
func (v *Variant) mem() int {
	n := len(v.body) + len(v.head)
	if v.file != nil {
		n += v.size
	}
	return n
}

// attachFile は大きい本文をメモリ上のファイルにも置く。drop なら、本文はファイルだけに置く
func (v *Variant) attachFile(drop bool) {
	if v.file != nil || sendfileMin <= 0 || v.size < sendfileMin {
		return
	}
	f, rc := newBodyFile(v.body)
	if f == nil {
		return
	}
	v.file, v.fileRC = f, rc
	v.refs.Store(1)
	v.owned.Store(true)
	if drop {
		v.body = nil
	}
}

// ref は送り始めるときに呼ぶ。もう閉じたファイルなら false
func (v *Variant) ref() bool {
	for {
		n := v.refs.Load()
		if n == 0 {
			return false
		}
		if v.refs.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// unref は送り終えたときに呼ぶ。最後の 1 つならファイルを閉じる
func (v *Variant) unref() {
	if v.refs.Add(-1) == 0 {
		v.closeFile()
	}
}

func (v *Variant) closeFile() {
	v.file.Close()
	if v.closing != nil {
		v.closing.Add(-int64(v.size))
	}
}

// dropOwner は持ち主が手放すとき (何度呼んでも 1 度だけ効く)。キャッシュのロックを持ったまま呼ばれるので、閉じるのは裏で行う
func (v *Variant) dropOwner(closing *atomic.Int64) {
	if v.file == nil || !v.owned.CompareAndSwap(true, false) {
		return
	}
	if closing != nil {
		v.closing = closing
		closing.Add(int64(v.size))
	}
	if v.refs.Add(-1) == 0 {
		go v.closeFile()
	}
}

// slot は圧縮した版の置き場 (最初に求められたとき 1 度だけ裏で作る)
type slot struct {
	once sync.Once
	// done を作り終えた (ここまで来れば once を通らなくてよい)
	started atomic.Bool
	done    chan struct{}
	v       atomic.Pointer[Variant]
}

type entityInit struct {
	status int
	typ    string
	body   []byte
	// 元がすでに付けている Content-Encoding (そのまま返す)
	encoding     string
	etag         string
	lastModified string
	// 返す頭書き。content-*, etag, vary, last-modified は入れない
	headers []hdr
	tier    Tier
}

var revalidateHeaders = map[string]bool{
	"Cache-Control": true, "Cdn-Cache-Control": true, "Expires": true, "Access-Control-Allow-Origin": true, "Content-Language": true,
}

// Entity は返事 1 つぶん (本文、頭書き、圧縮した版)。作ったあとは変えない (圧縮した版が増えるだけ)
type Entity struct {
	status         int
	tier           Tier
	identity       *Variant
	compressible   bool
	lastModified   string
	lastModifiedMs int64
	etag           string
	// 304 に付ける頭書き (状態の行と etag は付けるときに足す)
	head304 []byte

	br, gz slot

	// この時刻 (ms) まで新しい / まで (裏で確かめながら) 古いまま返せる / 元が壊れているときに古いまま返せる
	freshUntil, staleUntil, errorUntil atomic.Int64
	// 確かめに失敗したあと、次に確かめるまで待つ
	nextRefresh atomic.Int64
	lastUsed    atomic.Int64

	init   entityInit
	varies bool
	// 大きい本文をメモリ上のファイルにも置く (attachFiles のあと。キャッシュや静的なファイルに入れるもの)
	files bool
	// 捨てられた (キャッシュや表から外れた)。もう圧縮した版を作らない
	retired atomic.Bool

	// キャッシュに入っているあいだ (cache.go。ns, key, elem は EntityCache の mu で守る)
	ns   *nsCache
	key  string
	elem *listElem
	// 入れられたキャッシュ (圧縮した版ができたとき、ロックなしで見る)
	owner atomic.Pointer[nsCache]
}

func newEntity(init entityInit) *Entity {
	e := &Entity{status: init.status, tier: init.tier, init: init, etag: init.etag, lastModified: init.lastModified}
	e.compressible = init.encoding == "" && init.status == 200 && len(init.body) >= minCompress && compressibleType.MatchString(init.typ)
	e.varies = e.compressible || init.encoding != ""
	if init.lastModified != "" {
		if t, err := http.ParseTime(init.lastModified); err == nil {
			e.lastModifiedMs = t.UnixMilli()
		}
	}
	e.identity = e.makeVariant(init.encoding, init.body, init.etag)
	var h304 []byte
	for _, h := range init.headers {
		if revalidateHeaders[h.k] {
			h304 = appendHeader(h304, h.k, h.v)
		}
	}
	if e.varies {
		h304 = appendHeader(h304, "Vary", "Accept-Encoding")
	}
	if init.lastModified != "" {
		h304 = appendHeader(h304, "Last-Modified", init.lastModified)
	}
	e.head304 = h304
	return e
}

// bytes はメモリに置いている大きさ (見積もり)。鍵も数える
func (e *Entity) bytes() int {
	n := e.identity.mem() + entityOverhead + len(e.key)
	if v := e.br.v.Load(); v != nil {
		n += v.mem()
	}
	if v := e.gz.v.Load(); v != nil {
		n += v.mem()
	}
	return n
}

// attachFiles は大きい本文をメモリ上のファイルにも置く。人の目に触れる前 (キャッシュや表に入れる前) に 1 度だけ呼ぶ
func (e *Entity) attachFiles() {
	e.files = true
	e.identity.attachFile(false)
}

// retire はキャッシュや表から捨てたとき。ファイルは、送っている最中の客がいなくなったら閉じる。
// closing があれば、閉じるまでの大きさをそこに数える
func (e *Entity) retire(closing *atomic.Int64) {
	e.retired.Store(true)
	for _, v := range [3]*Variant{e.identity, e.br.v.Load(), e.gz.v.Load()} {
		if v != nil {
			v.dropOwner(closing)
		}
	}
}

func (e *Entity) makeVariant(encoding string, body []byte, etag string) *Variant {
	init := &e.init
	b := make([]byte, 0, 256)
	b = append(b, statusLine(init.status)...)
	if init.status != 204 && init.status != 304 {
		b = appendHeader(b, "Content-Type", init.typ)
		b = appendHeader(b, "Content-Length", strconv.Itoa(len(body)))
	}
	if etag != "" {
		b = appendHeader(b, "Etag", etag)
	}
	if encoding != "" {
		b = appendHeader(b, "Content-Encoding", encoding)
	}
	if e.varies {
		b = appendHeader(b, "Vary", "Accept-Encoding")
	}
	if init.lastModified != "" {
		b = appendHeader(b, "Last-Modified", init.lastModified)
	}
	for _, h := range init.headers {
		b = appendHeader(b, h.k, h.v)
	}
	return &Variant{encoding: encoding, body: body, size: len(body), etag: etag, head: b}
}

// matchEtag は If-None-Match が、この返事のどの版かに合うか。合えば、その ETag (ETag のない返事は "")
func (e *Entity) matchEtag(header string) (string, bool) {
	base := e.etag
	if base == "" {
		if strings.TrimSpace(header) == "*" {
			return "", true
		}
		return "", false
	}
	var br, gz string
	if e.compressible {
		br = variantEtag(base, "br")
		gz = variantEtag(base, "gz")
	}
	strong := strings.TrimPrefix(base, "W/")
	// 1 つだけ (ほとんどの場合) なら、切り分けずに比べる
	if !strings.Contains(header, ",") {
		t := strings.TrimPrefix(strings.TrimSpace(header), "W/")
		switch {
		case t == "*" || t == strong:
			return base, true
		case br != "" && t == strings.TrimPrefix(br, "W/"):
			return br, true
		case gz != "" && t == strings.TrimPrefix(gz, "W/"):
			return gz, true
		}
		return "", false
	}
	for _, t := range etagTokens(header) {
		if t == "*" || t == strong {
			return base, true
		}
		if br != "" && t == strings.TrimPrefix(br, "W/") {
			return br, true
		}
		if gz != "" && t == strings.TrimPrefix(gz, "W/") {
			return gz, true
		}
	}
	return "", false
}

// pick はいまできている中で、受けられる最良の版。まだなければ nil (ensure で作る)
func (e *Entity) pick(enc int) *Variant {
	if !e.compressible {
		return e.identity
	}
	if enc&encBr != 0 {
		return e.br.v.Load()
	}
	if enc&encGzip != 0 {
		return e.gz.v.Load()
	}
	return e.identity
}

// ensure は受けられる圧縮の版を作って返す。作れない・小さくならないときは、そのままの版
func (e *Entity) ensure(enc int) *Variant {
	if !e.compressible {
		return e.identity
	}
	wantBr := enc&encBr != 0
	wantGz := enc&encGzip != 0
	if !wantBr && !wantGz {
		return e.identity
	}
	if e.tier == tierFast {
		s := &e.gz
		if wantBr {
			s = &e.br
		}
		if !e.start(s, wantBr, true) {
			return e.identity
		}
		select {
		case <-s.done:
		default:
			timer := time.NewTimer(fastWait)
			defer timer.Stop()
			select {
			case <-s.done:
			case <-timer.C:
				return e.identity
			}
		}
		if v := s.v.Load(); v != nil {
			return v
		}
		return e.identity
	}
	// best: br を裏で作り、待つのは少しだけ。そのあいだ軽い gzip も作る
	var order [2]*slot
	n := 0
	if wantBr && e.start(&e.br, true, true) {
		order[n] = &e.br
		n++
	}
	if e.start(&e.gz, false, true) {
		order[n] = &e.gz
		n++
	}
	if n == 0 {
		return e.identity
	}
	timer := time.NewTimer(bestWait)
	defer timer.Stop()
	for _, s := range order[:n] {
		select {
		case <-s.done:
			if v := s.v.Load(); v != nil {
				return v
			}
		case <-timer.C:
			return e.identity
		}
	}
	return e.identity
}

// warm は先に作っておく (静的なファイルを読み込んだときなど)
func (e *Entity) warm() {
	if !e.compressible {
		return
	}
	// 手元のファイルの数だけなので、待ち行列の上限は見ない
	e.start(&e.br, true, false)
	e.start(&e.gz, false, false)
}

// start は圧縮した版を裏で作り始める。始めなかった (待ち行列がいっぱい、捨てられたあと) ときは false で、次に求められたときにやり直す
func (e *Entity) start(s *slot, br, bounded bool) bool {
	if s.started.Load() {
		return true
	}
	if e.retired.Load() {
		return false
	}
	n := int64(len(e.init.body))
	q := &compressQueues[e.tier]
	if bounded && !q.admit(n) {
		return false
	}
	first := false
	s.once.Do(func() {
		first = true
		s.done = make(chan struct{})
		go func() {
			defer close(s.done)
			if bounded {
				defer q.leave(n)
			}
			gate := gates[e.tier]
			gate <- struct{}{}
			defer func() { <-gate }()
			// 裏で panic しても全体を止めない (そのままの版を返し続ける)
			defer func() {
				if rec := recover(); rec != nil {
					fmt.Fprintln(os.Stderr, "compress:", rec)
				}
			}()
			// 待っているあいだに捨てられたものは作らない
			if e.retired.Load() {
				return
			}
			if v := e.build(br); v != nil {
				e.publish(s, v)
			}
		}()
	})
	s.started.Store(true)
	if bounded && !first {
		q.leave(n)
	}
	return true
}

func (e *Entity) build(br bool) *Variant {
	body := e.init.body
	best := e.tier == tierBest
	var buf bytes.Buffer
	buf.Grow(len(body) / 3)
	if br {
		q, lg := 5, 22
		if best {
			q, lg = 11, 24
		}
		w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: q, LGWin: lg})
		if _, err := w.Write(body); err != nil {
			return nil
		}
		if err := w.Close(); err != nil {
			return nil
		}
	} else {
		level := 6
		if best {
			level = 9
		}
		w, err := gzip.NewWriterLevel(&buf, level)
		if err != nil {
			return nil
		}
		if _, err := w.Write(body); err != nil {
			return nil
		}
		if err := w.Close(); err != nil {
			return nil
		}
	}
	// ほとんど小さくならないなら、そのまま返す
	if float64(buf.Len()) >= float64(len(body))*0.95 {
		return nil
	}
	var v *Variant
	if br {
		v = e.makeVariant("br", buf.Bytes(), variantEtag(e.etag, "br"))
	} else {
		v = e.makeVariant("gzip", buf.Bytes(), variantEtag(e.etag, "gz"))
	}
	if e.files && !e.retired.Load() {
		v.attachFile(true)
	}
	return v
}

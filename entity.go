package main

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// Variant は 1 つの表現 (そのまま、gzip、brotli)
type Variant struct {
	encoding string
	body     []byte
	etag     string
	// 状態の行から頭書きまで (Date と最後の空行は送るときに付ける)。そのまま writev に渡す
	head []byte
}

// slot は圧縮した版の置き場 (最初に求められたとき 1 度だけ裏で作る)
type slot struct {
	once sync.Once
	done chan struct{}
	v    atomic.Pointer[Variant]
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

// bytes はメモリに置いている大きさ (見積もり)
func (e *Entity) bytes() int {
	n := len(e.identity.body) + len(e.identity.head) + 1024
	if v := e.br.v.Load(); v != nil {
		n += len(v.body) + len(v.head)
	}
	if v := e.gz.v.Load(); v != nil {
		n += len(v.body) + len(v.head)
	}
	return n
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
	return &Variant{encoding: encoding, body: body, etag: etag, head: b}
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
		e.start(s, wantBr)
		<-s.done
		if v := s.v.Load(); v != nil {
			return v
		}
		return e.identity
	}
	// best: br を裏で作り、待つのは少しだけ。そのあいだ軽い gzip も作る
	if wantBr {
		e.start(&e.br, true)
	}
	e.start(&e.gz, false)
	timer := time.NewTimer(bestWait)
	defer timer.Stop()
	var order [2]*slot
	n := 0
	if wantBr {
		order[n] = &e.br
		n++
	}
	order[n] = &e.gz
	n++
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
	e.start(&e.br, true)
	e.start(&e.gz, false)
}

func (e *Entity) start(s *slot, br bool) {
	s.once.Do(func() {
		s.done = make(chan struct{})
		go func() {
			defer close(s.done)
			gate := gates[e.tier]
			gate <- struct{}{}
			defer func() { <-gate }()
			if v := e.build(br); v != nil {
				e.publish(s, v)
			}
		}()
	})
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
	if br {
		return e.makeVariant("br", buf.Bytes(), variantEtag(e.etag, "br"))
	}
	return e.makeVariant("gzip", buf.Bytes(), variantEtag(e.etag, "gz"))
}

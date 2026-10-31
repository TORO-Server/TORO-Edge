package main

import (
	"sync/atomic"
	"time"
)

// hid は、読み取る (元に渡すか、判断に使う) リクエストの頭書き。ここに無い頭書きは読み捨てる (Cookie など)
type hid int

const (
	hHost hid = iota
	hAcceptEncoding
	hIfNoneMatch
	hIfModifiedSince
	hRange
	hIfRange
	hOrigin
	hReferer
	hSecFetchSite
	hCfConnectingIP
	hXForwardedFor
	hXForwardedProto
	hUserAgent
	hAccept
	hLastEventID
	hSecWSKey
	hSecWSVersion
	hSecWSProtocol
	hSecWSExtensions
	hUpgrade
	hConnection
	hContentLength
	hTransferEncoding
	numHdr
)

// hdrNames は元に渡すときの名前
var hdrNames = [numHdr]string{
	"Host", "Accept-Encoding", "If-None-Match", "If-Modified-Since", "Range", "If-Range", "Origin", "Referer", "Sec-Fetch-Site",
	"Cf-Connecting-Ip", "X-Forwarded-For", "X-Forwarded-Proto", "User-Agent", "Accept", "Last-Event-Id",
	"Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Protocol", "Sec-Websocket-Extensions", "Upgrade", "Connection",
	"Content-Length", "Transfer-Encoding",
}

// hdrIDs は小文字の名前から hid を引く
var hdrIDs = func() map[string]hid {
	m := make(map[string]hid, numHdr)
	for i, n := range hdrNames {
		b := []byte(n)
		for j, c := range b {
			if c >= 'A' && c <= 'Z' {
				b[j] = c + 32
			}
		}
		m[string(b)] = hid(i)
	}
	return m
}()

// Request は読み取ったリクエスト (読む頭書きだけ)
type Request struct {
	Method string
	// ? も含む、書かれたままの宛先
	Target string
	Host   string
	h      [numHdr]string
	http11 bool
	// 返したあと、つなぎを使い続けてよいか
	keepAlive bool
	// 本文がある (GET / HEAD では受け付けない)
	hasBody bool
	// WebSocket の Upgrade か
	upgrade bool
	remote  string
}

func (r *Request) header(id hid) string { return r.h[id] }

// Ctx は 1 つのリクエストの状態
type Ctx struct {
	conn *conn
	req  *Request
	// GET か HEAD
	method string
	head   bool
	// ? の前 (デコードしていない) と、? から後ろ (なければ "")
	path   string
	search string
	// Accept-Encoding で受けられる圧縮 (encBr, encGzip)
	enc int
	// 経路の規則が取り出した部分 (ワールドの名前、assets のファイル名)
	cap string
	// ログに出す: HIT, MISS, STALE, REVALIDATED, COALESCED, BYPASS, ERROR-STALE, FALLBACK, -
	cache string
	// 返したバイト数
	bytes int64
	// アクセスログに出すか (エラーと遅いものは、これに関わらず出す)
	log   bool
	start time.Time
	// 返した状態 (0 はまだ何も返していない)
	status int
}

type Handler func(c *Ctx)

// Site は Host ごとの経路の決まり
type Site struct {
	name  string
	route func(path string, c *Ctx) Handler
}

// Rule は経路の 1 つ。exact / prefix / fn のどれか 1 つを使う。fn は合えば、取り出した部分 (なければ "") と true を返す
type Rule struct {
	exact    string
	hasExact bool
	prefix   string
	fn       func(path string) (string, bool)
	// 完全一致より先に見る (出してはいけないものを断る規則)
	first   bool
	handler Handler
}

// ResponseRules は元の返事の頭書きを、客に返す頭書きに直す決まり。
// hide は全部の返事に、cacheControl と set は成功の返事 (4xx / 5xx 以外) にだけ効く (nginx の add_header と同じ)
type ResponseRules struct {
	// "" か "upstream" なら元のまま。文字列ならそれに置き換える
	cacheControl string
	// 返さない頭書き (小文字)
	hide []string
	// 足す・置き換える頭書き
	set [][2]string
}

// stripedCounter は全コアから増やされる数 (1 つの数をみんなで増やすと、キャッシュラインの取り合いになる)
type stripedCounter struct {
	cells [16]struct {
		n atomic.Int64
		_ [56]byte
	}
}

func (s *stripedCounter) add(stripe uint32, d int64) { s.cells[stripe&15].n.Add(d) }

func (s *stripedCounter) load() int64 {
	var sum int64
	for i := range s.cells {
		sum += s.cells[i].n.Load()
	}
	return sum
}

// Counters は管理用のポートで見られる数 (/stats)
type Counters struct {
	// 毎リクエスト増える数は、つなぎごとにばらして数える
	requests, s2xx, s3xx, s4xx, s5xx, bytesOut, hit stripedCounter

	miss, stale, revalidated, coalesced, bypass, errorStale      atomic.Int64
	upstreamRequests, upstreamErrors, fallbacks, wsOpen, wsTotal atomic.Int64
}

var stats Counters
var startedAt = time.Now()

func countStatus(stripe uint32, status int) {
	switch {
	case status < 300:
		stats.s2xx.add(stripe, 1)
	case status < 400:
		stats.s3xx.add(stripe, 1)
	case status < 500:
		stats.s4xx.add(stripe, 1)
	default:
		stats.s5xx.add(stripe, 1)
	}
}

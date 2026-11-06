package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"
)

// debugHeaders が true のとき、返事に x-edge-cache を付ける (EDGE_DEBUG_HEADERS=1)
var debugHeaders bool

func appendHeader(b []byte, k, v string) []byte {
	b = append(b, k...)
	b = append(b, ": "...)
	b = append(b, v...)
	return append(b, '\r', '\n')
}

// debugLine は x-edge-cache の 1 行 (試験用)
func (c *Ctx) debugLine() []byte {
	if !debugHeaders {
		return nil
	}
	cn := c.conn
	cn.dbg = appendHeader(cn.dbg[:0], "X-Edge-Cache", c.cache)
	return cn.dbg
}

// armWrite は書き込みの期限を付ける。どの返事も期限なしでは書かない (読まない客に待たされない)。
// 小さい返事は、残りが半分あるあいだ付け直さない (タイマーの手間を毎回かけない)。full なら長さいっぱいに付け直す
func (cn *conn) armWrite(now time.Time, full bool) {
	limit := cn.srv.writeTimeout
	if !full && cn.writeUntil.Sub(now) > limit/2 {
		return
	}
	cn.writeUntil = now.Add(limit)
	cn.nc.SetWriteDeadline(cn.writeUntil)
}

// write は 頭書き (状態の行から、最後の空行の前まで) + Date + extra + 空行 + 本文 を、1 回の writev で送る
func (c *Ctx) write(status int, head, extra, body []byte) error {
	cn := c.conn
	c.status = status
	tail := crlf
	if cn.closing() {
		tail = connCloseTail
	}
	now := time.Now()
	iov := cn.iov[:0]
	iov = append(iov, head, dateLine(now))
	if len(extra) > 0 {
		iov = append(iov, extra)
	}
	iov = append(iov, tail)
	if len(body) > 0 && !c.head {
		iov = append(iov, body)
		c.bytes = int64(len(body))
	}
	cn.bufs = iov
	cn.armWrite(now, len(body) > bigWrite)
	_, err := cn.bufs.WriteTo(cn.nc)
	if err != nil {
		cn.closeAfter = true
	}
	return err
}

// sendEntity は返事 1 つ (圧縮した版を選び、条件付きリクエストなら 304) を送る
func sendEntity(c *Ctx, e *Entity) {
	r := c.req
	if e.status == 200 {
		matched, ok := "", false
		if inm := r.h[hIfNoneMatch]; inm != "" {
			matched, ok = e.matchEtag(inm)
		} else if ims := r.h[hIfModifiedSince]; ims != "" && e.lastModifiedMs != 0 {
			if t, err := http.ParseTime(ims); err == nil && e.lastModifiedMs <= t.UnixMilli() {
				matched, ok = e.etag, true
			}
		}
		if ok {
			cn := c.conn
			b := append(cn.scratch[:0], statusLine(304)...)
			if matched != "" {
				b = appendHeader(b, "Etag", matched)
			}
			b = append(b, e.head304...)
			cn.scratch = b
			c.write(304, b, c.debugLine(), nil)
			return
		}
	}
	v := e.pick(c.enc)
	if v == nil {
		v = e.ensure(c.enc)
	}
	if v.file != nil && !c.head {
		if v.ref() {
			c.writeFile(e.status, v)
			v.unref()
			return
		}
		// 捨てられてファイルを閉じたあと: そのままの版をメモリから送る
		v = e.identity
	}
	c.write(e.status, v.head, c.debugLine(), v.body)
}

// writeFile は頭書きを writev で、本文をメモリ上のファイルから sendfile で送る
func (c *Ctx) writeFile(status int, v *Variant) {
	cn := c.conn
	cn.armWrite(time.Now(), true)
	if err := c.write(status, v.head, c.debugLine(), nil); err != nil {
		return
	}
	sent, err := cn.sendFile(v)
	c.bytes = sent
	if err == nil {
		return
	}
	cn.closeAfter = true
	// sendfile が使えなかった (何も送らないうちに分かった) ときは、読んで write で送る
	if sent == 0 && !errors.Is(err, os.ErrDeadlineExceeded) {
		if b, rerr := v.readFile(); rerr == nil {
			if _, werr := cn.nc.Write(b); werr == nil {
				c.bytes = int64(len(b))
				cn.closeAfter = !cn.req.keepAlive
			}
		}
	}
}

// sendText は本文が短い文字だけの返事 (エラーなど)。extra は 名前, 値, 名前, 値 ...
func sendText(c *Ctx, status int, text string, extra ...string) {
	cn := c.conn
	b := append(cn.scratch[:0], statusLine(status)...)
	b = appendHeader(b, "Content-Type", "text/plain; charset=utf-8")
	b = appendHeader(b, "Content-Length", strconv.Itoa(len(text)))
	b = appendHeader(b, "Cache-Control", "no-store")
	for i := 0; i+1 < len(extra); i += 2 {
		b = appendHeader(b, extra[i], extra[i+1])
	}
	cn.scratch = b
	c.write(status, b, c.debugLine(), []byte(text))
}

var notFoundHead = append(append([]byte(nil), statusLine(404)...), "Content-Type: text/plain; charset=utf-8\r\nContent-Length: 10\r\n"...)
var notFoundBody = []byte("Not Found\n")

// notFound は 404。Cache-Control は付けない (nginx と同じ。何度も来る探りは Cloudflare が受けてくれる)
func notFound(c *Ctx) {
	c.write(404, notFoundHead, c.debugLine(), notFoundBody)
}

func badGateway(c *Ctx) {
	sendText(c, http.StatusBadGateway, "Bad Gateway\n")
}

// ───── ためずに流す返事 (動画、SSE など)

// stream は頭書きを送ったあとの、本文の書き込み口
type stream struct {
	c       *Ctx
	chunked bool
}

// beginStream は頭書きを送る。contentLength が負なら、長さの分からない本文 (チャンクで送る。HTTP/1.0 の客には、閉じて終わらせる)。
// headers は状態の行のあとに付ける頭書き (Content-Length などの框組みは、ここで付ける)
func (c *Ctx) beginStream(status int, headers []byte, contentLength int64) (*stream, error) {
	cn := c.conn
	s := &stream{c: c}
	b := append(cn.scratch[:0], statusLine(status)...)
	b = append(b, headers...)
	hasBody := !c.head && status != 204 && status != 304
	switch {
	case status == 204 || status == 304:
		// 本文も長さもない
	case contentLength >= 0:
		// HEAD でも、元が伝えた長さは、そのまま伝える
		b = appendHeader(b, "Content-Length", strconv.FormatInt(contentLength, 10))
	case hasBody && c.req.http11:
		b = appendHeader(b, "Transfer-Encoding", "chunked")
		s.chunked = true
	case hasBody:
		// HTTP/1.0: 閉じて終わりを伝える
		cn.closeAfter = true
	}
	cn.scratch = b
	err := c.write(status, b, c.debugLine(), nil)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// write は本文の一部を送る。送るたびに、止まった客に待たされないよう時間を区切る
func (s *stream) write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	cn := s.c.conn
	iov := cn.iov[:0]
	if s.chunked {
		hex := strconv.AppendInt(cn.chunk[:0], int64(len(p)), 16)
		hex = append(hex, '\r', '\n')
		iov = append(iov, hex, p, crlf)
	} else {
		iov = append(iov, p)
	}
	cn.bufs = iov
	cn.armWrite(time.Now(), true)
	_, err := cn.bufs.WriteTo(cn.nc)
	if err != nil {
		cn.closeAfter = true
		return err
	}
	s.c.bytes += int64(len(p))
	return nil
}

var lastChunk = []byte("0\r\n\r\n")

// finish は本文を送り終えたことを伝える
func (s *stream) finish() {
	if !s.chunked {
		return
	}
	cn := s.c.conn
	cn.armWrite(time.Now(), true)
	if _, err := cn.nc.Write(lastChunk); err != nil {
		cn.closeAfter = true
	}
}

// watcher は流しているあいだ、客が切れたかを見張る (次のリクエストの先頭は、読まずに残す)
type watcher struct {
	done chan struct{}
	// 客が切れていた (done が閉じたあとに読む)
	flag bool
}

// watch は客が切れたら cancel を呼ぶ。stopWatch で止める
func (cn *conn) watch(cancel context.CancelFunc) *watcher {
	w := &watcher{done: make(chan struct{})}
	go func() {
		defer close(w.done)
		if _, err := cn.br.Peek(1); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			w.flag = true
			cancel()
		}
	}()
	return w
}

func (cn *conn) stopWatch(w *watcher) {
	cn.nc.SetReadDeadline(time.Now().Add(-time.Second))
	<-w.done
	cn.nc.SetReadDeadline(time.Time{})
	if w.flag {
		cn.closeAfter = true
	}
}

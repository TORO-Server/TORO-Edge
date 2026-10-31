package main

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// このプロキシだけのための、小さな HTTP/1.1 サーバー。
// 前にいるのは Cloudflare (cloudflared) と、Docker の中の自分たちだけなので、必要なものだけを、厳しめに作ってある:
//   - 読むのは GET / HEAD (本文があるリクエストは断って、つなぎを閉じる)。読む頭書きは types.go の hid だけ
//   - 返事は、頭書きと本文を 1 回の writev で送る (net/http は、4 KB を超えると 2 回に分ける)
//   - 頭書きは、返事ができたときに文字列にしてあるので、返すときは並べて送るだけ

const (
	maxHeaderBytes    = 16 << 10
	idleTimeout       = 125 * time.Second
	readHeaderTimeout = 30 * time.Second
	writeTimeout      = 120 * time.Second
	// これより大きい本文を送るときは、止まった客に待たされないよう、書く時間に上限を付ける
	bigWrite = 256 << 10
)

var (
	crlf          = []byte("\r\n")
	connCloseTail = []byte("Connection: close\r\n\r\n")
)

var statusLines = func() map[int][]byte {
	m := map[int][]byte{}
	for _, code := range []int{100, 101, 200, 204, 206, 301, 302, 304, 400, 403, 404, 405, 414, 416, 426, 429, 431, 500, 502, 503, 504} {
		m[code] = []byte("HTTP/1.1 " + strconv.Itoa(code) + " " + http.StatusText(code) + "\r\n")
	}
	return m
}()

func statusLine(code int) []byte {
	if b, ok := statusLines[code]; ok {
		return b
	}
	return []byte("HTTP/1.1 " + strconv.Itoa(code) + " " + http.StatusText(code) + "\r\n")
}

type dateEntry struct {
	sec  int64
	line []byte
}

var dateCache atomic.Pointer[dateEntry]

// dateLine は Date の頭書きの 1 行 (1 秒に 1 回だけ作る)
func dateLine(now time.Time) []byte {
	sec := now.Unix()
	if e := dateCache.Load(); e != nil && e.sec == sec {
		return e.line
	}
	line := []byte("Date: " + now.UTC().Format(http.TimeFormat) + "\r\n")
	dateCache.Store(&dateEntry{sec: sec, line: line})
	return line
}

// Server は小さな HTTP/1.1 サーバー
type Server struct {
	// 経路を選んで返す。返したら、状態は c.status に入れておく
	handler func(c *Ctx)
	// 返し終えたあと (ログと数)
	onDone  func(c *Ctx)
	closing atomic.Bool
	conns   sync.Map
	nextID  atomic.Uint32
	// ln は Serve が入れ、Shutdown が閉じる (mu で守る)
	mu      sync.Mutex
	ln      net.Listener
	ctxPool sync.Pool
	rdPool  sync.Pool
}

func newServer(handler func(c *Ctx), onDone func(c *Ctx)) *Server {
	s := &Server{handler: handler, onDone: onDone}
	s.ctxPool.New = func() any { return new(Ctx) }
	s.rdPool.New = func() any { return bufio.NewReaderSize(nil, maxHeaderBytes) }
	return s
}

// Serve はつなぎを受けて、1 つずつ goroutine で返す
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closing.Load() {
		s.mu.Unlock()
		ln.Close()
		return nil
	}
	s.ln = ln
	s.mu.Unlock()
	var delay time.Duration
	for {
		nc, err := ln.Accept()
		if err != nil {
			if s.closing.Load() {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else if delay *= 2; delay > time.Second {
					delay = time.Second
				}
				time.Sleep(delay)
				continue
			}
			return err
		}
		delay = 0
		go s.serveConn(nc)
	}
}

// conn は 1 つのつなぎ (客 1 つ)
type conn struct {
	nc  net.Conn
	br  *bufio.Reader
	srv *Server
	id  uint32
	// 客の IP
	remote string
	req    Request
	// 次のリクエストを待っているあいだ (止めるときに、すぐ閉じてよい)
	idle atomic.Bool
	// この返事のあとで、つなぎを閉じる
	closeAfter bool
	// sendfile に使う raw 口 (必要になったとき求める)
	rc syscall.RawConn
	// writev に渡す並び (返事ごとに作り直さない)
	iov     [6][]byte
	bufs    net.Buffers
	scratch []byte
	dbg     []byte
	chunk   [24]byte
}

func (s *Server) serveConn(nc net.Conn) {
	cn := &conn{nc: nc, srv: s, id: s.nextID.Add(1)}
	cn.remote = remoteHost(nc.RemoteAddr().String())
	if t, ok := nc.(*net.TCPConn); ok {
		t.SetKeepAlive(true)
		t.SetKeepAlivePeriod(30 * time.Second)
	}
	br := s.rdPool.Get().(*bufio.Reader)
	br.Reset(nc)
	cn.br = br
	s.conns.Store(cn, struct{}{})
	defer func() {
		s.conns.Delete(cn)
		nc.Close()
		br.Reset(nil)
		s.rdPool.Put(br)
	}()
	for {
		if s.closing.Load() {
			return
		}
		cn.idle.Store(true)
		nc.SetReadDeadline(time.Now().Add(idleTimeout))
		if _, err := br.Peek(1); err != nil {
			return
		}
		cn.idle.Store(false)
		nc.SetReadDeadline(time.Now().Add(readHeaderTimeout))
		code, err := cn.readRequest()
		if err != nil {
			if code != 0 {
				cn.reject(code)
			}
			return
		}
		nc.SetReadDeadline(time.Time{})
		if !cn.serveRequest() {
			return
		}
	}
}

func remoteHost(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return strings.Trim(addr[:i], "[]")
	}
	return addr
}

// serveRequest は読んだリクエスト 1 つに答える。つなぎを使い続けてよければ true
func (cn *conn) serveRequest() bool {
	s := cn.srv
	req := &cn.req
	c := s.ctxPool.Get().(*Ctx)
	*c = Ctx{conn: cn, req: req, method: req.Method, head: req.Method == "HEAD", cache: "-", log: true, start: time.Now()}
	c.path = req.Target
	if q := strings.IndexByte(req.Target, '?'); q >= 0 {
		c.path, c.search = req.Target[:q], req.Target[q:]
	}
	c.enc = acceptedEncodings(req.h[hAcceptEncoding])
	cn.closeAfter = !req.keepAlive
	s.handler(c)
	if c.status == 0 {
		// 何も返さずに終わった (客が切れた)
		cn.closeAfter = true
		c.status = 499
	}
	s.onDone(c)
	keep := !cn.closeAfter && !s.closing.Load()
	s.ctxPool.Put(c)
	return keep
}

var (
	errBadRequest = errors.New("bad request")
)

// readRequest はリクエストの先頭 (1 行目と頭書き) を読む。だめなら、返す状態 (つなぎの不具合なら 0) とエラー
func (cn *conn) readRequest() (int, error) {
	req := &cn.req
	*req = Request{remote: cn.remote}
	br := cn.br
	line, err := br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return http.StatusRequestURITooLong, errBadRequest
		}
		return 0, err
	}
	total := len(line)
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return http.StatusBadRequest, errBadRequest
	}
	line = line[:len(line)-2]
	sp1 := bytes.IndexByte(line, ' ')
	sp2 := bytes.LastIndexByte(line, ' ')
	if sp1 <= 0 || sp2 <= sp1 {
		return http.StatusBadRequest, errBadRequest
	}
	switch string(line[sp2+1:]) {
	case "HTTP/1.1":
		req.http11 = true
	case "HTTP/1.0":
	default:
		return http.StatusBadRequest, errBadRequest
	}
	target := line[sp1+1 : sp2]
	if len(target) == 0 || target[0] != '/' {
		return http.StatusBadRequest, errBadRequest
	}
	for _, b := range target {
		if b <= 0x20 || b == 0x7f {
			return http.StatusBadRequest, errBadRequest
		}
	}
	switch string(line[:sp1]) {
	case "GET":
		req.Method = "GET"
	case "HEAD":
		req.Method = "HEAD"
	default:
		req.Method = string(line[:sp1])
	}
	req.Target = string(target)

	var lower [40]byte
	for {
		line, err = br.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return http.StatusRequestHeaderFieldsTooLarge, errBadRequest
			}
			return 0, err
		}
		total += len(line)
		if total > maxHeaderBytes {
			return http.StatusRequestHeaderFieldsTooLarge, errBadRequest
		}
		if len(line) == 2 && line[0] == '\r' {
			break
		}
		if len(line) < 3 || line[len(line)-2] != '\r' {
			return http.StatusBadRequest, errBadRequest
		}
		line = line[:len(line)-2]
		// 折り返した頭書き (obs-fold) は受けない
		if line[0] == ' ' || line[0] == '\t' {
			return http.StatusBadRequest, errBadRequest
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return http.StatusBadRequest, errBadRequest
		}
		name := line[:colon]
		if bytes.IndexByte(name, ' ') >= 0 || bytes.IndexByte(name, '\t') >= 0 {
			return http.StatusBadRequest, errBadRequest
		}
		value := bytes.Trim(line[colon+1:], " \t")
		if bytes.IndexByte(value, '\r') >= 0 || bytes.IndexByte(value, 0) >= 0 {
			return http.StatusBadRequest, errBadRequest
		}
		if len(name) > len(lower) {
			continue
		}
		for i, b := range name {
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			lower[i] = b
		}
		id, ok := hdrIDs[string(lower[:len(name)])]
		if !ok {
			continue
		}
		if req.h[id] == "" {
			req.h[id] = string(value)
		} else if id == hContentLength || id == hHost {
			return http.StatusBadRequest, errBadRequest
		} else {
			req.h[id] += ", " + string(value)
		}
	}
	req.Host = req.h[hHost]
	if req.http11 && req.Host == "" {
		return http.StatusBadRequest, errBadRequest
	}
	if cl := req.h[hContentLength]; cl != "" && cl != "0" {
		req.hasBody = true
	}
	if req.h[hTransferEncoding] != "" {
		req.hasBody = true
	}
	connection := req.h[hConnection]
	if req.http11 {
		req.keepAlive = !hasToken(connection, "close")
	} else {
		req.keepAlive = hasToken(connection, "keep-alive")
	}
	req.upgrade = req.h[hUpgrade] != "" && asciiEqualFold(strings.TrimSpace(req.h[hUpgrade]), "websocket") && hasToken(connection, "upgrade")
	return 0, nil
}

// reject は読めなかったリクエストに、状態だけを返して閉じる
func (cn *conn) reject(code int) {
	b := append([]byte(nil), statusLine(code)...)
	b = append(b, dateLine(time.Now())...)
	b = append(b, "Content-Length: 0\r\nConnection: close\r\n\r\n"...)
	cn.nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	cn.nc.Write(b)
	stats.requests.add(cn.id, 1)
	countStatus(cn.id, code)
}

// closing はこの返事のあとで閉じるか (Connection: close を付ける)
func (cn *conn) closing() bool {
	if !cn.closeAfter && cn.srv.closing.Load() {
		cn.closeAfter = true
	}
	return cn.closeAfter
}

// Shutdown は新しいつなぎを受けず、返し終えていないものを grace だけ待つ
func (s *Server) Shutdown(grace time.Duration) {
	s.mu.Lock()
	s.closing.Store(true)
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	deadline := time.Now().Add(grace)
	for {
		n := 0
		s.conns.Range(func(k, _ any) bool {
			cn := k.(*conn)
			n++
			if cn.idle.Load() {
				cn.nc.Close()
			}
			return true
		})
		if n == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.conns.Range(func(k, _ any) bool {
		k.(*conn).nc.Close()
		return true
	})
}

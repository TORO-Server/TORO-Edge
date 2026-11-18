package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"time"
)

// パススルー: ためない・読み替えない中継。auth、mcp、pass-api と、api.torosaba.net の /map/ と /map3d/ 以外の全部に使う。
//
// ここまでの経路 (GET / HEAD だけ、読む頭書きは決めたものだけ) では、認証のログイン (POST、Cookie、Location の素通し)、
// MCP (POST の本文と SSE)、Wiki の書き込みを渡せない。そこで、サイトが「このパスはパススルー」と決めたリクエストだけ、
// 読み終えた先頭 (cn.rawHead) と、つなぎそのものを net/http のサーバーへ渡し、httputil.ReverseProxy で元へ流す。
//   - 頭書き (Cookie、Authorization、Content-Type など) と本文 (Content-Length も chunked も) は、そのまま渡す。Host は客のものを保つ
//   - 返事は流しながら返す (FlushInterval -1。SSE や長い返事をためない)。Location と Set-Cookie は触らない
//   - WebSocket など Upgrade は ReverseProxy が扱う
//   - つなぎは 1 リクエストごとに閉じる (Connection: close)。続きのリクエストは edge の本体へ戻って振り分けられる
//   - 客が書いた X-Toro-Client-Ip は渡さない (Cloudflare の cf-connecting-ip があるときだけ、そこから作り直す)
//   - アクセスログには、クエリ (認可コードなどが入りうる) を出さない

// Passthrough は元のサーバー 1 つへのパススルー
type Passthrough struct {
	name  string
	proxy *httputil.ReverseProxy
	tr    *http.Transport
}

func newPassthrough(name string, ep Endpoint, headerTimeout time.Duration) *Passthrough {
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		// 圧縮は触らない (客の Accept-Encoding をそのまま渡し、元の Content-Encoding をそのまま返す)
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: headerTimeout,
	}
	addr := ep.Addr()
	p := &Passthrough{name: name, tr: tr}
	p.proxy = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			r.URL.Host = addr
			// r.Host (客の Host) はそのまま。X-Forwarded-For は ReverseProxy が、つないできた相手 (cloudflared) を足す
			r.Header.Del("X-Toro-Client-Ip")
			if cf := r.Header.Get("Cf-Connecting-Ip"); cf != "" {
				r.Header.Set("X-Toro-Client-Ip", cf)
			}
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			stats.upstreamErrors.Add(1)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, "Bad Gateway\n")
		},
	}
	return p
}

func (p *Passthrough) close() { p.tr.CloseIdleConnections() }

// handoffConn は、先頭を読み終えたつなぎを net/http のサーバーへ渡すための包み。
// 読むのは、先頭の写し → 先に読んでしまっていた分 → つなぎ の順 (bufio のバッファはここでは使わない)
type handoffConn struct {
	net.Conn
	r      io.Reader
	pass   *Passthrough
	status atomic.Int32
	bytes  atomic.Int64
	done   chan struct{}
	once   sync.Once
}

func (h *handoffConn) Read(b []byte) (int, error) { return h.r.Read(b) }

// Close は net/http の側が閉じたとき (返し終えた、WebSocket が終わった)。待っている edge の本体を進める
func (h *handoffConn) Close() error {
	err := h.Conn.Close()
	h.once.Do(func() { close(h.done) })
	return err
}

type passCtxKey struct{}

// recorder は返事の状態とバイト数を覚える (アクセスログと /stats のため)
type recorder struct {
	http.ResponseWriter
	h *handoffConn
}

func (r *recorder) WriteHeader(code int) {
	if code >= 200 || code == http.StatusSwitchingProtocols {
		r.h.status.CompareAndSwap(0, int32(code))
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.h.status.CompareAndSwap(0, http.StatusOK)
	n, err := r.ResponseWriter.Write(b)
	r.h.bytes.Add(int64(n))
	return n, err
}

// Unwrap は http.ResponseController (Flush と Hijack) が、下の ResponseWriter に届くようにする
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Hijack は WebSocket など (101): つなぎを渡すときに、状態を 101 として覚える
func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	r.h.status.CompareAndSwap(0, http.StatusSwitchingProtocols)
	return hj.Hijack()
}

// chanListener は、edge の本体が渡してくるつなぎを 1 つずつ Accept させる
type chanListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newChanListener() *chanListener {
	return &chanListener{ch: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{} }

func (l *chanListener) put(c net.Conn) bool {
	select {
	case l.ch <- c:
		return true
	case <-l.closed:
		return false
	}
}

// Handoff は、パススルーするつなぎを受ける net/http のサーバー 1 つ
type Handoff struct {
	srv *http.Server
	ln  *chanListener
}

func newHandoff() *Handoff {
	h := &Handoff{ln: newChanListener()}
	h.srv = &http.Server{
		Handler:           http.HandlerFunc(serveHandoff),
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, passCtxKey{}, c)
		},
	}
	go h.srv.Serve(h.ln)
	return h
}

func serveHandoff(w http.ResponseWriter, r *http.Request) {
	hc, _ := r.Context().Value(passCtxKey{}).(*handoffConn)
	if hc == nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// 1 リクエストごとに閉じる (続きは edge の本体が受ける)
	w.Header().Set("Connection", "close")
	hc.pass.proxy.ServeHTTP(&recorder{ResponseWriter: w, h: hc}, r)
}

func (h *Handoff) close() {
	h.ln.Close()
	h.srv.Close()
}

// servePass は先頭を読み終えたリクエストを、つなぎごと net/http へ渡して、終わるまで待つ
func (e *Edge) servePass(c *Ctx, p *Passthrough) {
	cn := c.conn
	// アクセスログにクエリを出さない (認可コードなどが入りうる)
	c.search = ""
	c.cache = "BYPASS"
	c.log = true
	cn.closeAfter = true
	if e.srv.closing.Load() {
		sendText(c, http.StatusServiceUnavailable, "Service Unavailable\n")
		return
	}
	// bufio が先に読んでしまっていた分 (本文の頭など) を取り出す。このあと bufio は使わない
	var ahead []byte
	if n := cn.br.Buffered(); n > 0 {
		b, _ := cn.br.Peek(n)
		ahead = append([]byte(nil), b...)
		cn.br.Discard(n)
	}
	hc := &handoffConn{
		Conn: cn.nc,
		r:    io.MultiReader(bytes.NewReader(append([]byte(nil), cn.rawHead...)), bytes.NewReader(ahead), cn.nc),
		pass: p,
		done: make(chan struct{}),
	}
	if !e.handoff.ln.put(hc) {
		sendText(c, http.StatusServiceUnavailable, "Service Unavailable\n")
		return
	}
	<-hc.done
	c.status = int(hc.status.Load())
	c.bytes = hc.bytes.Load()
}

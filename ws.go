package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 同時につなげる WebSocket の数
const maxSockets = 4096

var (
	wsMu    sync.Mutex
	wsConns = map[net.Conn]struct{}{}
	// 枠を取った数 (つなぎに行く前に取る)
	wsSlots int
)

var wsForward = []hid{hSecWSKey, hSecWSVersion, hSecWSProtocol, hSecWSExtensions, hOrigin, hUserAgent, hCfConnectingIP}

func tcpTune(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		t.SetNoDelay(true)
		t.SetKeepAlive(true)
		t.SetKeepAlivePeriod(30 * time.Second)
	}
}

const wsIdleTimeout = 120 * time.Second

type timeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (c *timeoutConn) Read(b []byte) (n int, err error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(b)
}

func (c *timeoutConn) Write(b []byte) (n int, err error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(b)
}

// proxyUpgrade はブラウザと元の WebSocket を、つないだあとは何も解かずにそのまま渡す (メッセージの形は知らなくてよい)。
// 元が 101 を返さなかったら、その返事の状態だけを返して閉じる
func proxyUpgrade(c *Ctx, target Endpoint, path string) {
	cn := c.conn
	// つなぎはこのあと WebSocket のものになるので、終わったら閉じる
	cn.closeAfter = true
	ipKey := clientKey(c.req)
	if !socketLimit.enter(ipKey) {
		tooManyStreams(c)
		return
	}
	defer socketLimit.leave(ipKey)
	wsMu.Lock()
	full := wsSlots >= maxSockets
	if !full {
		wsSlots++
	}
	wsMu.Unlock()
	if full {
		sendText(c, http.StatusServiceUnavailable, "Service Unavailable\n")
		return
	}
	defer func() {
		wsMu.Lock()
		wsSlots--
		wsMu.Unlock()
	}()
	up, err := net.DialTimeout("tcp", target.Addr(), 10*time.Second)
	if err != nil {
		badGateway(c)
		return
	}
	tcpTune(up)
	r := c.req
	var req strings.Builder
	req.WriteString("GET " + path + " HTTP/1.1\r\n")
	req.WriteString("Host: " + r.Host + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n")
	if xff := r.h[hXForwardedFor]; xff != "" {
		req.WriteString("X-Forwarded-For: " + xff + ", " + r.remote + "\r\n")
	} else {
		req.WriteString("X-Forwarded-For: " + r.remote + "\r\n")
	}
	proto := r.h[hXForwardedProto]
	if proto == "" {
		proto = "http"
	}
	req.WriteString("X-Forwarded-Proto: " + proto + "\r\n")
	for _, id := range wsForward {
		if v := r.h[id]; v != "" {
			req.WriteString(hdrNames[id] + ": " + v + "\r\n")
		}
	}
	req.WriteString("\r\n")
	up.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(up, req.String()); err != nil {
		up.Close()
		badGateway(c)
		return
	}
	upReader := bufio.NewReader(up)
	resp, err := http.ReadResponse(upReader, nil)
	if err != nil {
		up.Close()
		badGateway(c)
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		up.Close()
		// その返事の状態だけを返す
		head := append([]byte(nil), statusLine(resp.StatusCode)...)
		head = appendHeader(head, "Content-Length", "0")
		c.write(resp.StatusCode, head, nil, nil)
		return
	}
	var head strings.Builder
	head.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	for k, vals := range resp.Header {
		for _, v := range vals {
			head.WriteString(k + ": " + v + "\r\n")
		}
	}
	head.WriteString("\r\n")
	cn.nc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(cn.nc, head.String()); err != nil {
		up.Close()
		return
	}
	c.status = http.StatusSwitchingProtocols
	tcpTune(cn.nc)
	// それぞれが先に読んでしまっていた分
	if n := upReader.Buffered(); n > 0 {
		b, _ := upReader.Peek(n)
		cn.nc.Write(b)
	}
	if n := cn.br.Buffered(); n > 0 {
		b, _ := cn.br.Peek(n)
		up.Write(b)
	}
	up.SetDeadline(time.Time{})
	cn.nc.SetWriteDeadline(time.Time{})

	client := cn.nc
	wsMu.Lock()
	wsConns[client] = struct{}{}
	wsConns[up] = struct{}{}
	wsMu.Unlock()
	stats.wsOpen.Add(1)
	stats.wsTotal.Add(1)

	var once sync.Once
	end := func() {
		once.Do(func() {
			stats.wsOpen.Add(-1)
			wsMu.Lock()
			delete(wsConns, client)
			delete(wsConns, up)
			wsMu.Unlock()
			client.Close()
			up.Close()
		})
	}
	tcClient := &timeoutConn{Conn: client, timeout: wsIdleTimeout}
	tcUp := &timeoutConn{Conn: up, timeout: wsIdleTimeout}
	go func() {
		// ここで panic しても全体を止めない
		defer func() { recover() }()
		defer end()
		io.Copy(tcUp, tcClient)
	}()
	defer end()
	io.Copy(tcClient, tcUp)
}

// closeAllSockets は止めるとき: つながっている WebSocket を閉じる (ブラウザは、つなぎ直すか、取りに来る方に戻る)
func closeAllSockets() {
	wsMu.Lock()
	for c := range wsConns {
		c.Close()
	}
	wsMu.Unlock()
}

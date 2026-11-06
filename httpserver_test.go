package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

var bigBody = func() []byte {
	b := make([]byte, 1<<20)
	rand.Read(b)
	return b
}()

// testHandler は、試験用の経路 (本物の経路は、Edge と sites.go)
func testHandler(c *Ctx) {
	switch c.path {
	case "/big":
		head := append([]byte(nil), statusLine(200)...)
		head = appendHeader(head, "Content-Type", "application/octet-stream")
		head = appendHeader(head, "Content-Length", strconv.Itoa(len(bigBody)))
		c.write(200, head, nil, bigBody)
	case "/stream":
		s, err := c.beginStream(200, []byte("Content-Type: text/plain\r\n"), -1)
		if err != nil {
			return
		}
		for _, part := range []string{"alpha", "", "bravo-bravo", strings.Repeat("c", 70000)} {
			s.write([]byte(part))
		}
		s.finish()
	case "/fixed":
		s, err := c.beginStream(200, []byte("Content-Type: text/plain\r\n"), 5)
		if err != nil {
			return
		}
		s.write([]byte("hello"))
		s.finish()
	case "/body":
		sendText(c, 200, strconv.FormatBool(c.req.hasBody)+"\n")
	case "/host":
		sendText(c, 200, c.req.Host+"|"+c.req.h[hXForwardedFor]+"|"+c.req.h[hAcceptEncoding]+"\n")
	default:
		sendText(c, 200, "ok\n")
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func startTestServer(t *testing.T) (string, *Server) {
	t.Helper()
	srv := newServer(testHandler, func(c *Ctx) {})
	ln := listen(t)
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(200 * time.Millisecond) })
	return ln.Addr().String(), srv
}

func dial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	return c, bufio.NewReader(c)
}

func readResp(t *testing.T, br *bufio.Reader, method string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, body
}

// closed は、相手がつなぎを閉じたか (これ以上、何も送ってこないか)。
// こちらが送った分を読み切らずに閉じられたときは、EOF ではなくリセットになる
func closed(c net.Conn, br *bufio.Reader) bool {
	c.SetReadDeadline(time.Now().Add(1 * time.Second))
	_, err := br.ReadByte()
	if err == nil {
		return false
	}
	var ne net.Error
	return !(errors.As(err, &ne) && ne.Timeout())
}

func TestBasicKeepAliveAndDate(t *testing.T) {
	addr, _ := startTestServer(t)
	c, br := dial(t, addr)
	for i := 0; i < 3; i++ {
		io.WriteString(c, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n")
		resp, body := readResp(t, br, "GET")
		if resp.StatusCode != 200 || string(body) != "ok\n" {
			t.Fatalf("round %d: %d %q", i, resp.StatusCode, body)
		}
		if resp.Header.Get("Date") == "" || resp.Header.Get("Content-Length") != "3" {
			t.Fatalf("headers: %v", resp.Header)
		}
	}
}

func TestPipelining(t *testing.T) {
	addr, _ := startTestServer(t)
	c, br := dial(t, addr)
	io.WriteString(c, "GET /host HTTP/1.1\r\nHost: one\r\n\r\nGET /body HTTP/1.1\r\nHost: two\r\n\r\nGET /x HTTP/1.1\r\nHost: three\r\n\r\n")
	for _, want := range []string{"one||\n", "false\n", "ok\n"} {
		_, body := readResp(t, br, "GET")
		if string(body) != want {
			t.Fatalf("got %q want %q", body, want)
		}
	}
}

func TestHeadHasNoBodyButLength(t *testing.T) {
	addr, _ := startTestServer(t)
	c, br := dial(t, addr)
	io.WriteString(c, "HEAD /x HTTP/1.1\r\nHost: a\r\n\r\nGET /x HTTP/1.1\r\nHost: a\r\n\r\n")
	resp, body := readResp(t, br, "HEAD")
	if resp.Header.Get("Content-Length") != "3" || len(body) != 0 {
		t.Fatalf("HEAD: %v %q", resp.Header, body)
	}
	// HEAD のあとも、同じつなぎで続けられる (本文を送っていないので、ずれない)
	_, body = readResp(t, br, "GET")
	if string(body) != "ok\n" {
		t.Fatalf("after HEAD: %q", body)
	}
}

func TestConnectionHandling(t *testing.T) {
	addr, _ := startTestServer(t)

	c, br := dial(t, addr)
	io.WriteString(c, "GET /x HTTP/1.1\r\nHost: a\r\nConnection: close\r\n\r\n")
	resp, _ := readResp(t, br, "GET")
	if !resp.Close || !closed(c, br) {
		t.Fatalf("Connection: close not honored: %+v", resp.Header)
	}

	c, br = dial(t, addr)
	io.WriteString(c, "GET /x HTTP/1.0\r\n\r\n")
	if _, body := readResp(t, br, "GET"); string(body) != "ok\n" || !closed(c, br) {
		t.Fatal("HTTP/1.0 must close after the response")
	}

	c, br = dial(t, addr)
	io.WriteString(c, "GET /x HTTP/1.0\r\nConnection: keep-alive\r\n\r\nGET /x HTTP/1.0\r\n\r\n")
	for i := 0; i < 2; i++ {
		if _, body := readResp(t, br, "GET"); string(body) != "ok\n" {
			t.Fatalf("1.0 keep-alive round %d", i)
		}
	}
}

func TestHeadersAreReadCaseInsensitivelyAndJoined(t *testing.T) {
	addr, _ := startTestServer(t)
	c, br := dial(t, addr)
	io.WriteString(c, "GET /host HTTP/1.1\r\nhOsT: Example.COM\r\nx-forwarded-for: 1.1.1.1\r\nX-Forwarded-For:  2.2.2.2 \r\nACCEPT-ENCODING: gzip\r\nCookie: ignored=1\r\n\r\n")
	_, body := readResp(t, br, "GET")
	if string(body) != "Example.COM|1.1.1.1, 2.2.2.2|gzip\n" {
		t.Fatalf("got %q", body)
	}
}

func TestRequestBodyIsFlagged(t *testing.T) {
	addr, _ := startTestServer(t)
	// Transfer-Encoding は値が空でも、本文があるものとして断る
	for _, extra := range []string{"Content-Length: 5\r\n", "Transfer-Encoding: chunked\r\n", "Transfer-Encoding:\r\n"} {
		c, br := dial(t, addr)
		io.WriteString(c, "GET /body HTTP/1.1\r\nHost: a\r\n"+extra+"\r\n")
		_, body := readResp(t, br, "GET")
		if string(body) != "true\n" {
			t.Fatalf("%q: %q", extra, body)
		}
	}
	c, br := dial(t, addr)
	io.WriteString(c, "GET /body HTTP/1.1\r\nHost: a\r\nContent-Length: 0\r\n\r\n")
	if _, body := readResp(t, br, "GET"); string(body) != "false\n" {
		t.Fatalf("Content-Length: 0 must not count as a body: %q", body)
	}
}

// 読めないリクエストには状態だけを返して、つなぎを閉じる (スマグリングの足がかりにしない)
func TestMalformedRequestsAreRejected(t *testing.T) {
	addr, _ := startTestServer(t)
	long := strings.Repeat("a", 17<<10)
	cases := []struct {
		name string
		req  string
		want int
	}{
		{"bare LF request line", "GET /x HTTP/1.1\nHost: a\r\n\r\n", 400},
		{"bare LF header", "GET /x HTTP/1.1\r\nHost: a\nX: 1\r\n\r\n", 400},
		{"obs-fold", "GET /x HTTP/1.1\r\nHost: a\r\nX: 1\r\n  folded\r\n\r\n", 400},
		{"duplicate content-length", "GET /x HTTP/1.1\r\nHost: a\r\nContent-Length: 1\r\nContent-Length: 1\r\n\r\n", 400},
		{"duplicate host", "GET /x HTTP/1.1\r\nHost: a\r\nHost: b\r\n\r\n", 400},
		{"missing host (1.1)", "GET /x HTTP/1.1\r\n\r\n", 400},
		{"space in header name", "GET /x HTTP/1.1\r\nHost: a\r\nBad Name: 1\r\n\r\n", 400},
		{"no colon", "GET /x HTTP/1.1\r\nHost: a\r\nnocolon\r\n\r\n", 400},
		{"absolute-form target", "GET http://a/x HTTP/1.1\r\nHost: a\r\n\r\n", 400},
		{"control char in target", "GET /x\x01y HTTP/1.1\r\nHost: a\r\n\r\n", 400},
		{"unknown version", "GET /x HTTP/2.0\r\nHost: a\r\n\r\n", 400},
		{"no version", "GET /x\r\nHost: a\r\n\r\n", 400},
		{"NUL in value", "GET /x HTTP/1.1\r\nHost: a\r\nX: a\x00b\r\n\r\n", 400},
		{"control char in value", "GET /x HTTP/1.1\r\nHost: a\r\nX: a\x01b\r\n\r\n", 400},
		{"DEL in value", "GET /x HTTP/1.1\r\nHost: a\r\nX: a\x7fb\r\n\r\n", 400},
		{"CR in value", "GET /x HTTP/1.1\r\nHost: a\r\nX: a\rb\r\n\r\n", 400},
		{"control char in known value", "GET /x HTTP/1.1\r\nHost: a\x01\r\n\r\n", 400},
		{"separator in header name", "GET /x HTTP/1.1\r\nHost: a\r\nBad(Name: 1\r\n\r\n", 400},
		{"control char in header name", "GET /x HTTP/1.1\r\nHost: a\r\nBa\x01d: 1\r\n\r\n", 400},
		{"long bad header name", "GET /x HTTP/1.1\r\nHost: a\r\n" + strings.Repeat("n", 50) + "(: 1\r\n\r\n", 400},
		{"separator in method", "G(T /x HTTP/1.1\r\nHost: a\r\n\r\n", 400},
		{"duplicate host after an empty one", "GET /x HTTP/1.1\r\nHost:\r\nHost: b\r\n\r\n", 400},
		{"duplicate content-length after an empty one", "GET /x HTTP/1.1\r\nHost: a\r\nContent-Length:\r\nContent-Length: 1\r\n\r\n", 400},
		{"empty content-length", "GET /x HTTP/1.1\r\nHost: a\r\nContent-Length:\r\n\r\n", 400},
		{"too large headers", "GET /x HTTP/1.1\r\nHost: a\r\nX: " + long + "\r\n\r\n", 431},
		{"too long request line", "GET /" + long + " HTTP/1.1\r\nHost: a\r\n\r\n", 414},
	}
	for _, tc := range cases {
		c, br := dial(t, addr)
		io.WriteString(c, tc.req)
		resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if resp.StatusCode != tc.want || !resp.Close || !closed(c, br) {
			t.Errorf("%s: status %d (want %d), close=%v", tc.name, resp.StatusCode, tc.want, resp.Close)
		}
	}
}

// 厳しくしても、普通の頭書きは通す (タブ、obs-text、token の記号、知らない長い名前、値が空のもの)
func TestUnusualButValidRequestsAreAccepted(t *testing.T) {
	addr, _ := startTestServer(t)
	for _, req := range []string{
		"GET /x HTTP/1.1\r\nHost: a\r\nX: a\tb\r\n\r\n",
		"GET /x HTTP/1.1\r\nHost: a\r\nX: caf\xc3\xa9\r\n\r\n",
		"GET /x HTTP/1.1\r\nHost: a\r\nX-!#$%&'*+-.^_`|~: 1\r\n\r\n",
		"GET /x HTTP/1.1\r\nHost: a\r\n" + strings.Repeat("n", 50) + ": 1\r\n\r\n",
		"GET /x HTTP/1.1\r\nHost: a\r\nAccept:\r\nAccept: text/plain\r\n\r\n",
		"OPTIONS /x HTTP/1.1\r\nHost: a\r\n\r\n",
	} {
		c, br := dial(t, addr)
		io.WriteString(c, req)
		if resp, body := readResp(t, br, "GET"); resp.StatusCode != 200 || string(body) != "ok\n" {
			t.Errorf("%q: %d %q", req, resp.StatusCode, body)
		}
	}
}

// 流したあとに残った書き込みの期限で、同じつなぎの次の返事が失敗しない
func TestStaleWriteDeadlineDoesNotBreakTheNextResponse(t *testing.T) {
	srv := newServer(testHandler, func(c *Ctx) {})
	srv.writeTimeout = 150 * time.Millisecond
	ln := listen(t)
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(200 * time.Millisecond) })
	c, br := dial(t, ln.Addr().String())
	for _, path := range []string{"/fixed", "/stream", "/x"} {
		io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: a\r\n\r\n")
		if resp, _ := readResp(t, br, "GET"); resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		// 期限を過ぎてから、同じつなぎで次を頼む
		time.Sleep(300 * time.Millisecond)
		io.WriteString(c, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n")
		if resp, body := readResp(t, br, "GET"); resp.StatusCode != 200 || string(body) != "ok\n" {
			t.Fatalf("after %s: %d %q", path, resp.StatusCode, body)
		}
	}
}

// 読まない客への小さな返事でも、書き込みに期限があるので、つなぎを手放す
func TestSmallResponseToANonReadingClientTimesOut(t *testing.T) {
	srv := newServer(testHandler, func(c *Ctx) {})
	srv.writeTimeout = 100 * time.Millisecond
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		srv.serveConn(server)
		close(done)
	}()
	if _, err := io.WriteString(client, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("connection is stuck writing to a client that does not read")
	}
}

// リクエストの読み取りは毎回通るので、手間を見ておく (Cloudflare が付けるものに近い頭書き)
func BenchmarkReadRequest(b *testing.B) {
	raw := []byte("GET /maps/overworld/tiles/0/x-3/5/5/z-3/9/0.prbm HTTP/1.1\r\nHost: 3d-map.torosaba.net\r\n" +
		"User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36\r\n" +
		"Accept: */*\r\nAccept-Encoding: gzip, br\r\nAccept-Language: ja,en-US;q=0.9,en;q=0.8\r\nCdn-Loop: cloudflare; loops=1\r\n" +
		"Cf-Connecting-Ip: 203.0.113.9\r\nCf-Ipcountry: JP\r\nCf-Ray: 8f0c1f2a3b4c5d6e-NRT\r\nCf-Visitor: {\"scheme\":\"https\"}\r\n" +
		"Referer: https://3d-map.torosaba.net/\r\nSec-Ch-Ua: \"Chromium\";v=\"130\", \"Not?A_Brand\";v=\"99\"\r\nSec-Ch-Ua-Mobile: ?0\r\n" +
		"Sec-Ch-Ua-Platform: \"Windows\"\r\nSec-Fetch-Dest: empty\r\nSec-Fetch-Mode: cors\r\nSec-Fetch-Site: same-origin\r\n" +
		"X-Forwarded-For: 203.0.113.9\r\nX-Forwarded-Proto: https\r\nConnection: keep-alive\r\n\r\n")
	r := bytes.NewReader(raw)
	cn := &conn{br: bufio.NewReaderSize(r, maxHeaderBytes)}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		r.Reset(raw)
		cn.br.Reset(r)
		if code, err := cn.readRequest(); err != nil {
			b.Fatal(code, err)
		}
	}
}

// ためたものを返す経路の手間 (読み取り、返事、1 往復ぶん)
func BenchmarkCachedHit(b *testing.B) {
	body := bytes.Repeat([]byte{7}, 8<<10)
	e := newEntity(entityInit{status: 200, typ: "image/png", body: body, etag: `"x"`, tier: tierFast})
	srv := newServer(func(c *Ctx) { sendEntity(c, e) }, func(*Ctx) {})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(100 * time.Millisecond)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReaderSize(c, 64<<10)
	req := []byte("GET /tile HTTP/1.1\r\nHost: map.torosaba.net\r\nAccept-Encoding: gzip, br\r\nCf-Connecting-Ip: 203.0.113.9\r\n\r\n")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Write(req); err != nil {
			b.Fatal(err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func TestLargeBodyIsDeliveredIntact(t *testing.T) {
	addr, _ := startTestServer(t)
	c, br := dial(t, addr)
	for i := 0; i < 3; i++ {
		io.WriteString(c, "GET /big HTTP/1.1\r\nHost: a\r\n\r\n")
		resp, body := readResp(t, br, "GET")
		if resp.StatusCode != 200 || !bytes.Equal(body, bigBody) {
			t.Fatalf("round %d: %d, %d bytes", i, resp.StatusCode, len(body))
		}
	}
}

func TestChunkedStream(t *testing.T) {
	addr, _ := startTestServer(t)
	c, br := dial(t, addr)
	io.WriteString(c, "GET /stream HTTP/1.1\r\nHost: a\r\n\r\nGET /x HTTP/1.1\r\nHost: a\r\n\r\n")
	resp, body := readResp(t, br, "GET")
	if len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
		t.Fatalf("not chunked: %+v", resp.TransferEncoding)
	}
	if want := "alphabravo-bravo" + strings.Repeat("c", 70000); string(body) != want {
		t.Fatalf("body mismatch: %d bytes", len(body))
	}
	// 終わりが正しく伝わるので、同じつなぎで次に進める
	if _, body = readResp(t, br, "GET"); string(body) != "ok\n" {
		t.Fatalf("after stream: %q", body)
	}
}

func TestStreamOverHTTP10ClosesToEndTheBody(t *testing.T) {
	addr, _ := startTestServer(t)
	c, br := dial(t, addr)
	io.WriteString(c, "GET /stream HTTP/1.0\r\n\r\n")
	resp, body := readResp(t, br, "GET")
	if len(resp.TransferEncoding) != 0 || !strings.HasPrefix(string(body), "alphabravo-bravo") || !closed(c, br) {
		t.Fatalf("HTTP/1.0 stream: te=%v len=%d", resp.TransferEncoding, len(body))
	}
}

func TestShutdownClosesIdleConnectionsAndStopsAccepting(t *testing.T) {
	addr, srv := startTestServer(t)
	c, br := dial(t, addr)
	io.WriteString(c, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n")
	readResp(t, br, "GET")
	done := make(chan struct{})
	go func() {
		srv.Shutdown(2 * time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("Shutdown did not return although every connection is idle")
	}
	if !closed(c, br) {
		t.Fatal("idle connection must be closed")
	}
	if nc, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		nc.Close()
		t.Fatal("server still accepts after Shutdown")
	}
}

func TestManyConcurrentConnections(t *testing.T) {
	addr, _ := startTestServer(t)
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		go func() {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			br := bufio.NewReader(c)
			for j := 0; j < 50; j++ {
				io.WriteString(c, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n")
				resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
				if err != nil {
					errs <- err
					return
				}
				body, _ := io.ReadAll(resp.Body)
				if string(body) != "ok\n" {
					errs <- io.ErrUnexpectedEOF
					return
				}
			}
			errs <- nil
		}()
	}
	for i := 0; i < 64; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

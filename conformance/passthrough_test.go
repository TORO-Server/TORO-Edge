package conformance

// 統一した入口 (auth / mcp / pass-api / api のパススルーと、assets ホストのタイル) の適合試験。
// 元のサーバーは試験の中の小さな HTTP サーバー (本番には一切つながない)。ホストは Host 頭書きで選ぶ。

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recReq struct {
	Method, URI, Host string
	Header            http.Header
	Body              []byte
	Chunked           bool
}

type rec struct {
	mu   sync.Mutex
	reqs []recReq
	h    http.HandlerFunc
	srv  *httptest.Server
}

func (r *rec) set(h http.HandlerFunc) {
	r.mu.Lock()
	r.h = h
	r.mu.Unlock()
}

func (r *rec) all() []recReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recReq(nil), r.reqs...)
}

func (r *rec) last(t *testing.T) recReq {
	t.Helper()
	a := r.all()
	if len(a) == 0 {
		t.Fatal("upstream saw no request")
	}
	return a[len(a)-1]
}

func newRec(t *testing.T, h http.HandlerFunc) *rec {
	t.Helper()
	r := &rec{h: h}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(q.Body, 8<<20))
		r.mu.Lock()
		r.reqs = append(r.reqs, recReq{Method: q.Method, URI: q.RequestURI, Host: q.Host, Header: q.Header.Clone(), Body: body, Chunked: len(q.TransferEncoding) > 0})
		r.mu.Unlock()
		// 本文は覚えたので、handler 側が読めるよう戻す
		q.Body = io.NopCloser(bytes.NewReader(body))
		r.mu.Lock()
		cur := r.h
		r.mu.Unlock()
		cur(w, q)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rec) port() int { return r.srv.Listener.Addr().(*net.TCPAddr).Port }

type passEnv struct {
	t                       *testing.T
	edge                    *edgeHandle
	api, auth, mcp, passapi *rec
	client                  *http.Client
}

func withPass(t *testing.T, fn func(e *passEnv)) {
	t.Helper()
	root := t.TempDir()
	dyn, blue := root+"/dynmap", root+"/bluemap"
	put(t, dyn, "index.html", []byte("<html></html>"))
	put(t, blue, "settings.json", []byte("{}"))
	ok200 := func(tag string) http.HandlerFunc {
		return func(w http.ResponseWriter, q *http.Request) { fmt.Fprintf(w, "%s %s", tag, q.RequestURI) }
	}
	e := &passEnv{t: t}
	e.api = newRec(t, ok200("api"))
	e.auth = newRec(t, ok200("auth"))
	e.mcp = newRec(t, ok200("mcp"))
	e.passapi = newRec(t, ok200("passapi"))
	live := newRec(t, ok200("live"))
	e.edge = startBinary(t, config{
		mapWeb: e.api.port(), blueMapLive: live.port(), apiBackend: e.api.port(),
		dynmapWeb: dyn, blueMapWeb: blue, cacheBytes: 64 << 20, maxEntityBytes: 8 << 20,
		extraEnv: []string{
			fmt.Sprintf("EDGE_AUTH_BACKEND=127.0.0.1:%d", e.auth.port()),
			fmt.Sprintf("EDGE_MCP_BACKEND=127.0.0.1:%d", e.mcp.port()),
			fmt.Sprintf("EDGE_PASSAPI_BACKEND=127.0.0.1:%d", e.passapi.port()),
		},
	})
	defer e.edge.stop()
	e.client = &http.Client{
		Timeout:       15 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	fn(e)
}

func (e *passEnv) do(method, host, path string, body io.Reader, hdr map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", e.edge.port, path), body)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s%s: %v", method, host, path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

// ホストごとの振り分け。Host、パス、クエリは客のまま元へ届く
func TestPassHostRouting(t *testing.T) {
	withPass(t, func(e *passEnv) {
		for _, c := range []struct {
			host string
			rec  *rec
			tag  string
		}{
			{"auth.torosaba.net", e.auth, "auth"},
			{"mcp.torosaba.net", e.mcp, "mcp"},
			{"pass-api.torosaba.net", e.passapi, "passapi"},
			{"api.torosaba.net", e.api, "api"},
		} {
			res, body := e.do("GET", c.host, "/x/y%20z?a=1&b=2", nil, nil)
			eq(t, res.StatusCode, 200, c.host)
			eq(t, string(body), c.tag+" /x/y%20z?a=1&b=2", c.host)
			q := c.rec.last(t)
			eq(t, q.Host, c.host, "Host is kept")
			eq(t, q.URI, "/x/y%20z?a=1&b=2", c.host)
			ok(t, res.Header.Get("Connection") == "close" || res.Close, "pass-through closes the connection: "+c.host)
		}
	})
}

// 認証: POST (JSON とフォーム、Content-Length と chunked)、Cookie、Authorization、Location と Set-Cookie の素通し
func TestPassAuthFlow(t *testing.T) {
	withPass(t, func(e *passEnv) {
		e.auth.set(func(w http.ResponseWriter, q *http.Request) {
			body, _ := io.ReadAll(q.Body)
			switch q.URL.Path {
			case "/.well-known/openid-configuration":
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"issuer":"https://auth.torosaba.net"}`)
			case "/login":
				http.SetCookie(w, &http.Cookie{Name: "__Host-toro_session", Value: "abc", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
				http.SetCookie(w, &http.Cookie{Name: "__Host-toro_login_nonce", Value: "n", Path: "/", Secure: true})
				w.Header().Set("Location", "https://discord.com/oauth2/authorize?client_id=1&state=s")
				w.WriteHeader(302)
			case "/token":
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				fmt.Fprintf(w, `{"len":%d,"ct":%q,"cookie":%q,"authz":%q}`, len(body), q.Header.Get("Content-Type"), q.Header.Get("Cookie"), q.Header.Get("Authorization"))
			default:
				w.WriteHeader(404)
			}
		})
		res, body := e.do("GET", "auth.torosaba.net", "/.well-known/openid-configuration", nil, nil)
		eq(t, res.StatusCode, 200, "discovery")
		eq(t, string(body), `{"issuer":"https://auth.torosaba.net"}`, "discovery body")
		eq(t, res.Header.Get("Content-Type"), "application/json", "discovery type")

		res, _ = e.do("GET", "auth.torosaba.net", "/login?next=%2Fhome", nil, nil)
		eq(t, res.StatusCode, 302, "login redirect is not followed")
		eq(t, res.Header.Get("Location"), "https://discord.com/oauth2/authorize?client_id=1&state=s", "Location untouched")
		cookies := res.Header.Values("Set-Cookie")
		eq(t, len(cookies), 2, "both Set-Cookie kept")
		ok(t, strings.Contains(cookies[0], "__Host-toro_session=abc") && strings.Contains(cookies[0], "Secure") && strings.Contains(cookies[0], "HttpOnly") && strings.Contains(cookies[0], "SameSite=Lax"), cookies[0])

		form := "grant_type=authorization_code&code=xyz&code_verifier=" + strings.Repeat("v", 64)
		res, body = e.do("POST", "auth.torosaba.net", "/token", strings.NewReader(form), map[string]string{
			"Content-Type": "application/x-www-form-urlencoded", "Cookie": "__Host-toro_session=abc; other=1", "Authorization": "Bearer tok123",
		})
		eq(t, res.StatusCode, 200, "token")
		eq(t, string(body), fmt.Sprintf(`{"len":%d,"ct":"application/x-www-form-urlencoded","cookie":"__Host-toro_session=abc; other=1","authz":"Bearer tok123"}`, len(form)), "form body, cookie and authorization arrive")
		eq(t, res.Header.Get("Cache-Control"), "no-store", "Cache-Control untouched")

		// chunked (Content-Length なし)
		pr, pw := io.Pipe()
		go func() {
			for i := 0; i < 3; i++ {
				io.WriteString(pw, strings.Repeat("a", 1000))
				time.Sleep(20 * time.Millisecond)
			}
			pw.Close()
		}()
		res, body = e.do("POST", "auth.torosaba.net", "/token", pr, map[string]string{"Content-Type": "application/json"})
		eq(t, res.StatusCode, 200, "chunked token")
		ok(t, strings.HasPrefix(string(body), `{"len":3000,`), string(body))
		q := e.auth.last(t)
		ok(t, q.Chunked, "chunked framing reaches upstream")

		// 大きめの本文 (1 MiB)
		big := strings.Repeat("z", 1<<20)
		res, body = e.do("POST", "auth.torosaba.net", "/token", strings.NewReader(big), map[string]string{"Content-Type": "text/plain"})
		ok(t, strings.HasPrefix(string(body), `{"len":1048576,`), string(body))
	})
}

// 内側の頭書き: 客が書いた X-Toro-Client-Ip は渡さず、cf-connecting-ip から作る。X-Forwarded-For と X-Forwarded-Proto は渡す
func TestPassClientHeaders(t *testing.T) {
	withPass(t, func(e *passEnv) {
		e.do("GET", "api.torosaba.net", "/v1/ping", nil, map[string]string{"X-Toro-Client-Ip": "6.6.6.6", "X-Forwarded-Proto": "https", "X-Forwarded-For": "203.0.113.9"})
		q := e.api.last(t)
		eq(t, q.Header.Get("X-Toro-Client-Ip"), "", "spoofed header removed")
		eq(t, q.Header.Get("X-Forwarded-Proto"), "https", "proto kept")
		ok(t, strings.HasPrefix(q.Header.Get("X-Forwarded-For"), "203.0.113.9, "), q.Header.Get("X-Forwarded-For"))
		e.do("GET", "api.torosaba.net", "/v1/ping", nil, map[string]string{"X-Toro-Client-Ip": "6.6.6.6", "Cf-Connecting-Ip": "198.51.100.7"})
		eq(t, e.api.last(t).Header.Get("X-Toro-Client-Ip"), "198.51.100.7", "rebuilt from cf-connecting-ip")
	})
}

// MCP: SSE は元が 1 つ目のイベントを出した時点で客に届く (ためない)。2 つ目は元が待っているあいだに届かなくてよい
func TestPassStreaming(t *testing.T) {
	withPass(t, func(e *passEnv) {
		release := make(chan struct{})
		e.mcp.set(func(w http.ResponseWriter, q *http.Request) {
			if q.Method == "POST" {
				io.Copy(io.Discard, q.Body)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(200)
			io.WriteString(w, "event: endpoint\ndata: /message\n\n")
			w.(http.Flusher).Flush()
			<-release
			io.WriteString(w, "event: ping\ndata: 2\n\n")
		})
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/sse", e.edge.port), nil)
		req.Host = "mcp.torosaba.net"
		req.Header.Set("Accept", "text/event-stream")
		res, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		eq(t, res.StatusCode, 200, "sse status")
		eq(t, res.Header.Get("Content-Type"), "text/event-stream", "sse type")
		br := bufio.NewReader(res.Body)
		got := make(chan string, 1)
		go func() {
			l1, _ := br.ReadString('\n')
			l2, _ := br.ReadString('\n')
			got <- l1 + l2
		}()
		select {
		case s := <-got:
			eq(t, s, "event: endpoint\ndata: /message\n", "first event arrives while the upstream is still open")
		case <-time.After(3 * time.Second):
			t.Fatal("first SSE event was buffered")
		}
		close(release)
		rest, _ := io.ReadAll(br)
		ok(t, strings.Contains(string(rest), "event: ping"), string(rest))

		res2, body := e.do("POST", "mcp.torosaba.net", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer t"})
		eq(t, res2.StatusCode, 200, "tools/list")
		ok(t, strings.Contains(string(body), `"tools"`), string(body))
		q := e.mcp.last(t)
		eq(t, string(q.Body), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "json-rpc body")
		eq(t, q.Header.Get("Authorization"), "Bearer t", "authorization")
	})
}

// WebSocket: api ホストの /map/ と /map3d/ 以外のパス、pass-api は、101 のあとを何も解かずに渡す
func TestPassWebSocket(t *testing.T) {
	withPass(t, func(e *passEnv) {
		echo := func(w http.ResponseWriter, q *http.Request) {
			if !strings.EqualFold(q.Header.Get("Upgrade"), "websocket") {
				w.WriteHeader(426)
				return
			}
			conn, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			io.WriteString(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: x\r\n\r\n")
			brw.Flush()
			buf := make([]byte, 64)
			for {
				n, err := brw.Read(buf)
				if n > 0 {
					conn.Write(append([]byte("echo:"), buf[:n]...))
				}
				if err != nil {
					return
				}
			}
		}
		e.api.set(echo)
		e.passapi.set(echo)
		for _, c := range []struct{ host, path string }{{"api.torosaba.net", "/live/ws"}, {"pass-api.torosaba.net", "/ws"}} {
			conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", e.edge.port))
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", c.path, c.host)
			br := bufio.NewReader(conn)
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			res, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("%s: %v", c.host, err)
			}
			eq(t, res.StatusCode, 101, c.host+" status")
			io.WriteString(conn, "hello")
			buf := make([]byte, 10)
			_, err = io.ReadFull(br, buf)
			if err != nil {
				t.Fatalf("%s: %v", c.host, err)
			}
			eq(t, string(buf), "echo:hello", c.host+" echo")
			conn.Close()
		}
	})
}

// api ホストは /map/ と /map3d/ を今までどおり edge が返し (キャッシュ、知らないパスは 404)、それ以外は全部 toro-api へ (メソッドも問わない)
func TestPassApiSplit(t *testing.T) {
	withPass(t, func(e *passEnv) {
		e.api.set(func(w http.ResponseWriter, q *http.Request) {
			if strings.HasPrefix(q.URL.Path, "/map/v1/tiles/") {
				w.Header().Set("Content-Type", "image/webp")
				w.Header().Set("Cache-Control", "public, max-age=300")
				w.Header().Set("Etag", `"1"`)
				io.WriteString(w, "TILE")
				return
			}
			fmt.Fprintf(w, "api %s %s", q.Method, q.RequestURI)
		})
		tile := "/map/v1/tiles/main/t/0_-1/13_-29.webp"
		for i := 0; i < 3; i++ {
			res, body := e.do("GET", "api.torosaba.net", tile, nil, nil)
			eq(t, res.StatusCode, 200, "tile")
			eq(t, string(body), "TILE", "tile body")
		}
		n := 0
		for _, q := range e.api.all() {
			if q.URI == tile {
				n++
			}
		}
		eq(t, n, 1, "tile is cached by the edge (upstream asked once)")
		before := len(e.api.all())
		res, _ := e.do("GET", "api.torosaba.net", "/map/v1/nothing-here", nil, nil)
		ok(t, res.StatusCode == 404 || len(e.api.all()) > before, "unknown /map/ path is answered by the edge rules")
		res, _ = e.do("POST", "api.torosaba.net", "/map/v1/tiles/x", strings.NewReader("a"), nil)
		eq(t, res.StatusCode, 400, "POST under /map/ is still refused by the edge (body)")

		res, body := e.do("POST", "api.torosaba.net", "/wiki/pages/a?x=1", strings.NewReader(`{"a":1}`), map[string]string{"Content-Type": "application/json"})
		eq(t, res.StatusCode, 200, "wiki POST")
		eq(t, string(body), "api POST /wiki/pages/a?x=1", "wiki POST goes to the api")
		res, body = e.do("DELETE", "api.torosaba.net", "/wiki/pages/a", nil, nil)
		eq(t, string(body), "api DELETE /wiki/pages/a", "DELETE")
		res, body = e.do("GET", "api.torosaba.net", "/", nil, nil)
		eq(t, string(body), "api GET /", "root goes to the api")
		res, body = e.do("GET", "api.torosaba.net", "/_assets/abcdefghijkl.png", nil, nil)
		eq(t, string(body), "api GET /_assets/abcdefghijkl.png", "other paths")
	})
}

// assets ホスト: ファイルは今までどおり /_assets/ へ、タイルは同じパスで toro-api へ (キャッシュあり、3D は cdn-cache-control)
func TestAssetsTiles(t *testing.T) {
	withPass(t, func(e *passEnv) {
		e.api.set(func(w http.ResponseWriter, q *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			switch {
			case strings.HasPrefix(q.URL.Path, "/map/v1/tiles/"):
				w.Header().Set("Content-Type", "image/webp")
				w.Header().Set("Cache-Control", "public, max-age=300")
				io.WriteString(w, "TILE2D")
			case strings.HasPrefix(q.URL.Path, "/map3d/v1/maps/") && strings.Contains(q.URL.Path, "/tiles/"):
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Cache-Control", "public, max-age=86400, stale-if-error=604800")
				io.WriteString(w, "TILE3D")
			case strings.HasPrefix(q.URL.Path, "/_assets/"):
				w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
				io.WriteString(w, "ASSET")
			default:
				io.WriteString(w, "OTHER")
			}
		})
		count := func(uri string) int {
			n := 0
			for _, q := range e.api.all() {
				if q.URI == uri {
					n++
				}
			}
			return n
		}
		// ファイル
		res, body := e.do("GET", "assets.torosaba.net", "/abcdefghijkl.png", nil, map[string]string{"Referer": "https://3d-map.torosaba.net/", "Cf-Connecting-Ip": "198.51.100.7"})
		eq(t, string(body), "ASSET", "file")
		eq(t, e.api.last(t).URI, "/_assets/abcdefghijkl.png", "file is rewritten to /_assets/")
		eq(t, e.api.last(t).Header.Get("X-Toro-Client-Ip"), "198.51.100.7", "client ip header for the file rule")
		eq(t, e.api.last(t).Header.Get("Referer"), "https://3d-map.torosaba.net/", "referer is forwarded for the file rule")

		// 2D
		t2d := "/map/v1/tiles/main/t/0_-1/13_-29.webp"
		for i := 0; i < 3; i++ {
			res, body = e.do("GET", "assets.torosaba.net", t2d, nil, nil)
			eq(t, res.StatusCode, 200, "2d tile")
			eq(t, string(body), "TILE2D", "2d body")
		}
		eq(t, count(t2d), 1, "2d tile is asked once (edge cache)")
		eq(t, res.Header.Get("Access-Control-Allow-Origin"), "*", "CORS header of the upstream is kept")
		_, _ = e.do("GET", "assets.torosaba.net", t2d+"?timestamp=1793723325379", nil, nil)
		eq(t, count(t2d+"?timestamp=1793723325379"), 1, "timestamp URL is a separate entry and is passed to the upstream")
		// api ホストの同じパスとキャッシュを共有する
		_, _ = e.do("GET", "api.torosaba.net", t2d, nil, nil)
		eq(t, count(t2d), 1, "api and assets share the cache entry")

		// 3D
		t3d := "/map3d/v1/maps/overworld/tiles/0/x0/z0.prbm"
		res, body = e.do("GET", "assets.torosaba.net", t3d, nil, nil)
		eq(t, res.StatusCode, 200, "3d tile")
		eq(t, string(body), "TILE3D", "3d body")
		eq(t, res.Header.Get("Cdn-Cache-Control"), "max-age=60", "3d tile gets cdn-cache-control")
		eq(t, res.Header.Get("Access-Control-Allow-Origin"), "*", "3d CORS")
		_, _ = e.do("GET", "assets.torosaba.net", "/map3d/v1/maps/overworld/tiles/1/x-3/7/0/z-3/9/3.png", nil, nil)

		// タイル以外は assets ホストでは 404 (元に聞かない)
		before := len(e.api.all())
		for _, p := range []string{
			"/map/v1/configuration", "/map/v1/world/main/0", "/map/v1/ws",
			"/map3d/v1/maps/overworld/settings.json", "/map3d/v1/maps/overworld/live/players.json", "/map3d/v1/marumasign/overworld.json",
			"/map3d/v1/maps/overworld/tiles/", "/map3d/v1/maps/../tiles/x", "/map3d/v1/maps//tiles/x",
			"/map/v1/tiles/../configuration", "/map/v1/tiles/%2e%2e/configuration", "/map/v1/tiles/.hidden",
			"/", "/wiki/pages/a", "/abcdefghijkl", "/ABCDEFGHIJKL.png",
		} {
			res, _ = e.do("GET", "assets.torosaba.net", p, nil, nil)
			eq(t, res.StatusCode, 404, "assets host refuses "+p)
		}
		eq(t, len(e.api.all()), before, "refused paths never reach the upstream")

		// preflight は 3D のタイルのパスだけ元へ
		e.api.set(func(w http.ResponseWriter, q *http.Request) {
			if q.Method == "OPTIONS" {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Headers", "Range")
				w.WriteHeader(204)
				return
			}
			io.WriteString(w, "x")
		})
		res, _ = e.do("OPTIONS", "assets.torosaba.net", t3d, nil, map[string]string{"Origin": "https://3d-map.torosaba.net", "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "range"})
		eq(t, res.StatusCode, 204, "3d tile preflight")
		eq(t, res.Header.Get("Access-Control-Allow-Headers"), "Range", "preflight header from the upstream")
		res, _ = e.do("OPTIONS", "assets.torosaba.net", "/abcdefghijkl.png", nil, map[string]string{"Origin": "https://3d-map.torosaba.net"})
		eq(t, res.StatusCode, 405, "preflight for the file rule is unchanged")
	})
}

// 元が決まっていないホストは受けない (map ホストと同じ扱い)。元が落ちているときは 502
func TestPassUnconfiguredAndDown(t *testing.T) {
	withPass(t, func(e *passEnv) {
		e.auth.srv.Close()
		res, body := e.do("GET", "auth.torosaba.net", "/x", nil, nil)
		eq(t, res.StatusCode, 502, "upstream down")
		eq(t, string(body), "Bad Gateway\n", "502 body")
	})
	root := t.TempDir()
	put(t, root+"/dynmap", "index.html", []byte("<html></html>"))
	put(t, root+"/bluemap", "settings.json", []byte("{}"))
	f := newRec(t, func(w http.ResponseWriter, q *http.Request) { io.WriteString(w, "up") })
	edge := startBinary(t, config{mapWeb: f.port(), blueMapLive: f.port(), apiBackend: f.port(), dynmapWeb: root + "/dynmap", blueMapWeb: root + "/bluemap", cacheBytes: 1 << 20, maxEntityBytes: 1 << 19})
	defer edge.stop()
	r, err := request(edge.port, "/x", reqOpts{host: "auth.torosaba.net"})
	if err != nil {
		t.Fatal(err)
	}
	ok(t, r.status == 404 || r.status == 405, fmt.Sprint("auth without a backend is not proxied: ", r.status))
	for _, q := range f.all() {
		if q.Host == "auth.torosaba.net" {
			t.Fatal("unconfigured host reached an upstream")
		}
	}
}

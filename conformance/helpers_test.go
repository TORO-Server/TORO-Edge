// 実装の言語によらない適合試験: 偽の元のサーバーと一時フォルダを用意して、実行ファイル (EDGE_BIN) を外から確かめる。
// 実装の中の単体試験は、ルートの *_test.go にある。
//
//	go build -trimpath -o toro-edge . && cd conformance && EDGE_BIN=../toro-edge go test -count=1 .
//
// EDGE_BIN を省くと、リポジトリの直下をビルドして使う。
package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// 試験の相手の実行ファイル。TestMain が決める
var edgeBin string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if v := os.Getenv("EDGE_BIN"); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		edgeBin = abs
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "edge-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	edgeBin = filepath.Join(dir, "toro-edge")
	if runtime.GOOS == "windows" {
		edgeBin += ".exe"
	}
	build := exec.Command("go", "build", "-trimpath", "-o", edgeBin, "..")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "go build failed:", err)
		return 1
	}
	return m.Run()
}

// ---- 小さな表明 (外の部品は使わない) ----

func eq[T comparable](t *testing.T, got, want T, msg ...any) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", fmt.Sprint(msg...), got, want)
	}
}

func ok(t *testing.T, cond bool, msg ...any) {
	t.Helper()
	if !cond {
		t.Fatalf("not ok: %s", fmt.Sprint(msg...))
	}
}

func sameBytes(t *testing.T, got, want []byte, msg ...any) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: bodies differ (got %d bytes, want %d bytes)", fmt.Sprint(msg...), len(got), len(want))
	}
}

// ---- 偽の元のサーバー ----

type seenReq struct {
	URL string
	// Host も入れる (Go の http.Request は Host を Header に入れないので)
	Header http.Header
}

// fake は元のサーバーの代わり。handler は試験ごとに差し替える
type fake struct {
	mu       sync.Mutex
	hits     map[string]int
	seen     []seenReq
	handler  http.HandlerFunc
	srv      *http.Server
	port     int
	hijacked []net.Conn
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(404)
}

func startFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{hits: map[string]int{}, handler: notFound}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.port = ln.Addr().(*net.TCPAddr).Port
	f.srv = &http.Server{Handler: http.HandlerFunc(f.serve)}
	go f.srv.Serve(ln)
	return f
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	h := r.Header.Clone()
	h.Set("Host", r.Host)
	f.mu.Lock()
	f.hits[r.RequestURI]++
	f.seen = append(f.seen, seenReq{URL: r.RequestURI, Header: h})
	handler := f.handler
	f.mu.Unlock()
	handler(w, r)
}

func (f *fake) setHandler(h http.HandlerFunc) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

func (f *fake) hitCount(url string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[url]
}

func (f *fake) seenLen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

// seenAt は i 番目に来たリクエスト (負なら後ろから)
func (f *fake) seenAt(i int) seenReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 {
		i += len(f.seen)
	}
	return f.seen[i]
}

func (f *fake) hijack(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter) {
	c, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(err)
	}
	f.mu.Lock()
	f.hijacked = append(f.hijacked, c)
	f.mu.Unlock()
	return c, rw
}

// stop は聞くのをやめ、つながっているものも (Upgrade したものも) 切る。何度呼んでもよい
func (f *fake) stop() {
	f.srv.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.hijacked {
		c.Close()
	}
	f.hijacked = nil
}

// send は、頭書きを付けて、本文を返す (Node の res.writeHead + res.end と同じ。長さを付ける)
func send(w http.ResponseWriter, status int, headers map[string]string, body []byte) {
	h := w.Header()
	for k, v := range headers {
		h.Set(k, v)
	}
	if h.Get("Content-Length") == "" && status != 204 && status != 304 {
		h.Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(status)
	if len(body) > 0 {
		w.Write(body)
	}
}

func sendText(w http.ResponseWriter, status int, headers map[string]string, body string) {
	send(w, status, headers, []byte(body))
}

// destroy は、返事をせずに、つなぎを切る (元が落ちたふり)
func (f *fake) destroy(w http.ResponseWriter) {
	c, _ := f.hijack(w)
	c.Close()
}

// echoUpgrade は、WebSocket の上流のふり (101 を返し、届いたものをそのまま返す)
func (f *fake) echoUpgrade(w http.ResponseWriter, _ *http.Request) {
	c, rw := f.hijack(w)
	c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: test\r\n\r\n"))
	go func() {
		defer c.Close()
		io.Copy(c, rw)
	}()
}

// ---- 客 ----

type reply struct {
	status int
	header http.Header
	body   []byte
}

// h は頭書きの値 (なければ空)
func (r *reply) h(name string) string { return r.header.Get(name) }

// has は、その頭書きがあるか
func (r *reply) has(name string) bool { return len(r.header.Values(name)) > 0 }

type reqOpts struct {
	host    string
	method  string
	headers map[string]string
}

func buildRequest(path string, o reqOpts, keepAlive bool) (method string, raw []byte) {
	method = o.method
	if method == "" {
		method = "GET"
	}
	host := o.host
	if host == "" {
		host = "map.torosaba.net"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: %s\r\n", method, path, host)
	if !keepAlive {
		b.WriteString("Connection: close\r\n")
	}
	for k, v := range o.headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if method == "POST" || method == "PUT" {
		b.WriteString("Content-Length: 0\r\n")
	}
	b.WriteString("\r\n")
	return method, []byte(b.String())
}

// request は 1 回ごとに新しくつないで、素の HTTP/1.1 で取る (パスは 1 バイトも変えない。本文も勝手に展開しない)
func request(port int, path string, o reqOpts) (*reply, error) {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	method, raw := buildRequest(path, o, false)
	if _, err := conn.Write(raw); err != nil {
		return nil, err
	}
	return readReply(bufio.NewReaderSize(conn, 64<<10), method)
}

func readReply(br *bufio.Reader, method string) (*reply, error) {
	res, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return &reply{status: res.StatusCode, header: res.Header, body: body}, nil
}

// ---- edge の起動 ----

type config struct {
	mapWeb, blueMapLive int
	dynmapWeb           string
	blueMapWeb          string
	cacheBytes          int
	maxEntityBytes      int
	// nil は実装の既定。0 なら、大きい本文も write で送る
	sendfileMin *int
}

type edgeHandle struct {
	port, adminPort int
	stop            func()
}

var listening = regexp.MustCompile(`listening on :(\d+) \(admin :(\d+)\)`)

func startBinary(t *testing.T, c config) *edgeHandle {
	t.Helper()
	env := []string{
		"EDGE_PORT=0",
		"EDGE_ADMIN_PORT=0",
		fmt.Sprintf("EDGE_MAPWEB=127.0.0.1:%d", c.mapWeb),
		fmt.Sprintf("EDGE_BLUEMAP_LIVE=127.0.0.1:%d", c.blueMapLive),
		"EDGE_DYNMAP_WEB=" + c.dynmapWeb,
		"EDGE_BLUEMAP_WEB=" + c.blueMapWeb,
		"EDGE_ACCESS_LOG=0",
		"EDGE_DEBUG_HEADERS=1",
		"EDGE_CACHE_BYTES=" + strconv.Itoa(c.cacheBytes),
		"EDGE_MAX_ENTITY_BYTES=" + strconv.Itoa(c.maxEntityBytes),
		"EDGE_SHUTDOWN_GRACE_MS=200",
	}
	if c.sendfileMin != nil {
		env = append(env, "EDGE_SENDFILE_MIN="+strconv.Itoa(*c.sendfileMin))
	}
	// 外の EDGE_* は渡さない (試験の設定を乱さないため)。それ以外 (PATH、Windows の SYSTEMROOT など) は渡す
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "EDGE_") {
			env = append(env, kv)
		}
	}
	cmd := exec.Command(edgeBin)
	cmd.Env = env
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	found := make(chan [2]int, 1)
	go func() {
		var seen strings.Builder
		buf := make([]byte, 4096)
		sent := false
		for {
			n, err := out.Read(buf)
			if n > 0 && !sent {
				seen.Write(buf[:n])
				if m := listening.FindStringSubmatch(seen.String()); m != nil {
					p, _ := strconv.Atoi(m[1])
					a, _ := strconv.Atoi(m[2])
					found <- [2]int{p, a}
					sent = true
				}
			}
			if err != nil {
				return
			}
		}
	}()

	h := &edgeHandle{}
	var once sync.Once
	h.stop = func() {
		once.Do(func() {
			if runtime.GOOS == "windows" {
				cmd.Process.Kill()
			} else {
				cmd.Process.Signal(syscall.SIGTERM)
			}
			select {
			case <-exited:
			case <-time.After(3 * time.Second):
				cmd.Process.Kill()
				<-exited
			}
		})
	}
	select {
	case p := <-found:
		h.port, h.adminPort = p[0], p[1]
	case <-exited:
		t.Fatalf("%s exited early", edgeBin)
	case <-time.After(10 * time.Second):
		h.stop()
		t.Fatalf("%s did not start", edgeBin)
	}
	return h
}

// ---- 試験の環境 ----

type env struct {
	t               *testing.T
	edge            *edgeHandle
	mapweb, live    *fake
	dynDir, blueDir string
}

const (
	settingsJSON = `{"version":"5.x","scripts":["a.js"],"maps":["overworld","other"]}`
	indexHTML    = "<html><head>\n\t<script type=\"text/javascript\" src=\"standalone/config.js\"></script>\n</head></html>\n"
)

var css = strings.Repeat(".dynmap { color: red; margin: 0 auto; }\n", 120)

type overrides struct {
	cacheBytes, maxEntityBytes int
	sendfileMin                *int
}

func put(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// withEnv は、試験用のフォルダ・元のサーバー・edge をひとまとめに用意して、終わったら片づける
func withEnv(t *testing.T, o overrides, fn func(e *env)) {
	t.Helper()
	root := t.TempDir()
	dynDir := filepath.Join(root, "dynmap")
	blueDir := filepath.Join(root, "bluemap")
	put(t, dynDir, "index.html", []byte(indexHTML))
	put(t, dynDir, "css/style.css", []byte(css))
	put(t, dynDir, "images/blank.png", []byte{0x89, 0x50, 0x4E, 0x47, 1, 2, 3, 4})
	put(t, dynDir, "standalone/config.js", []byte("var config = {}"))
	put(t, dynDir, "standalone/MySQL_config.php", []byte(`<?php $password = "secret"; ?>`))
	put(t, dynDir, ".secret", []byte("hidden"))
	put(t, root, "outside.txt", []byte("outside the root"))
	put(t, blueDir, "index.html", []byte("<html>old bluemap</html>"))
	put(t, blueDir, "settings.json", []byte(settingsJSON))
	put(t, blueDir, "lang/settings.conf", []byte("default: \"en\"\nother: \"en\"\n"))
	put(t, blueDir, "marumasign/overworld.json", []byte(`{"signs":[]}`))
	put(t, blueDir, "assets/a.png", []byte{1, 2, 3})
	put(t, blueDir, "maps/hidden.json", []byte(`{"old":true}`))

	mapweb := startFake(t)
	live := startFake(t)
	cacheBytes, maxEntity := o.cacheBytes, o.maxEntityBytes
	if cacheBytes == 0 {
		cacheBytes = 64 << 20
	}
	if maxEntity == 0 {
		maxEntity = 8 << 20
	}
	edge := startBinary(t, config{
		mapWeb: mapweb.port, blueMapLive: live.port,
		dynmapWeb: dynDir, blueMapWeb: blueDir,
		cacheBytes: cacheBytes, maxEntityBytes: maxEntity, sendfileMin: o.sendfileMin,
	})
	defer func() {
		edge.stop()
		mapweb.stop()
		live.stop()
	}()
	fn(&env{t: t, edge: edge, mapweb: mapweb, live: live, dynDir: dynDir, blueDir: blueDir})
}

func (e *env) req(port int, host, path string, o []reqOpts) *reply {
	e.t.Helper()
	var opts reqOpts
	if len(o) > 0 {
		opts = o[0]
	}
	if opts.host == "" {
		opts.host = host
	}
	r, err := request(port, path, opts)
	if err != nil {
		e.t.Fatalf("GET %s (%s): %v", path, opts.host, err)
	}
	return r
}

// get は map.torosaba.net (Dynmap) へ
func (e *env) get(path string, o ...reqOpts) *reply {
	e.t.Helper()
	return e.req(e.edge.port, "map.torosaba.net", path, o)
}

// blue は 3d-map.torosaba.net (BlueMap) へ
func (e *env) blue(path string, o ...reqOpts) *reply {
	e.t.Helper()
	return e.req(e.edge.port, "3d-map.torosaba.net", path, o)
}

// assets は assets.torosaba.net へ
func (e *env) assets(path string, o ...reqOpts) *reply {
	e.t.Helper()
	return e.req(e.edge.port, "assets.torosaba.net", path, o)
}

func (e *env) admin(path string, method string) *reply {
	e.t.Helper()
	r, err := request(e.edge.adminPort, path, reqOpts{method: method})
	if err != nil {
		e.t.Fatalf("admin %s: %v", path, err)
	}
	return r
}

// cacheStats は、キャッシュにあるものの数と大きさ (管理用のポートの /stats)
func (e *env) cacheStats() (entries, size int) {
	e.t.Helper()
	var s struct {
		Cache struct {
			Entries int `json:"entries"`
			Bytes   int `json:"bytes"`
		} `json:"cache"`
	}
	if err := json.Unmarshal(e.admin("/stats", "GET").body, &s); err != nil {
		e.t.Fatal(err)
	}
	return s.Cache.Entries, s.Cache.Bytes
}

// sequence は、1 本のつなぎ (keep-alive) で順に取る。途中で切れたら失敗
func sequence(t *testing.T, port int, host string, items []seqItem) []*reply {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	br := bufio.NewReaderSize(conn, 64<<10)
	var replies []*reply
	for i, it := range items {
		method, raw := buildRequest(it.path, reqOpts{host: host, method: it.method, headers: it.headers}, true)
		if _, err := conn.Write(raw); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		r, err := readReply(br, method)
		if err != nil {
			t.Fatalf("request %d (%s %s): %v", i, method, it.path, err)
		}
		replies = append(replies, r)
	}
	return replies
}

type seqItem struct {
	path    string
	method  string
	headers map[string]string
}

package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strings"
	"time"
)

// Edge は自前のリバースプロキシ
type Edge struct {
	cfg       Config
	cache     *EntityCache
	sites     *Sites
	mapWeb    *Upstream
	live      *Upstream
	log       *AccessLog
	out       io.Writer
	srv       *Server
	admin     *http.Server
	stop      chan struct{}
	Port      int
	AdminPort int
}

func startEdge(cfg Config, out io.Writer) (*Edge, error) {
	debugHeaders = cfg.DebugHeaders
	sendfileMin = cfg.SendfileMin
	e := &Edge{
		cfg:    cfg,
		cache:  newEntityCache(cfg.CacheBytes),
		mapWeb: newUpstream("map-web", cfg.MapWeb, 256, 3*time.Second),
		live:   newUpstream("bluemap-live", cfg.BlueMapLive, 32, 2*time.Second),
		out:    out,
		log:    newAccessLog(out, cfg.AccessLog),
		stop:   make(chan struct{}),
	}
	dyn := newStaticSite(cfg.DynmapWeb)
	blue := newStaticSite(cfg.BlueMapWeb, "maps")
	e.sites = buildSites(SiteDeps{config: cfg, cache: e.cache, mapWeb: e.mapWeb, live: e.live, dynmapStatic: dyn, blueMapStatic: blue})

	// 静的なファイルは、起動してすぐ返せるよう裏で読んでおく (読み終わるまでは、そのつどディスクから読む)
	for _, p := range e.sites.preload {
		go func(p preloadJob) {
			p.site.preload(p.rulesFor)
			p.prime()
			fmt.Fprintf(out, "preload %s: %d files\n", p.site.root, p.site.size())
		}(p)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return nil, err
	}
	aln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.AdminPort))
	if err != nil {
		ln.Close()
		return nil, err
	}
	e.Port = ln.Addr().(*net.TCPAddr).Port
	e.AdminPort = aln.Addr().(*net.TCPAddr).Port

	e.srv = newServer(e.handle, e.done)
	mux := http.NewServeMux()
	mux.HandleFunc("/", e.serveAdmin)
	if os.Getenv("EDGE_PPROF") == "1" {
		mux.HandleFunc("/debug/pprof/", adminOnly(pprof.Index))
		mux.HandleFunc("/debug/pprof/profile", adminOnly(pprof.Profile))
		mux.HandleFunc("/debug/pprof/symbol", adminOnly(pprof.Symbol))
		mux.HandleFunc("/debug/pprof/trace", adminOnly(pprof.Trace))
	}
	e.admin = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go e.srv.Serve(ln)
	go e.admin.Serve(aln)
	go e.cache.sweepLoop(e.stop)
	return e, nil
}

func (e *Edge) siteFor(r *Request) *Site {
	if s, ok := e.sites.byHost[hostName(r.Host)]; ok {
		return s
	}
	return e.sites.fallback
}

// handle は経路を選んで、返す
func (e *Edge) handle(c *Ctx) {
	req := c.req
	cn := c.conn
	// 本文があるリクエストは受けない (読み飛ばさず、つなぎごと閉じる)
	if req.hasBody {
		cn.closeAfter = true
		sendText(c, http.StatusBadRequest, "Bad Request\n")
		return
	}
	if len(req.Target) > 8192 {
		sendText(c, http.StatusRequestURITooLong, "URI Too Long\n")
		return
	}
	site := e.siteFor(req)
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(e.out, "error %s %s: %v\n", c.method, c.path, rec)
			if c.status == 0 {
				sendText(c, http.StatusInternalServerError, "Internal Server Error\n")
			} else {
				cn.closeAfter = true
			}
		}
	}()
	if req.upgrade {
		// WebSocket (map.torosaba.net と 3d-map.torosaba.net の /up/ws だけ): つないだあとは何も解かずにそのまま渡す
		if e.srv.closing.Load() || (site.name != "map" && site.name != "bluemap") || c.path != "/up/ws" {
			notFound(c)
			return
		}
		proxyUpgrade(c, e.cfg.MapWeb, "/up/ws"+c.search)
		return
	}
	if c.method != "GET" && c.method != "HEAD" {
		sendText(c, http.StatusMethodNotAllowed, "Method Not Allowed\n", "Allow", "GET, HEAD")
		return
	}
	h := site.route(c.path, c)
	if h == nil {
		notFound(c)
		return
	}
	h(c)
}

// done は返し終えたあと: 数とログ
func (e *Edge) done(c *Ctx) {
	id := c.conn.id
	stats.requests.add(id, 1)
	countStatus(id, c.status)
	stats.bytesOut.add(id, c.bytes)
	e.log.add(c, c.status)
}

func rssBytes() uint64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	var size, resident uint64
	fmt.Sscanf(string(b), "%d %d", &size, &resident)
	return resident * uint64(os.Getpagesize())
}

// adminAllowed は管理用の合言葉 (EDGE_ADMIN_TOKEN。空なら確かめない) を、長さも時間で漏らさないよう SHA-256 にしてから比べる
func adminAllowed(r *http.Request) bool {
	token := os.Getenv("EDGE_ADMIN_TOKEN")
	if token == "" {
		return true
	}
	want := sha256.Sum256([]byte(token))
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	a := sha256.Sum256([]byte(bearer))
	b := sha256.Sum256([]byte(r.Header.Get("X-Admin-Token")))
	byBearer := subtle.ConstantTimeCompare(a[:], want[:]) == 1 && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
	byHeader := subtle.ConstantTimeCompare(b[:], want[:]) == 1
	return byBearer || byHeader
}

// adminOnly は合言葉を確かめてから渡す (pprof)
func adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !adminAllowed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// serveAdmin は管理用 (コンテナの中の 127.0.0.1 だけ): 生きているか、数、キャッシュを空にする
func (e *Edge) serveAdmin(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	closing := e.srv.closing.Load()
	reply := func(status int, v any) {
		body, _ := json.MarshalIndent(v, "", " ")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		w.Write(body)
	}
	if path != "/healthz" && !adminAllowed(r) {
		reply(401, map[string]string{"error": "unauthorized"})
		return
	}
	switch {
	case path == "/healthz":
		status := 200
		if closing {
			status = 503
		}
		reply(status, map[string]bool{"ok": !closing})
	case path == "/stats":
		entries, bytes, evictions := e.cache.stats()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		reply(200, map[string]any{
			"uptimeSeconds": int64(time.Since(startedAt).Seconds()), "startedAt": startedAt.UnixMilli(),
			"requests": stats.requests.load(), "s2xx": stats.s2xx.load(), "s3xx": stats.s3xx.load(), "s4xx": stats.s4xx.load(), "s5xx": stats.s5xx.load(),
			"bytesOut": stats.bytesOut.load(), "hit": stats.hit.load(), "miss": stats.miss.Load(), "stale": stats.stale.Load(),
			"revalidated": stats.revalidated.Load(), "coalesced": stats.coalesced.Load(), "bypass": stats.bypass.Load(), "errorStale": stats.errorStale.Load(),
			"upstreamRequests": stats.upstreamRequests.Load(), "upstreamErrors": stats.upstreamErrors.Load(), "fallbacks": stats.fallbacks.Load(),
			"wsOpen": stats.wsOpen.Load(), "wsTotal": stats.wsTotal.Load(),
			"cache":  map[string]any{"entries": entries, "bytes": bytes, "closingBytes": e.cache.closing.Load(), "limit": e.cfg.CacheBytes, "evictions": evictions},
			"memory": map[string]any{"rss": rssBytes(), "heapUsed": ms.HeapAlloc, "heapSys": ms.HeapSys, "goroutines": runtime.NumGoroutine()},
		})
	case path == "/purge" && r.Method == http.MethodPost:
		e.cache.clear()
		reply(200, map[string]bool{"ok": true})
	default:
		reply(404, map[string]string{"error": "not found"})
	}
}

// close は止める: 新しいつなぎを受けず、返し終えていないものを grace だけ待つ
func (e *Edge) close(grace time.Duration) {
	// つながっている WebSocket は待たれないので、猶予のあとで閉じる
	timer := time.AfterFunc(grace, closeAllSockets)
	defer timer.Stop()
	close(e.stop)
	e.srv.Shutdown(grace)
	closeAllSockets()
	e.admin.Close()
	e.mapWeb.close()
	e.live.close()
	e.log.flush()
}

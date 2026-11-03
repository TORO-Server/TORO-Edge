package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
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
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	e.admin = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go e.srv.Serve(ln)
	go e.admin.Serve(aln)
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

// serveAdmin は管理用 (Docker の中だけ): 生きているか、数、キャッシュを空にする
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
	if path != "/healthz" {
		if token := os.Getenv("EDGE_ADMIN_TOKEN"); token != "" {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+token && r.Header.Get("X-Admin-Token") != token {
				reply(401, map[string]string{"error": "unauthorized"})
				return
			}
		}
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
			"cache":  map[string]any{"entries": entries, "bytes": bytes, "limit": e.cfg.CacheBytes, "evictions": evictions},
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
	e.srv.Shutdown(grace)
	closeAllSockets()
	e.admin.Close()
	e.mapWeb.close()
	e.live.close()
	e.log.flush()
}

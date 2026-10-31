package main

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CachedOptions は toro-map-web などの返事をメモリにためて返す決まり
type CachedOptions struct {
	upstream *Upstream
	// ためる場所 (名前の空間)
	cache *nsCache
	// キャッシュの鍵。省略は パス + クエリ (クエリがなければ、パスそのまま)
	key func(c *Ctx) string
	// 元に聞くパスとクエリ (省略は ctx のもの)
	path func(c *Ctx) (string, string)
	// 元の Cache-Control を無視して、この長さ (ms) だけ新しいとする。0 なら元に従う
	fixedTtl int64
	// 元が何も言わないときの長さ (ms)
	defaultTtl int64
	// 元の max-age の上限 (ms)。0 なら上限なし
	maxTtl int64
	// 古くなったあと、裏で確かめるあいだ、古いまま返してよい長さ (ms)
	swr int64
	// 元が壊れているとき、古いまま返してよい長さ (ms)
	errorTtl int64
	// 404 を覚えておく長さ (ms)。0 なら覚えない
	negativeTtl   int64
	response      ResponseRules
	tier          Tier
	headerTimeout time.Duration
	// これより大きい返事は、ためずに流す (バイト)
	maxEntityBytes int64
	// 元がつながらないとき・5xx を返したときで、古いものもないときの代わり
	fallback Handler
	log      bool
}

type fetched struct {
	entity *Entity
	failed bool
	bypass bool
}

type flight struct {
	done chan struct{}
	res  fetched
	err  error
}

var notStoredHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "transfer-encoding": true, "te": true, "trailer": true, "upgrade": true, "content-length": true,
	"content-encoding": true, "content-type": true, "etag": true, "last-modified": true, "date": true, "server": true, "x-powered-by": true,
	"vary": true, "accept-ranges": true, "content-range": true, "age": true, "content-md5": true, "set-cookie": true,
}

// cachedHandler は元の返事をメモリにためて返す (同じものを同時に聞かない、古いものは裏で確かめる、元が壊れていても古いものを返す)
func cachedHandler(o CachedOptions) Handler {
	var mu sync.Mutex
	inflight := map[string]*flight{}
	headerTimeout := o.headerTimeout
	if headerTimeout == 0 {
		headerTimeout = 30 * time.Second
	}
	keyOf := o.key
	if keyOf == nil {
		keyOf = func(c *Ctx) string {
			if c.search == "" {
				return c.path
			}
			return c.path + c.search
		}
	}
	upstreamPath := func(c *Ctx) (string, string) {
		if o.path != nil {
			return o.path(c)
		}
		return c.path, strings.TrimPrefix(c.search, "?")
	}

	fetchOnce := func(path, query string, base map[string]string, key string, stale *Entity) (fetched, error) {
		if stale != nil {
			if stale.etag != "" {
				base["If-None-Match"] = stale.etag
			} else if stale.lastModified != "" {
				base["If-Modified-Since"] = stale.lastModified
			}
		}
		// 先頭の客が切れても、待っている客みんなのために最後まで聞く
		ctx, cancelAll := context.WithTimeout(context.Background(), headerTimeout+60*time.Second)
		defer cancelAll()
		resp, cancel, err := o.upstream.do(ctx, UpstreamRequest{method: "GET", path: path, query: query, headers: base, headerTimeout: headerTimeout})
		if err != nil {
			return fetched{}, err
		}
		defer cancel()
		defer resp.Body.Close()
		status := resp.StatusCode
		now := nowMs()
		if status == http.StatusNotModified && stale != nil {
			io.Copy(io.Discard, resp.Body)
			setFreshness(stale, freshness(200, resp.Header, &o), now, &o)
			return fetched{entity: stale}, nil
		}
		// 終わらない返事 (SSE) は、ためようとしない。呼んだ客みんなに、そのまま流す
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			return fetched{bypass: true}, nil
		}
		if resp.ContentLength > o.maxEntityBytes {
			return fetched{bypass: true}, nil
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, o.maxEntityBytes+1))
		if err != nil {
			return fetched{}, err
		}
		if int64(len(body)) > o.maxEntityBytes {
			return fetched{bypass: true}, nil
		}
		entity := buildEntity(status, resp.Header, body, &o)
		f := freshness(status, resp.Header, &o)
		if f.store && (f.ttl > 0 || entity.etag != "") {
			setFreshness(entity, f, now, &o)
			entity.attachFiles()
			o.cache.set(key, entity, now)
		}
		return fetched{entity: entity, failed: status >= 500}, nil
	}

	start := func(key string, c *Ctx, stale *Entity) (*flight, bool) {
		mu.Lock()
		if f, ok := inflight[key]; ok {
			mu.Unlock()
			return f, true
		}
		f := &flight{done: make(chan struct{})}
		inflight[key] = f
		mu.Unlock()
		path, query := upstreamPath(c)
		base := baseUpstreamHeaders(c.req)
		go func() {
			f.res, f.err = fetchOnce(path, query, base, key, stale)
			mu.Lock()
			delete(inflight, key)
			mu.Unlock()
			close(f.done)
		}()
		return f, false
	}

	refreshInBackground := func(key string, c *Ctx, e *Entity) {
		f, _ := start(key, c, e)
		go func() {
			<-f.done
			if f.err != nil || f.res.bypass || f.res.failed {
				e.nextRefresh.Store(nowMs() + 1000)
			}
		}()
	}

	return func(c *Ctx) {
		c.log = o.log
		key := keyOf(c)
		now := nowMs()
		e := o.cache.get(key, now)
		if e != nil {
			if now < e.freshUntil.Load() {
				c.cache = "HIT"
				stats.hit.add(c.conn.id, 1)
				sendEntity(c, e)
				return
			}
			if now < e.staleUntil.Load() {
				c.cache = "STALE"
				stats.stale.Add(1)
				if now >= e.nextRefresh.Load() {
					refreshInBackground(key, c, e)
				}
				sendEntity(c, e)
				return
			}
			// 元が壊れていると分かったばかりなら、聞かずに古いものを返す
			if now < e.nextRefresh.Load() && now < e.errorUntil.Load() {
				c.cache = "ERROR-STALE"
				stats.errorStale.Add(1)
				sendEntity(c, e)
				return
			}
		}
		f, joined := start(key, c, e)
		<-f.done
		r, err := f.res, f.err
		now = nowMs()
		if err == nil && r.bypass {
			c.cache = "BYPASS"
			stats.bypass.Add(1)
			bypass(c, &o, upstreamPath)
			return
		}
		if err == nil && !r.failed {
			switch {
			case joined:
				c.cache = "COALESCED"
				stats.coalesced.Add(1)
			case e != nil:
				c.cache = "REVALIDATED"
				stats.revalidated.Add(1)
			default:
				c.cache = "MISS"
				stats.miss.Add(1)
			}
			sendEntity(c, r.entity)
			return
		}
		// 元に聞けなかった (つながらない、時間切れ、5xx)
		if e != nil && now < e.errorUntil.Load() {
			e.nextRefresh.Store(now + 1000)
			c.cache = "ERROR-STALE"
			stats.errorStale.Add(1)
			sendEntity(c, e)
			return
		}
		if o.fallback != nil {
			c.cache = "FALLBACK"
			stats.fallbacks.Add(1)
			o.fallback(c)
			return
		}
		if err == nil {
			c.cache = "MISS"
			sendEntity(c, r.entity)
			return
		}
		badGateway(c)
	}
}

// bypass は大きすぎてためられないものを、そのまま流す
func bypass(c *Ctx, o *CachedOptions, upstreamPath func(*Ctx) (string, string)) {
	headers := baseUpstreamHeaders(c.req)
	for _, id := range []hid{hRange, hIfRange, hIfNoneMatch, hIfModifiedSince} {
		if v := c.req.h[id]; v != "" {
			headers[hdrNames[id]] = v
		}
	}
	p, q := upstreamPath(c)
	ht := o.headerTimeout
	if ht == 0 {
		ht = 30 * time.Second
	}
	relayRequest(c, o.upstream, UpstreamRequest{method: c.method, path: p, query: q, headers: headers, headerTimeout: ht}, 300*time.Second, &o.response)
}

type fresh struct {
	store bool
	ttl   int64
}

func freshness(status int, h http.Header, o *CachedOptions) fresh {
	if status >= 500 {
		return fresh{}
	}
	if status != 200 && status != 204 {
		if status == 404 && o.negativeTtl > 0 {
			return fresh{true, o.negativeTtl}
		}
		return fresh{}
	}
	if o.fixedTtl > 0 {
		return fresh{true, o.fixedTtl}
	}
	cc := parseCacheControl(h.Get("Cache-Control"))
	if cc.noStore || cc.isPrivate {
		return fresh{}
	}
	age := cc.sMaxAge
	if age < 0 {
		age = cc.maxAge
	}
	var ttl int64
	switch {
	case cc.noCache:
		ttl = 0
	case age >= 0:
		ttl = age * 1000
	default:
		ttl = o.defaultTtl
	}
	if o.maxTtl > 0 && ttl > o.maxTtl {
		ttl = o.maxTtl
	}
	return fresh{true, ttl}
}

func setFreshness(e *Entity, f fresh, now int64, o *CachedOptions) {
	if !f.store {
		return
	}
	fu := now + f.ttl
	e.freshUntil.Store(fu)
	// 毎回確かめさせるもの (ttl 0) は、古いまま返さない
	su := fu
	if f.ttl > 0 {
		su += o.swr
	}
	e.staleUntil.Store(su)
	e.errorUntil.Store(fu + o.errorTtl)
	e.nextRefresh.Store(0)
}

func buildEntity(status int, h http.Header, body []byte, o *CachedOptions) *Entity {
	headers := map[string]string{}
	for k, vals := range h {
		lk := strings.ToLower(k)
		if notStoredHeaders[lk] || len(vals) == 0 {
			continue
		}
		headers[lk] = vals[0]
	}
	for _, k := range o.response.hide {
		delete(headers, k)
	}
	// エラーの返事には、付け足さない (nginx の add_header は成功の返事にだけ付く)
	if status < 400 {
		if o.response.cacheControl != "" && o.response.cacheControl != "upstream" {
			headers["cache-control"] = o.response.cacheControl
		}
		for _, kv := range o.response.set {
			headers[kv[0]] = kv[1]
		}
	}
	flat := make([]hdr, 0, len(headers))
	for k, v := range headers {
		flat = append(flat, mkHdr(k, v))
	}
	hasBody := status != 204 && status != 304
	typ := ""
	if hasBody {
		typ = h.Get("Content-Type")
		if typ == "" {
			typ = "application/octet-stream"
		}
	}
	etag := h.Get("Etag")
	if etag == "" && status == 200 && len(body) > 0 {
		sum := sha1.Sum(body)
		etag = `"` + base64.RawURLEncoding.EncodeToString(sum[:])[:22] + `"`
	}
	encoding := h.Get("Content-Encoding")
	if encoding == "identity" {
		encoding = ""
	}
	return newEntity(entityInit{
		status:       status,
		typ:          typ,
		body:         body,
		encoding:     encoding,
		etag:         etag,
		lastModified: h.Get("Last-Modified"),
		headers:      flat,
		tier:         o.tier,
	})
}

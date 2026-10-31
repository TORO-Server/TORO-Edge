package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type StreamOptions struct {
	upstream *Upstream
	// 元に聞くパス (エスケープしたまま) とクエリ
	path func(c *Ctx) (string, string)
	// 客の頭書きのうち、そのまま渡すもの
	forward []hid
	// 足す頭書き
	set func(c *Ctx) map[string]string
	// 元の返事の頭書きの付け替え
	rules         *ResponseRules
	headerTimeout time.Duration
	// 本文が止まっていてよい時間
	idleTimeout time.Duration
	log         bool
}

// streamHandler はためずに、元の返事をそのまま客へ流す (動画、Range、SSE、権限をそのつど元が確かめるもの)
func streamHandler(o StreamOptions) Handler {
	return func(c *Ctx) {
		c.log = o.log
		c.cache = "BYPASS"
		headers := baseUpstreamHeaders(c.req)
		for _, id := range o.forward {
			if v := c.req.h[id]; v != "" {
				headers[hdrNames[id]] = v
			}
		}
		if o.set != nil {
			for k, v := range o.set(c) {
				headers[k] = v
			}
		}
		p, q := o.path(c)
		relayRequest(c, o.upstream, UpstreamRequest{method: c.method, path: p, query: q, headers: headers, headerTimeout: o.headerTimeout}, o.idleTimeout, o.rules)
	}
}

var copyBufs = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// relayRequest は元に聞いて、返事の頭書きと本文を、そのまま客へ流す。客が切れたら元も切る
func relayRequest(c *Ctx, up *Upstream, q UpstreamRequest, idle time.Duration, rules *ResponseRules) {
	cn := c.conn
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := cn.watch(cancel)
	defer cn.stopWatch(w)
	resp, cancelResp, err := up.do(ctx, q)
	if err != nil {
		badGateway(c)
		return
	}
	defer cancelResp()
	defer resp.Body.Close()

	status := resp.StatusCode
	ok := status < 400
	replaced := map[string]bool{}
	replaceCC := false
	if rules != nil && ok {
		for _, kv := range rules.set {
			replaced[http.CanonicalHeaderKey(kv[0])] = true
		}
		if rules.cacheControl != "" && rules.cacheControl != "upstream" {
			replaceCC = true
			replaced["Cache-Control"] = true
		}
	}
	hide := map[string]bool{}
	if rules != nil {
		for _, h := range rules.hide {
			hide[http.CanonicalHeaderKey(h)] = true
		}
	}
	var hb []byte
	for k, vals := range resp.Header {
		lk := strings.ToLower(k)
		// 長さの伝え方 (Content-Length / chunked) は、こちらで付け直す
		if hopByHop[lk] || lk == "content-length" || replaced[k] || hide[k] {
			continue
		}
		for _, v := range vals {
			hb = appendHeader(hb, k, v)
		}
	}
	if replaceCC {
		hb = appendHeader(hb, "Cache-Control", rules.cacheControl)
	}
	if rules != nil && ok {
		for _, kv := range rules.set {
			hb = appendHeader(hb, http.CanonicalHeaderKey(kv[0]), kv[1])
		}
	}
	s, err := c.beginStream(status, hb, resp.ContentLength)
	if err != nil || c.head || status == 204 || status == 304 {
		return
	}

	var timer *time.Timer
	if idle > 0 {
		timer = time.AfterFunc(idle, cancel)
		defer timer.Stop()
	}
	bp := copyBufs.Get().(*[]byte)
	defer copyBufs.Put(bp)
	buf := *bp
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if timer != nil {
				timer.Reset(idle)
			}
			if s.write(buf[:n]) != nil {
				return
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				s.finish()
			} else {
				// 元が途中で切れた: 終わりを伝えずに閉じる (客は、途中で切れたと分かる)
				cn.closeAfter = true
			}
			return
		}
	}
}

package main

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Upstream は元のサーバー 1 つへの接続 (つなぎっぱなし)。同じ相手に何度もつなぎ直さない。
// 長く続くもの (SSE、動画、遅い客への中継) で、ためるための短い取得を待たせないよう、接続の口を分ける
type Upstream struct {
	name     string
	endpoint Endpoint
	// ためるために聞く口
	transport *http.Transport
	// そのまま流す口 (relayRequest)
	streams *http.Transport
}

func newUpstream(name string, ep Endpoint, maxConns int, dialTimeout time.Duration) *Upstream {
	return &Upstream{
		name:      name,
		endpoint:  ep,
		transport: newTransport(maxConns, dialTimeout),
		streams:   newTransport(maxConns, dialTimeout),
	}
}

func newTransport(maxConns int, dialTimeout time.Duration) *http.Transport {
	return &http.Transport{
		DialContext:         (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        maxConns,
		MaxIdleConnsPerHost: maxConns / 2,
		MaxConnsPerHost:     maxConns,
		IdleConnTimeout:     90 * time.Second,
		// 圧縮は元に頼まず、ここで 1 度だけ行う (Accept-Encoding: identity を自分で付ける)
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
		WriteBufferSize:    32 << 10,
		ReadBufferSize:     32 << 10,
	}
}

// UpstreamRequest は元に聞く 1 回ぶん
type UpstreamRequest struct {
	method        string
	path          string // ? の前 (エスケープしたまま)
	query         string // ? を除く
	headers       map[string]string
	headerTimeout time.Duration
	// そのまま流す口を使う
	stream bool
}

// do は頭書きが届いたら返す。呼んだ側は、本文を読み終えるか Close し、cancel を呼ぶ。
// ctx が終わると、元への接続も切れる (客が切れたとき)
func (u *Upstream) do(ctx context.Context, q UpstreamRequest) (*http.Response, context.CancelFunc, error) {
	stats.upstreamRequests.Add(1)
	target, err := url.ParseRequestURI(q.path)
	if err != nil {
		stats.upstreamErrors.Add(1)
		return nil, nil, err
	}
	target.Scheme = "http"
	target.Host = u.endpoint.Addr()
	target.RawQuery = q.query
	cctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(cctx, q.method, target.String(), nil)
	if err != nil {
		cancel()
		stats.upstreamErrors.Add(1)
		return nil, nil, err
	}
	for k, v := range q.headers {
		if k == "host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	// 頭書きが届くまでの時間 (届いたら止める。本文を待つ時間は別に見る)
	timer := time.AfterFunc(q.headerTimeout, cancel)
	transport := u.transport
	if q.stream {
		transport = u.streams
	}
	resp, err := transport.RoundTrip(req)
	timer.Stop()
	if err != nil {
		cancel()
		stats.upstreamErrors.Add(1)
		return nil, nil, err
	}
	return resp, cancel, nil
}

func (u *Upstream) close() {
	u.transport.CloseIdleConnections()
	u.streams.CloseIdleConnections()
}

// baseUpstreamHeaders は元に渡す頭書き。客の頭書きは、ここに書いたものだけを渡す
// (X-Toro-Client-IP のような内側の頭書きを、客に書かせない)。
// Host は客のもの (map-web の fromMapPage が Origin と比べる)。圧縮は元に頼まず、ここで 1 度だけ行う
func baseUpstreamHeaders(r *Request) map[string]string {
	out := map[string]string{
		"host":            r.Host,
		"accept-encoding": "identity",
	}
	if xff := r.h[hXForwardedFor]; xff != "" {
		out["X-Forwarded-For"] = xff + ", " + r.remote
	} else {
		out["X-Forwarded-For"] = r.remote
	}
	if p := r.h[hXForwardedProto]; p != "" {
		out["X-Forwarded-Proto"] = p
	} else {
		out["X-Forwarded-Proto"] = "http"
	}
	if cf := r.h[hCfConnectingIP]; cf != "" {
		out["Cf-Connecting-Ip"] = cf
	}
	return out
}

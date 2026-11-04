package main

import (
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

const (
	// 1 つの IP から同時に流せる数 (動画、assets、大きくてためられないもの)。看板の多い場所では画像を一度にたくさん読む
	maxStreamsPerIP = 64
	// 1 つの IP から同時につなげる SSE の数。WebSocket につながらないときの代わりなので少しでよい
	maxSSEPerIP = 8
	// 1 つの IP から同時につなげる WebSocket の数
	maxSocketsPerIP = 16
)

var (
	streamLimit = newIPLimit(maxStreamsPerIP)
	sseLimit    = newIPLimit(maxSSEPerIP)
	socketLimit = newIPLimit(maxSocketsPerIP)
)

// clientKey は同時に使う数を数える単位。Cloudflare が付ける客の IP で、IPv6 は /64 にまとめる (1 つの回線に /64 が丸ごと割り当てられるので)。
// 付いていないもの ("") は Docker の中からなので数えない
func clientKey(r *Request) string {
	ip := r.h[hCfConnectingIP]
	if strings.IndexByte(ip, ':') < 0 {
		return ip
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	p, err := a.Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}

// ipLimit は客ごとの同時に使う数の上限
type ipLimit struct {
	mu     sync.Mutex
	counts map[string]int
	max    int
}

func newIPLimit(max int) *ipLimit {
	return &ipLimit{counts: map[string]int{}, max: max}
}

// enter は 1 つ増やす。上限なら false。"" は数えない
func (l *ipLimit) enter(key string) bool {
	if key == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[key] >= l.max {
		return false
	}
	l.counts[key]++
	return true
}

func (l *ipLimit) leave(key string) {
	if key == "" {
		return
	}
	l.mu.Lock()
	if n := l.counts[key]; n <= 1 {
		delete(l.counts, key)
	} else {
		l.counts[key] = n - 1
	}
	l.mu.Unlock()
}

func tooManyStreams(c *Ctx) {
	sendText(c, http.StatusTooManyRequests, "Too Many Requests\n", "Retry-After", "5")
}

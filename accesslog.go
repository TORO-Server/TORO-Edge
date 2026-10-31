package main

import (
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AccessLog はまとめて書く (リクエストごとに write しない)
type AccessLog struct {
	out     io.Writer
	enabled bool

	mu       sync.Mutex
	buf      strings.Builder
	timer    *time.Timer
	stamp    string
	stampSec int64
}

func newAccessLog(out io.Writer, enabled bool) *AccessLog {
	return &AccessLog{out: out, enabled: enabled}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (l *AccessLog) add(c *Ctx, status int) {
	ms := float64(time.Since(c.start).Microseconds()) / 1000
	// 出さない指定でも、エラーと遅いものは出す
	if !l.enabled || (!c.log && status < 400 && ms < 1000) {
		return
	}
	if status == 404 && !c.log {
		return
	}
	r := c.req
	ip := r.h[hCfConnectingIP]
	if ip == "" {
		ip = r.remote
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if sec := now.Unix(); sec != l.stampSec {
		l.stampSec = sec
		l.stamp = now.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	b := &l.buf
	b.WriteString(ip)
	b.WriteString(" - - [")
	b.WriteString(l.stamp)
	b.WriteString("] \"")
	b.WriteString(c.method)
	b.WriteByte(' ')
	b.WriteString(c.path)
	b.WriteString(c.search)
	if r.http11 {
		b.WriteString(" HTTP/1.1\" ")
	} else {
		b.WriteString(" HTTP/1.0\" ")
	}
	b.WriteString(strconv.Itoa(status))
	b.WriteByte(' ')
	b.WriteString(strconv.FormatInt(c.bytes, 10))
	b.WriteString(" \"")
	b.WriteString(dash(r.h[hReferer]))
	b.WriteString("\" \"")
	b.WriteString(dash(r.h[hUserAgent]))
	b.WriteString("\" \"")
	b.WriteString(dash(r.Host))
	b.WriteString("\" cache:")
	b.WriteString(c.cache)
	b.WriteString(" rt:")
	b.WriteString(strconv.FormatFloat(ms/1000, 'f', 3, 64))
	b.WriteByte('\n')
	if b.Len() > 32<<10 {
		l.flushLocked()
	} else if l.timer == nil {
		l.timer = time.AfterFunc(200*time.Millisecond, l.flush)
	}
}

func (l *AccessLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
}

func (l *AccessLog) flushLocked() {
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	if l.buf.Len() == 0 {
		return
	}
	io.WriteString(l.out, l.buf.String())
	l.buf.Reset()
}

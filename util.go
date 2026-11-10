package main

import (
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	encBr   = 1
	encGzip = 2
)

var (
	encMu    sync.RWMutex
	encCache = map[string]int{}
)

// acceptedEncodings は Accept-Encoding から、返せる圧縮 (br, gzip) を取り出す。同じ文字列が何度も来るので覚えておく
func acceptedEncodings(header string) int {
	if header == "" {
		return 0
	}
	encMu.RLock()
	v, ok := encCache[header]
	encMu.RUnlock()
	if ok {
		return v
	}
	v = 0
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.ToLower(strings.TrimSpace(part)), ";")
		name := strings.TrimSpace(fields[0])
		q := 1.0
		for _, p := range fields[1:] {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, "q=") {
				f, err := strconv.ParseFloat(strings.TrimSpace(p[2:]), 64)
				if err != nil {
					f = math.NaN()
				}
				q = f
			}
		}
		if !(q > 0) {
			continue
		}
		switch name {
		case "br":
			v |= encBr
		case "gzip", "x-gzip":
			v |= encGzip
		}
	}
	encMu.Lock()
	if len(encCache) > 512 {
		encCache = map[string]int{}
	}
	encCache[header] = v
	encMu.Unlock()
	return v
}

// hostName は Host の頭書きから、ポートを除いた小文字の名前 (すでに小文字なら、新しい文字列を作らない)
func hostName(header string) string {
	if header == "" {
		return ""
	}
	h := strings.ToLower(header)
	if strings.HasPrefix(h, "[") {
		if i := strings.IndexByte(h, ']'); i >= 0 {
			return h[:i+1]
		}
		return h
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		return h[:i]
	}
	return h
}

type cacheControl struct {
	maxAge, sMaxAge             int64 // -1 なし
	noStore, noCache, isPrivate bool
}

var (
	reMaxAge  = regexp.MustCompile(`(?i)(?:^|[\s,])max-age\s*=\s*"?(\d+)`)
	reSMaxAge = regexp.MustCompile(`(?i)(?:^|[\s,])s-maxage\s*=\s*"?(\d+)`)
	reNoStore = regexp.MustCompile(`(?i)(?:^|[\s,])no-store\b`)
	reNoCache = regexp.MustCompile(`(?i)(?:^|[\s,])no-cache\b`)
	rePrivate = regexp.MustCompile(`(?i)(?:^|[\s,])private\b`)
)

// parseCacheControl は元の返事を受けたときだけ使う (ためたものを返すときは使わない)
func parseCacheControl(h string) cacheControl {
	cc := cacheControl{maxAge: -1, sMaxAge: -1}
	if m := reMaxAge.FindStringSubmatch(h); m != nil {
		cc.maxAge, _ = strconv.ParseInt(m[1], 10, 64)
	}
	if m := reSMaxAge.FindStringSubmatch(h); m != nil {
		cc.sMaxAge, _ = strconv.ParseInt(m[1], 10, 64)
	}
	cc.noStore = reNoStore.MatchString(h)
	cc.noCache = reNoCache.MatchString(h)
	cc.isPrivate = rePrivate.MatchString(h)
	return cc
}

// etagTokens は If-None-Match の中身 (複数あるときも) を、弱い印 (W/) を外して並べる
func etagTokens(header string) []string {
	var out []string
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimSpace(t)
		t = strings.TrimPrefix(t, "W/")
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// variantEtag は圧縮した版の ETag ("abc" → "abc-br")
func variantEtag(etag, suffix string) string {
	if etag == "" {
		return ""
	}
	weak := strings.HasPrefix(etag, "W/")
	body := etag
	if weak {
		body = etag[2:]
	}
	if !strings.HasSuffix(body, `"`) {
		return etag
	}
	out := body[:len(body)-1] + "-" + suffix + `"`
	if weak {
		return "W/" + out
	}
	return out
}

func nowMs() int64 { return time.Now().UnixMilli() }

// hdr は返す頭書き 1 つ (名前は正規形)
type hdr struct{ k, v string }

func mkHdr(k, v string) hdr { return hdr{k: http.CanonicalHeaderKey(k), v: v} }

// hopByHop は元に渡さない、つなぎごとの頭書き (小文字)
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "transfer-encoding": true, "te": true, "trailer": true, "upgrade": true,
	"proxy-authenticate": true, "proxy-authorization": true, "proxy-connection": true, "date": true, "server": true, "x-powered-by": true,
}

// ───── 正規表現を使わない経路の判定 (毎リクエスト通るので)

// asciiEqualFold は a が、小文字の b と大文字小文字を除いて同じか
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		if c != b[i] {
			return false
		}
	}
	return true
}

// denyPath は返してはいけないパス: .php (.php5 など) で終わるもの、/. で始まる部分 (隠しファイル) があるもの、
// エンコードしたドット・スラッシュ・バックスラッシュ・NUL があるもの (元でデコードされて、ここの判定をすり抜けないように)
func denyPath(p string) bool {
	if strings.Contains(p, "/.") {
		return true
	}
	if strings.IndexByte(p, '%') >= 0 && hasEncodedDelimiter(p) {
		return true
	}
	n := len(p)
	if n >= 4 && asciiEqualFold(p[n-4:], ".php") {
		return true
	}
	if n >= 5 && p[n-1] >= '0' && p[n-1] <= '9' && asciiEqualFold(p[n-5:n-1], ".php") {
		return true
	}
	return false
}

// hasEncodedDelimiter は %2e %2f %5c %00 があるか (大文字小文字は区別しない)
func hasEncodedDelimiter(p string) bool {
	for i := 0; i+2 < len(p); i++ {
		if p[i] != '%' {
			continue
		}
		a, b := p[i+1], p[i+2]|0x20
		if (a == '2' && (b == 'e' || b == 'f')) || (a == '5' && b == 'c') || (a == '0' && p[i+2] == '0') {
			return true
		}
	}
	return false
}

// mapsLive は /maps/<地図>/live/<rest> の地図と rest を取り出す
func mapsLive(path string) (string, string, bool) {
	const pre = "/maps/"
	if !strings.HasPrefix(path, pre) {
		return "", "", false
	}
	r := path[len(pre):]
	i := strings.IndexByte(r, '/')
	if i <= 0 {
		return "", "", false
	}
	id := r[:i]
	r = r[i+1:]
	if !strings.HasPrefix(r, "live/") {
		return "", "", false
	}
	return id, r[len("live/"):], true
}

// isDigits は 1 文字以上の数字だけか
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// upWorld は /up/world/<ワールド>/<時刻> のワールドを取り出す。時刻は数字だけ (ブラウザは失敗すると 1 を引くので、負も受ける)
func upWorld(path string) (string, bool) {
	const pre = "/up/world/"
	if !strings.HasPrefix(path, pre) {
		return "", false
	}
	r := path[len(pre):]
	i := strings.IndexByte(r, '/')
	if i <= 0 {
		return "", false
	}
	ts := strings.TrimPrefix(r[i+1:], "-")
	if len(ts) > 20 || !isDigits(ts) {
		return "", false
	}
	return r[:i], true
}

// tileQuery は /tiles/ のクエリのうち、toro-webmap が見る timestamp=<数字> だけを取り出す (なければ "")
func tileQuery(search string) string {
	if search == "" {
		return ""
	}
	for q := search[1:]; q != ""; {
		part := q
		if i := strings.IndexByte(q, '&'); i >= 0 {
			part, q = q[:i], q[i+1:]
		} else {
			q = ""
		}
		if v, ok := strings.CutPrefix(part, "timestamp="); ok && isDigits(v) {
			return part
		}
	}
	return ""
}

// skinQuery は /mcassets/skin のクエリのうち、name と uuid だけを並べる (ほかのものを変えて別のものとしてためさせない)
func skinQuery(search string) string {
	if search == "" {
		return ""
	}
	var out strings.Builder
	for _, part := range strings.Split(search[1:], "&") {
		if strings.HasPrefix(part, "name=") || strings.HasPrefix(part, "uuid=") {
			if out.Len() > 0 {
				out.WriteByte('&')
			}
			out.WriteString(part)
		}
	}
	return out.String()
}

// assetFile は /<12 文字>.<拡張子> のファイル名を取り出す
func assetFile(p string) (string, bool) {
	if len(p) < 15 || len(p) > 24 || p[0] != '/' {
		return "", false
	}
	for i := 1; i <= 12; i++ {
		c := p[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '2' && c <= '7') {
			return "", false
		}
	}
	if p[13] != '.' {
		return "", false
	}
	for i := 14; i < len(p); i++ {
		c := p[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
			return "", false
		}
	}
	return p[1:], true
}

// hasToken はカンマで区切った頭書きの値に、トークンがあるか (大文字小文字は区別しない。tok は小文字)
func hasToken(v, tok string) bool {
	for len(v) > 0 {
		var part string
		if i := strings.IndexByte(v, ','); i >= 0 {
			part, v = v[:i], v[i+1:]
		} else {
			part, v = v, ""
		}
		if asciiEqualFold(strings.TrimSpace(part), tok) {
			return true
		}
	}
	return false
}

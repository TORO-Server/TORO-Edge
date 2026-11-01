// nginx (古い入り口) と edge (新しい入り口) に、同じ URL を同じ Host で聞いて、返事の違いを集める。
//
// 使い方 (Docker の minecraft-network の中で):
//
//	parity --old toro-nginx:80 --new toro-edge:80 --paths paths.txt
//
// paths.txt は 1 行 1 つ。"<パス>" か "<Host> <パス>"。Host を書かないものは、3 つのサイトすべてで聞く
package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
)

var hosts = []string{"map.torosaba.net", "3d-map.torosaba.net", "assets.torosaba.net"}

// 返事が刻々と変わる (ライブ情報、更新) ので、本文は比べない
var volatile = regexp.MustCompile(`/live/|^/up/(?:world|configuration)|^/maps/[^/]+/settings\.json$`)

// 違って当然の頭書き
var ignore = map[string]bool{
	"date": true, "server": true, "connection": true, "keep-alive": true, "content-length": true, "transfer-encoding": true,
	"etag": true, "last-modified": true, "vary": true, "x-edge-cache": true, "accept-ranges": true, "x-powered-by": true,
	"age": true, "content-encoding": true,
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

var client = &http.Client{
	Timeout:       20 * time.Second,
	Transport:     &http.Transport{DisableKeepAlives: true, DisableCompression: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func get(target, host, path, encoding string) (*reply, error) {
	req, err := http.NewRequest("GET", "http://"+target+path, nil)
	if err != nil {
		return nil, err
	}
	req.Host = host
	req.Header.Set("Accept-Encoding", encoding)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return &reply{res.StatusCode, res.Header, body}, nil
}

func decode(r *reply) []byte {
	var rd io.Reader
	switch r.header.Get("Content-Encoding") {
	case "br":
		rd = brotli.NewReader(bytes.NewReader(r.body))
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(r.body))
		if err != nil {
			return r.body
		}
		rd = zr
	case "deflate":
		// Node の inflateSync は zlib 形式
		zr, err := zlib.NewReader(bytes.NewReader(r.body))
		if err != nil {
			rd = flate.NewReader(bytes.NewReader(r.body))
		} else {
			rd = zr
		}
	default:
		return r.body
	}
	out, err := io.ReadAll(rd)
	if err != nil {
		return r.body
	}
	return out
}

func sha(b []byte) string {
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])[:12]
}

func mediaType(v string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(v, ";", 2)[0]))
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func main() {
	oldTarget := flag.String("old", "toro-nginx:80", "old entry (host:port)")
	newTarget := flag.String("new", "toro-edge:80", "new entry (host:port)")
	pathsFile := flag.String("paths", "", "paths file")
	flag.Parse()
	if *pathsFile == "" {
		fmt.Fprintln(os.Stderr, "--paths <file>")
		os.Exit(2)
	}
	data, err := os.ReadFile(*pathsFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	type target struct{ host, path string }
	var targets []target
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		parts := strings.Fields(t)
		if len(parts) >= 2 && strings.Contains(parts[0], ".") {
			targets = append(targets, target{parts[0], parts[1]})
		} else {
			for _, h := range hosts {
				targets = append(targets, target{h, parts[0]})
			}
		}
	}

	headerDiffs := map[string]int{}
	examples := map[string]string{}
	var statusDiffs, bodyDiffs, errs []string
	compared, identical := 0, 0

	for _, t := range targets {
		for _, encoding := range []string{"gzip, br", "identity"} {
			type pair struct {
				r   *reply
				err error
			}
			ca, cb := make(chan pair, 1), make(chan pair, 1)
			go func() { r, err := get(*oldTarget, t.host, t.path, encoding); ca <- pair{r, err} }()
			go func() { r, err := get(*newTarget, t.host, t.path, encoding); cb <- pair{r, err} }()
			pa, pb := <-ca, <-cb
			if pa.err != nil || pb.err != nil {
				e := pa.err
				if e == nil {
					e = pb.err
				}
				errs = append(errs, fmt.Sprintf("%s %s: %v", t.host, t.path, e))
				continue
			}
			a, b := pa.r, pb.r
			compared++
			same := true
			if a.status != b.status {
				statusDiffs = append(statusDiffs, fmt.Sprintf("%s %s [%s]: nginx %d → edge %d", t.host, t.path, encoding, a.status, b.status))
				continue
			}
			if a.status == 200 || a.status == 204 {
				ma, mb := mediaType(a.header.Get("Content-Type")), mediaType(b.header.Get("Content-Type"))
				if ma != mb {
					same = false
					k := fmt.Sprintf("content-type: %s → %s", ma, mb)
					headerDiffs[k]++
					examples[k] = t.host + t.path
				}
				if !volatile.MatchString(t.path) {
					da, db := decode(a), decode(b)
					if sha(da) != sha(db) {
						same = false
						bodyDiffs = append(bodyDiffs, fmt.Sprintf("%s %s [%s]: nginx %dB %s → edge %dB %s", t.host, t.path, encoding, len(da), sha(da), len(db), sha(db)))
					}
				}
			}
			names := map[string]bool{}
			for n := range a.header {
				names[strings.ToLower(n)] = true
			}
			for n := range b.header {
				names[strings.ToLower(n)] = true
			}
			for n := range names {
				if ignore[n] || n == "content-type" {
					continue
				}
				va, vb := "(none)", "(none)"
				if v := a.header.Values(n); len(v) > 0 {
					va = strings.Join(v, ", ")
				}
				if v := b.header.Values(n); len(v) > 0 {
					vb = strings.Join(v, ", ")
				}
				if va != vb {
					same = false
					k := fmt.Sprintf("%s: %s → %s", n, cut(va, 90), cut(vb, 90))
					headerDiffs[k]++
					examples[k] = fmt.Sprintf("%d %s%s", a.status, t.host, t.path)
				}
			}
			if same {
				identical++
			}
		}
	}

	fmt.Printf("compared %d (%d URL × 2 encodings): identical %d\n", compared, len(targets), identical)
	fmt.Printf("\nstatus differences: %d\n", len(statusDiffs))
	for _, s := range statusDiffs[:min(40, len(statusDiffs))] {
		fmt.Println("  " + s)
	}
	fmt.Printf("\nbody differences: %d\n", len(bodyDiffs))
	for _, s := range bodyDiffs[:min(40, len(bodyDiffs))] {
		fmt.Println("  " + s)
	}
	fmt.Printf("\nheader differences (distinct): %d\n", len(headerDiffs))
	keys := make([]string, 0, len(headerDiffs))
	for k := range headerDiffs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return headerDiffs[keys[i]] > headerDiffs[keys[j]] })
	for _, k := range keys[:min(60, len(keys))] {
		fmt.Printf("  ×%d  %s   e.g. %s\n", headerDiffs[k], k, examples[k])
	}
	fmt.Printf("\nerrors: %d\n", len(errs))
	for _, s := range errs[:min(20, len(errs))] {
		fmt.Println("  " + s)
	}
}

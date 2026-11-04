// 入り口 (edge) に負荷をかけて、毎秒の数と待ち時間を測る。
//
// 使い方 (Docker の minecraft-network の中で):
//
//	bench --target toro-edge:80 --label edge --scenarios scenarios.json [--seconds 8] [--connections 64] [--workers 2] [--only mix]
//
// scenarios.json: [{ "name": "...", "host": "map.torosaba.net", "path": "/css/dynmap_style.css", "headers": { ... } }, ...]
//
//	"paths": [...] を書くと、その中から順に取る。"conditional": true なら、先に 1 度取って ETag を If-None-Match に付ける (304 の経路)
//
// --workers は、負荷をかける側が使う CPU の数 (GOMAXPROCS)。入り口の CPU を増やすときは、こちらも増やす (先に頭打ちにならないように)
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type scenario struct {
	Name        string            `json:"name"`
	Host        string            `json:"host"`
	Path        string            `json:"path"`
	Paths       []string          `json:"paths"`
	Headers     map[string]string `json:"headers"`
	Conditional bool              `json:"conditional"`
}

type result struct {
	Label    string  `json:"label"`
	Scenario string  `json:"scenario"`
	Total    int     `json:"total"`
	RPS      float64 `json:"rps"`
	P50      float64 `json:"p50"`
	P975     float64 `json:"p975"`
	P99      float64 `json:"p99"`
	Max      float64 `json:"max"`
	MBps     float64 `json:"mbps"`
	OK       int     `json:"ok"`
	Non2xx   int     `json:"non2xx"`
	Errors   int     `json:"errors"`
}

func main() {
	target := flag.String("target", "toro-edge:80", "host:port")
	label := flag.String("label", "", "label in the RESULT line (default: target)")
	scenariosFile := flag.String("scenarios", "scenarios.json", "scenario file")
	seconds := flag.Int("seconds", 8, "duration per scenario")
	connections := flag.Int("connections", 64, "keep-alive connections")
	workers := flag.Int("workers", 2, "client CPUs (GOMAXPROCS)")
	only := flag.String("only", "", "run only scenarios whose name contains this")
	flag.Parse()
	if *label == "" {
		*label = *target
	}
	runtime.GOMAXPROCS(*workers)

	data, err := os.ReadFile(*scenariosFile)
	if err != nil {
		fatal(err)
	}
	var scenarios []scenario
	if err := json.Unmarshal(data, &scenarios); err != nil {
		fatal(err)
	}

	fmt.Printf("target=%s label=%s connections=%d seconds=%d\n", *target, *label, *connections, *seconds)
	fmt.Printf("%-26s %9s %8s %8s %8s %8s %8s %10s %8s %7s\n", "scenario", "req/s", "p50 ms", "p97.5", "p99", "max", "MB/s", "2xx/3xx", "non2xx", "errors")
	var rows []result
	for _, s := range scenarios {
		if *only != "" && !strings.Contains(s.Name, *only) {
			continue
		}
		paths := s.Paths
		if len(paths) == 0 {
			paths = []string{s.Path}
		}
		var requests [][]byte
		for _, p := range paths {
			headers := map[string]string{}
			for k, v := range s.Headers {
				headers[k] = v
			}
			if s.Conditional {
				if etag := fetchETag(*target, s.Host, p, headers); etag != "" {
					headers["if-none-match"] = etag
				}
			}
			requests = append(requests, buildRequest(s.Host, p, headers))
		}
		// 温める (最初の 1 回の遅さを測らない)
		for _, p := range paths {
			fetchETag(*target, s.Host, p, s.Headers)
		}
		r := runScenario(*target, requests, *connections, time.Duration(*seconds)*time.Second)
		r.Label, r.Scenario = *label, s.Name
		fmt.Printf("%-26s %9d %8.1f %8.1f %8.1f %8.1f %8.1f %10d %8d %7d\n", s.Name, int(r.RPS+0.5), r.P50, r.P975, r.P99, r.Max, r.MBps, r.OK, r.Non2xx, r.Errors)
		rows = append(rows, r)
	}
	out, _ := json.Marshal(rows)
	fmt.Printf("RESULT %s\n", out)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func buildRequest(host, path string, headers map[string]string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\n", path, host)
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

// fetchETag は 1 回取って ETag を返す (取れなければ空)
func fetchETag(target, host, path string, headers map[string]string) string {
	req, err := http.NewRequest("GET", "http://"+target+path, nil)
	if err != nil {
		return ""
	}
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true}}
	res, err := c.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	return res.Header.Get("Etag")
}

type stat struct {
	lat                []float64 // ms
	bytes              int64
	ok, non2xx, errors int
}

func runScenario(target string, requests [][]byte, connections int, d time.Duration) result {
	var wg sync.WaitGroup
	stats := make([]stat, connections)
	deadline := time.Now().Add(d)
	start := time.Now()
	for i := 0; i < connections; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			worker(target, requests, i, deadline, &stats[i])
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()

	var all []float64
	var r result
	var bytes int64
	for _, s := range stats {
		all = append(all, s.lat...)
		bytes += s.bytes
		r.OK += s.ok
		r.Non2xx += s.non2xx
		r.Errors += s.errors
	}
	sort.Float64s(all)
	q := func(p float64) float64 {
		if len(all) == 0 {
			return 0
		}
		i := int(float64(len(all)) * p)
		if i >= len(all) {
			i = len(all) - 1
		}
		return all[i]
	}
	r.Total = len(all)
	r.RPS = float64(len(all)) / elapsed
	r.P50, r.P975, r.P99 = q(0.5), q(0.975), q(0.99)
	if len(all) > 0 {
		r.Max = all[len(all)-1]
	}
	r.MBps = float64(bytes) / elapsed / 1048576
	return r
}

// worker は 1 本のつなぎ (keep-alive) で、deadline まで順に取り続ける
func worker(target string, requests [][]byte, id int, deadline time.Time, s *stat) {
	var conn net.Conn
	var br *bufio.Reader
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	for n := id; time.Now().Before(deadline); n++ {
		if conn == nil {
			c, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				s.errors++
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			conn, br = c, bufio.NewReaderSize(c, 128<<10)
		}
		t0 := time.Now()
		conn.SetDeadline(t0.Add(20 * time.Second))
		if _, err := conn.Write(requests[n%len(requests)]); err != nil {
			s.errors++
			conn.Close()
			conn = nil
			continue
		}
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			s.errors++
			conn.Close()
			conn = nil
			continue
		}
		size, err := io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if err != nil {
			s.errors++
			conn.Close()
			conn = nil
			continue
		}
		s.lat = append(s.lat, float64(time.Since(t0).Microseconds())/1000)
		s.bytes += size
		if res.StatusCode >= 200 && res.StatusCode < 400 {
			s.ok++
		} else {
			s.non2xx++
		}
		if res.Close {
			conn.Close()
			conn = nil
		}
	}
}

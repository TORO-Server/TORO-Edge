// 空のキャッシュに、同じ URL の一覧を 1 回ずつ取って、かかった時間を測る (元に聞きに行く経路。ヒットではない)。
//
// 使い方: coldpass --target toro-edge:80 --scenarios scenarios.json --only mix [--concurrency 8]
//
// 先に、キャッシュを空にしておく (edge は POST /purge、nginx は空のキャッシュで起動し直す)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type scenario struct {
	Name    string            `json:"name"`
	Host    string            `json:"host"`
	Path    string            `json:"path"`
	Paths   []string          `json:"paths"`
	Headers map[string]string `json:"headers"`
}

func main() {
	target := flag.String("target", "toro-edge:80", "host:port")
	scenariosFile := flag.String("scenarios", "scenarios.json", "scenario file")
	concurrency := flag.Int("concurrency", 8, "concurrent requests")
	only := flag.String("only", "mix", "scenario name substring")
	flag.Parse()

	data, err := os.ReadFile(*scenariosFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var all []scenario
	if err := json.Unmarshal(data, &all); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var sc *scenario
	for i := range all {
		if strings.Contains(all[i].Name, *only) {
			sc = &all[i]
			break
		}
	}
	if sc == nil {
		fmt.Fprintf(os.Stderr, "no scenario %s\n", *only)
		os.Exit(1)
	}
	paths := sc.Paths
	if len(paths) == 0 {
		paths = []string{sc.Path}
	}

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: *concurrency, DisableCompression: true}}
	get := func(path string) float64 {
		start := time.Now()
		req, err := http.NewRequest("GET", "http://"+*target+path, nil)
		if err != nil {
			return 0
		}
		req.Host = sc.Host
		for k, v := range sc.Headers {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			return 0
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return float64(time.Since(start).Microseconds()) / 1000
	}

	var mu sync.Mutex
	var times []float64
	next := 0
	begin := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= len(paths) {
					mu.Unlock()
					return
				}
				p := paths[next]
				next++
				mu.Unlock()
				ms := get(p)
				mu.Lock()
				times = append(times, ms)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	total := float64(time.Since(begin).Microseconds()) / 1000
	sort.Float64s(times)
	q := func(p float64) float64 {
		i := int(float64(len(times)) * p)
		if i >= len(times) {
			i = len(times) - 1
		}
		return times[i]
	}
	out, _ := json.Marshal(map[string]any{
		"target": *target, "n": len(paths), "concurrency": *concurrency,
		"totalMs": round1(total), "p50": round1(q(0.5)), "p99": round1(q(0.99)), "max": round1(times[len(times)-1]),
	})
	fmt.Printf("RESULT %s\n", out)
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

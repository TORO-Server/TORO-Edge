package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version はビルドのときに -X で入れる (GitHub Actions では、タグかコミット)
var version = "dev"

// healthcheck は Docker のヘルスチェック用 (イメージに wget などを入れていないので、自分で管理用のポートに聞く)
func healthcheck(cfg Config) int {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.AdminPort))
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func main() {
	cfg := loadConfig(environ())
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(cfg))
	}
	edge, err := startEdge(cfg, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "toro-edge:", err)
		os.Exit(1)
	}
	fmt.Printf("toro-edge listening on :%d (admin :%d), cache %d MiB, version %s\n", edge.Port, edge.AdminPort, cfg.CacheBytes>>20, version)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	s := <-sig
	fmt.Printf("%s: shutting down\n", s)
	edge.close(time.Duration(cfg.ShutdownGraceMs) * time.Millisecond)
}

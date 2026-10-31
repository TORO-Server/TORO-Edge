package main

import (
	"os"
	"strconv"
	"strings"
)

// Endpoint は元のサーバー (host:port)
type Endpoint struct {
	Host string
	Port int
}

func (e Endpoint) Addr() string { return e.Host + ":" + strconv.Itoa(e.Port) }

type Config struct {
	Port int
	// 管理用 (/healthz, /stats, POST /purge)。Docker の中のネットワークからだけ。Tunnel には出さない
	AdminPort int
	// toro-map-web (Nuxt)
	MapWeb Endpoint
	// BlueMap 内蔵の Web サーバー (ライブ情報)
	BlueMapLive Endpoint
	// Dynmap の web/ と、BlueMap の web/
	DynmapWeb    string
	BlueMapWeb   string
	HostsMap     []string
	HostsBlueMap []string
	HostsAssets  []string
	// 地図のデータをためる上限 (バイト)
	CacheBytes int64
	// これより大きい返事はためずに流す (バイト)
	MaxEntityBytes int64
	AccessLog      bool
	// 返事に x-edge-cache を付ける
	DebugHeaders bool
	// 止めるとき、返し終えていないものを待つ時間 (ミリ秒)
	ShutdownGraceMs int
	// この大きさ (バイト) 以上の本文は、メモリ上のファイルから sendfile で送る (0 なら使わない)
	SendfileMin int
}

func envOr(env map[string]string, key, fallback string) string {
	if v, ok := env[key]; ok && v != "" {
		return v
	}
	return fallback
}

func envInt(env map[string]string, key string, fallback int) int {
	if v, ok := env[key]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func endpoint(value string, defaultPort int) Endpoint {
	i := strings.LastIndex(value, ":")
	if i < 0 {
		return Endpoint{Host: value, Port: defaultPort}
	}
	port, err := strconv.Atoi(value[i+1:])
	if err != nil {
		port = defaultPort
	}
	return Endpoint{Host: value[:i], Port: port}
}

func hostList(value string, fallback string) []string {
	if value == "" {
		value = fallback
	}
	var out []string
	for _, s := range strings.Split(value, ",") {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func loadConfig(env map[string]string) Config {
	cacheBytes := int64(2048) << 20
	if v := envOr(env, "EDGE_CACHE_BYTES", ""); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cacheBytes = n
		}
	} else if v := envOr(env, "EDGE_CACHE_MB", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cacheBytes = int64(f * 1024 * 1024)
		}
	}
	maxEntity := cacheBytes / 8
	if maxEntity > 32<<20 {
		maxEntity = 32 << 20
	}
	if v := envOr(env, "EDGE_MAX_ENTITY_BYTES", ""); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			maxEntity = n
		}
	}
	return Config{
		Port:            envInt(env, "EDGE_PORT", 80),
		AdminPort:       envInt(env, "EDGE_ADMIN_PORT", 8081),
		MapWeb:          endpoint(envOr(env, "EDGE_MAPWEB", "toro-map-web:3000"), 3000),
		BlueMapLive:     endpoint(envOr(env, "EDGE_BLUEMAP_LIVE", "toro-main:8100"), 8100),
		DynmapWeb:       envOr(env, "EDGE_DYNMAP_WEB", "/srv/dynmap-web"),
		BlueMapWeb:      envOr(env, "EDGE_BLUEMAP_WEB", "/srv/bluemap-web"),
		HostsMap:        hostList(env["EDGE_HOST_MAP"], "map.torosaba.net"),
		HostsBlueMap:    hostList(env["EDGE_HOST_BLUEMAP"], "3d-map.torosaba.net"),
		HostsAssets:     hostList(env["EDGE_HOST_ASSETS"], "assets.torosaba.net"),
		CacheBytes:      cacheBytes,
		MaxEntityBytes:  maxEntity,
		AccessLog:       env["EDGE_ACCESS_LOG"] != "0",
		DebugHeaders:    env["EDGE_DEBUG_HEADERS"] == "1",
		ShutdownGraceMs: envInt(env, "EDGE_SHUTDOWN_GRACE_MS", 10000),
		SendfileMin:     envInt(env, "EDGE_SENDFILE_MIN", 64<<10),
	}
}

func environ() map[string]string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

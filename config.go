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
	// 管理用 (/healthz, /stats, POST /purge)。コンテナの中の 127.0.0.1 からだけ。Tunnel には出さない
	AdminPort int
	// 地図サーバー (Nuxt)
	MapWeb Endpoint
	// api.torosaba.net の /map/v1/ (と、その WebSocket) の振り分け先。EDGE_API_BACKEND で決める。決めなければ MapWeb と同じ (地図サーバー)。
	// 2D の地図 API を TORO-API へ移すときは、これだけを変える
	ApiBackend Endpoint
	// BlueMap 内蔵の Web サーバー (ライブ情報)
	BlueMapLive Endpoint
	// Dynmap の web/ と、BlueMap の web/
	DynmapWeb    string
	BlueMapWeb   string
	HostsMap     []string
	HostsBlueMap []string
	HostsAssets  []string
	// api.torosaba.net の /map/v1/ (Dynmap の API。ほかのパスは Tunnel が toro-api へ渡すので、ここには来ない)
	HostsApi []string
	// パススルーするサイト (ためず、読み替えず、本文も全部の頭書きも渡す)。Backend の Host が空のサイトは、受けない (Host の指定だけがあっても、元が決まるまで効かない)。
	// auth.torosaba.net (共通認証)、mcp.torosaba.net (AI コネクタ。SSE)、pass-api.torosaba.net (toro-main の中の受け口)
	HostsAuth      []string
	HostsMcp       []string
	HostsPassApi   []string
	AuthBackend    Endpoint
	McpBackend     Endpoint
	PassApiBackend Endpoint
	// api.torosaba.net の /map/ と /map3d/ 以外の全部を、ApiBackend へパススルーするか (EDGE_API_PASSTHROUGH=0 で止める。止めると 404 に戻る)
	ApiPassthrough bool
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

// optEndpoint は決めていなければ空の Endpoint (Host が空) を返す
func optEndpoint(env map[string]string, key string, defaultPort int) Endpoint {
	if v := envOr(env, key, ""); v != "" {
		return endpoint(v, defaultPort)
	}
	return Endpoint{}
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
	mapWeb := endpoint(envOr(env, "EDGE_MAPWEB", "localhost:3000"), 3000)
	apiBackend := mapWeb
	if v := envOr(env, "EDGE_API_BACKEND", ""); v != "" {
		apiBackend = endpoint(v, 3000)
	}
	return Config{
		Port:            envInt(env, "EDGE_PORT", 80),
		AdminPort:       envInt(env, "EDGE_ADMIN_PORT", 8081),
		MapWeb:          mapWeb,
		ApiBackend:      apiBackend,
		BlueMapLive:     endpoint(envOr(env, "EDGE_BLUEMAP_LIVE", "localhost:8100"), 8100),
		DynmapWeb:       envOr(env, "EDGE_DYNMAP_WEB", "/srv/dynmap-web"),
		BlueMapWeb:      envOr(env, "EDGE_BLUEMAP_WEB", "/srv/bluemap-web"),
		HostsMap:        hostList(env["EDGE_HOST_MAP"], "map.torosaba.net"),
		HostsBlueMap:    hostList(env["EDGE_HOST_BLUEMAP"], "3d-map.torosaba.net"),
		HostsAssets:     hostList(env["EDGE_HOST_ASSETS"], "assets.torosaba.net"),
		HostsApi:        hostList(env["EDGE_HOST_API"], "api.torosaba.net"),
		HostsAuth:       hostList(env["EDGE_HOST_AUTH"], "auth.torosaba.net"),
		HostsMcp:        hostList(env["EDGE_HOST_MCP"], "mcp.torosaba.net"),
		HostsPassApi:    hostList(env["EDGE_HOST_PASSAPI"], "pass-api.torosaba.net"),
		AuthBackend:     optEndpoint(env, "EDGE_AUTH_BACKEND", 80),
		McpBackend:      optEndpoint(env, "EDGE_MCP_BACKEND", 80),
		PassApiBackend:  optEndpoint(env, "EDGE_PASSAPI_BACKEND", 80),
		ApiPassthrough:  env["EDGE_API_PASSTHROUGH"] != "0",
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

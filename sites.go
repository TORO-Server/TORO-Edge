package main

import (
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	sec = int64(1000)
	min = 60 * sec
	hr  = 60 * min
	day = 24 * hr
)

// 3d-map.torosaba.net (BlueMap) に付けるヘッダー。BlueMap の webserver.conf の additional-headers と同じもの
// media-src: MarumaSign の看板の動画・音声・ライブ配信 (ライブ配信は MediaSource の blob: で再生する)
var blueMapHeaders = [][2]string{
	{"content-security-policy", "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob: https:; media-src 'self' https: blob:; connect-src 'self' https:; worker-src 'self' blob:; font-src 'self' data:; object-src 'none'; base-uri 'self'; form-action 'none'; frame-ancestors 'self'"},
	{"x-content-type-options", "nosniff"},
	{"referrer-policy", "no-referrer"},
}

// BlueMap 本体の返事から外す頭書き (BlueMap は max-age=86400 や自分の CSP を付けて返す)
var liveHide = []string{"cache-control", "cdn-cache-control", "content-security-policy", "x-content-type-options", "referrer-policy"}

// replaceOnce は大文字小文字を区別せず、最初の 1 つだけを置き換える (nginx の sub_filter と同じ)
func replaceOnce(id, search, replacement string) *Transform {
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(search))
	return &Transform{id: id, apply: func(body []byte) []byte {
		loc := re.FindIndex(body)
		if loc == nil {
			return body
		}
		out := make([]byte, 0, len(body)+len(replacement))
		out = append(out, body[:loc[0]]...)
		out = append(out, replacement...)
		return append(out, body[loc[1]:]...)
	}}
}

var (
	// Dynmap の前の画面 (index.html)。軽くするスクリプト (map-web/client/dynmap-client.ts) を、Dynmap の js を読み終えたところに差し込む。
	// Cloudflare がブラウザに 4 時間持たせるので、スクリプトを変えたら ?v= の数を上げる
	transformLegacy = replaceOnce("legacy", `<script type="text/javascript" src="standalone/config.js`,
		"<script type=\"text/javascript\" src=\"toro/dynmap-client.js?v=2\"></script>\n\t<script type=\"text/javascript\" src=\"standalone/config.js")
	// ブラウザの言語が一覧に無いときの言語を、英語から日本語にする
	transformLang = replaceOnce("lang", `default: "en"`, `default: "ja"`)
	// 前の画面の BlueMap は index.html を Cache-Control なしで返していたので、ブラウザが何日か持っていることがある。
	// 前の画面も settings.json は毎回確かめるので、scripts に読み直すスクリプトを足す (今の画面は読まない。BlueMapApp.ts の isReplacedScript)。
	// 内容から ETag を作るので、足す前のものを持つブラウザに 304 を返してしまうことはない
	transformSettings = replaceOnce("settings", `"scripts":[`, `"scripts":["toro-refresh.js?v=1",`)
)

// 前の画面のときだけ、1 度だけ読み直す (読み直すとブラウザはページを確かめ直し、今の画面が届く)
const toroRefresh = "(function(){if(document.getElementById('__nuxt'))return;try{if(sessionStorage.getItem('toro-refresh'))return;sessionStorage.setItem('toro-refresh','1')}catch(e){return}location.reload()})();\n"

// SiteDeps は経路の決まりを作るのに使うもの
type SiteDeps struct {
	config        Config
	cache         *EntityCache
	mapWeb        *Upstream
	live          *Upstream
	dynmapStatic  *StaticSite
	blueMapStatic *StaticSite
}

type preloadJob struct {
	site     *StaticSite
	rulesFor func(rel string) *StaticRules
	prime    func()
}

type Sites struct {
	byHost   map[string]*Site
	fallback *Site
	preload  []preloadJob
}

func buildSites(d SiteDeps) *Sites {
	cfg := d.config
	// 名前の空間 (同じパスでも、サイトや使い道が違えば、別のものをためる)
	nsMap := d.cache.space()
	nsBlue := d.cache.space()
	nsBlueLive := d.cache.space()
	nsBlueFallback := d.cache.space()

	// 鍵と元に聞く URL はパスだけ。クエリを変えて別のものとしてためさせない (BlueMap や古い画面が付けることもある)
	pathKey := func(c *Ctx) string { return c.path }
	pathOnly := func(c *Ctx) (string, string) { return c.path, "" }

	// proxied は map-web (Nuxt) の返事をためて返す。クエリを使うものは key と path を決める
	proxied := func(ns *nsCache, mutate func(o *CachedOptions)) Handler {
		o := CachedOptions{
			upstream:       d.mapWeb,
			cache:          ns,
			key:            pathKey,
			path:           pathOnly,
			errorTtl:       7 * day,
			tier:           tierFast,
			maxEntityBytes: cfg.MaxEntityBytes,
			log:            true,
		}
		mutate(&o)
		return cachedHandler(o)
	}

	deny := Rule{first: true, fn: func(p string) (string, bool) { return "", denyPath(p) }, handler: notFound}

	redirectToRoot := func(c *Ctx) {
		cn := c.conn
		b := append(cn.scratch[:0], statusLine(301)...)
		b = appendHeader(b, "Location", "/"+c.search)
		b = appendHeader(b, "Content-Length", "0")
		cn.scratch = b
		c.write(301, b, c.debugLine(), nil)
	}

	// ───────────── map.torosaba.net (Dynmap)
	mapNoCache := newStaticRules([2]string{"cache-control", "no-cache"})
	mapStatic := newStaticRules([2]string{"cache-control", "public, max-age=86400"})

	mapRules := []Rule{
		deny,
		// 地図の画面は toro-map-web (map-web/app/、Nuxt) が返す
		exactRule("/", proxied(nsMap, func(o *CachedOptions) {
			o.fixedTtl, o.swr = 10*sec, min
			o.response = ResponseRules{cacheControl: "no-cache", set: [][2]string{{"content-language", "ja"}}}
		})),
		prefixRule("/_nuxt/", proxied(nsMap, func(o *CachedOptions) {
			o.defaultTtl, o.maxTtl, o.swr, o.negativeTtl, o.tier = hr, day, day, 2*sec, tierBest
		})),
		// 前の画面へのリンク (index.html?worldname=...) は新しい画面へ
		exactRule("/index.html", redirectToRoot),
		// ホーム画面に追加するための Service Worker (map-web/client/sw.ts) と、アイコンなど (map-web/public/pwa/)。
		// Service Worker は更新がすぐ届くよう、毎回確かめさせる
		exactRule("/sw.js", proxied(nsMap, func(o *CachedOptions) {
			o.fixedTtl, o.swr = 10*sec, 30*sec
			o.response = ResponseRules{cacheControl: "no-cache"}
		})),
		prefixRule("/pwa/", proxied(nsMap, func(o *CachedOptions) {
			o.defaultTtl, o.maxTtl, o.swr, o.negativeTtl, o.tier = 5*min, hr, hr, 2*sec, tierBest
		})),
		// 前の画面 (Dynmap の web/index.html)。比べるために残しておく
		exactRule("/legacy.html", staticHandler(d.dynmapStatic, mapNoCache, staticOptions{file: "index.html", transform: transformLegacy})),
		// 変えたらすぐ届くよう、毎回 ETag で確かめさせる (2KB ほど)
		prefixRule("/toro/", proxied(nsMap, func(o *CachedOptions) {
			o.fixedTtl, o.swr, o.negativeTtl = 10*sec, 30*sec, 2*sec
			o.response = ResponseRules{cacheControl: "no-cache"}
		})),
		// JsonFileClientUpdateComponent が書き出す
		exactRule("/standalone/config.js", staticHandler(d.dynmapStatic, mapNoCache, staticOptions{})),
		// 設定とワールドの更新。全員が 2 秒ごとに読むので、ワールドごとに 1 秒だけまとめる (中身は時刻によらず同じ)
		exactRule("/up/configuration", proxied(nsMap, func(o *CachedOptions) {
			o.key = func(*Ctx) string { return "config" }
			o.fixedTtl, o.swr = 5*sec, 5*sec
		})),
		// 元に聞く URL も鍵から作る (先頭の客の時刻やクエリを、ワールドで 1 つの返事に持ち込まない)。時刻は見ないので 0
		fnRule(upWorld, proxied(nsMap, func(o *CachedOptions) {
			o.key = func(c *Ctx) string { return "update:" + c.cap }
			o.path = func(c *Ctx) (string, string) { return "/up/world/" + c.cap + "/0", "" }
			o.fixedTtl, o.swr, o.negativeTtl = sec, 2*sec, 2*sec
		})),
		// 更新とプレイヤーの位置を届ける WebSocket (サーバーが Upgrade を見て渡す。つながらないときは、ブラウザが上の 2 つを取りに来る)
		exactRule("/up/ws", func(c *Ctx) { sendText(c, http.StatusUpgradeRequired, "Upgrade Required\n", "Upgrade", "websocket") }),
		// Web チャットやログインは使っていない
		prefixRule("/up/", notFound),
		// 地図のタイル、マーカー、プレイヤーの顔。キャッシュの長さは map-web の Cache-Control に従う (なければ 5 分)
		// 描き直したタイルは ?timestamp= で別の URL になる (クエリはこれだけを見る)
		prefixRule("/tiles/", proxied(nsMap, func(o *CachedOptions) {
			o.key = func(c *Ctx) string {
				if q := tileQuery(c.search); q != "" {
					return c.path + "?" + q
				}
				return c.path
			}
			o.path = func(c *Ctx) (string, string) { return c.path, tileQuery(c.search) }
			o.defaultTtl, o.maxTtl, o.swr, o.negativeTtl, o.log = 5*min, day, 30*sec, 2*sec, false
		})),
		// js や css は index.html が ?_=<版> を付けて読む
		prefixRule("/", staticHandler(d.dynmapStatic, mapStatic, staticOptions{})),
	}

	// ───────────── 3d-map.torosaba.net (BlueMap)
	blueNoCache := newStaticRules(append([][2]string{{"cache-control", "no-cache"}}, blueMapHeaders...)...)
	blueStatic := newStaticRules(append([][2]string{{"cache-control", "public, max-age=86400, stale-if-error=604800"}, {"cdn-cache-control", "max-age=60"}}, blueMapHeaders...)...)

	// toro-main の再起動中は、BlueMap が DB に書いた最後の版を返す (toro-map-web 経由)。ここは Cloudflare にもためさせない
	liveFallback := proxied(nsBlueFallback, func(o *CachedOptions) {
		o.fixedTtl, o.swr, o.errorTtl, o.log = sec, sec, min, false
		o.response = ResponseRules{hide: []string{"access-control-allow-origin"}, cacheControl: "no-cache", set: blueMapHeaders}
	})

	sum := sha1.Sum([]byte(toroRefresh))
	refreshHeaders := []hdr{mkHdr("cache-control", "no-cache")}
	for _, kv := range blueMapHeaders {
		refreshHeaders = append(refreshHeaders, mkHdr(kv[0], kv[1]))
	}
	toroRefreshEntity := newEntity(entityInit{
		status:  200,
		typ:     "text/javascript; charset=utf-8",
		body:    []byte(toroRefresh),
		etag:    `"` + base64.RawURLEncoding.EncodeToString(sum[:])[:22] + `"`,
		headers: refreshHeaders,
		tier:    tierFast,
	})

	// マーカー。toro-map-web が BlueMap のマーカーに Dynmap の地点 (一覧にだけ出す) を足して返す
	markers := proxied(nsBlue, func(o *CachedOptions) {
		o.fixedTtl, o.swr, o.errorTtl, o.log = sec, sec, min, false
		o.response = ResponseRules{hide: []string{"access-control-allow-origin"}, cacheControl: "no-cache", set: blueMapHeaders}
	})
	// BlueMap 5 の Server-Sent Events (ブラウザが何時間もつなぎっぱなしにする)。終わらない返事なので、ためずにそのまま流す
	sse := streamHandler(StreamOptions{
		upstream: d.live,
		path:     func(c *Ctx) (string, string) { return c.path, strings.TrimPrefix(c.search, "?") },
		// EventSource が付ける頭書き (つなぎ直すときは、最後に受けたイベントの id)
		forward:       []hid{hAccept, hLastEventID},
		headerTimeout: 10 * time.Second,
		idleTimeout:   5 * time.Minute,
		log:           true,
		rules:         &ResponseRules{hide: liveHide, cacheControl: "no-cache", set: blueMapHeaders},
		limit:         sseLimit,
	})
	// ライブ情報 (プレイヤーの位置など) は BlueMap 内蔵の Web サーバーに聞く。BlueMap は max-age=86400 を付けて返すので、ブラウザに持たせない。
	// 見ている人の数だけ聞くと toro-main に響くので、1 秒だけまとめる
	live := cachedHandler(CachedOptions{
		upstream:       d.live,
		cache:          nsBlueLive,
		key:            pathKey,
		path:           pathOnly,
		fixedTtl:       sec,
		swr:            sec,
		errorTtl:       10 * sec,
		headerTimeout:  5 * time.Second,
		tier:           tierFast,
		maxEntityBytes: cfg.MaxEntityBytes,
		log:            false,
		fallback:       liveFallback,
		response:       ResponseRules{hide: liveHide, cacheControl: "no-cache", set: blueMapHeaders},
	})
	// 地図のデータ (タイル、テクスチャ、地図ごとの設定、プレイヤーの顔など)。
	// 同じタイルを何人もが読むので 60 秒まとめる (Cloudflare の CDN-Cache-Control と同じ長さ)
	mapData := proxied(nsBlue, func(o *CachedOptions) {
		o.fixedTtl, o.swr, o.negativeTtl, o.tier, o.log = min, 30*sec, 5*sec, tierBest, false
		o.response = ResponseRules{hide: []string{"access-control-allow-origin"}, set: append([][2]string{{"cdn-cache-control", "max-age=60"}}, blueMapHeaders...)}
	})

	blueRules := []Rule{
		deny,
		// /maps/<地図>/live/ はライブ情報 (markers.json は map-web、sse は流す、players.json は BlueMap 本体)。それ以外は地図のデータ。
		// ライブ情報はためきれないものが toro-main に届くので、知らない地図や知らない名前は聞きに行かない
		prefixRule("/maps/", func(c *Ctx) {
			if id, rest, ok := mapsLive(c.path); ok {
				if !d.blueMapStatic.knownMap(id) {
					notFound(c)
					return
				}
				switch rest {
				case "markers.json":
					markers(c)
				case "sse":
					sse(c)
				case "players.json":
					live(c)
				default:
					notFound(c)
				}
				return
			}
			mapData(c)
		}),
		// 画面は toro-map-web (map-web/app/bluemap/、BlueMap の webapp を Nuxt と TypeScript にしたもの) が返す
		exactRule("/", proxied(nsBlue, func(o *CachedOptions) {
			o.fixedTtl, o.swr = 10*sec, min
			o.response = ResponseRules{cacheControl: "no-cache", set: append(append([][2]string{}, blueMapHeaders...), [2]string{"content-language", "ja"})}
		})),
		prefixRule("/_nuxt/", proxied(nsBlue, func(o *CachedOptions) {
			o.defaultTtl, o.maxTtl, o.swr, o.negativeTtl, o.tier = hr, day, day, 2*sec, tierBest
			o.response = ResponseRules{set: blueMapHeaders}
		})),
		exactRule("/index.html", redirectToRoot),
		exactRule("/sw.js", proxied(nsBlue, func(o *CachedOptions) {
			o.fixedTtl, o.swr = 10*sec, 30*sec
			o.response = ResponseRules{cacheControl: "no-cache", set: blueMapHeaders}
		})),
		prefixRule("/pwa/", proxied(nsBlue, func(o *CachedOptions) {
			o.defaultTtl, o.maxTtl, o.swr, o.negativeTtl, o.tier = 5*min, hr, hr, 2*sec, tierBest
			o.response = ResponseRules{set: blueMapHeaders}
		})),
		exactRule("/lang/settings.conf", staticHandler(d.blueMapStatic, blueNoCache, staticOptions{transform: transformLang})),
		fnRule(func(p string) (string, bool) {
			return "", strings.HasPrefix(p, "/marumasign/") && strings.HasSuffix(p, ".json")
		}, staticHandler(d.blueMapStatic, blueNoCache, staticOptions{})),
		exactRule("/settings.json", staticHandler(d.blueMapStatic, blueNoCache, staticOptions{transform: transformSettings})),
		exactRule("/toro-refresh.js", func(c *Ctx) { c.cache = "HIT"; sendEntity(c, toroRefreshEntity) }),
		// MarumaSign の看板の、転送の行き先を調べるものと CORS のないサイトの中継 (toro-map-web は看板データにある URL とそこからたどれるものだけを読む)。
		// 動画や生放送をそのまま流すので、ためずに渡す。安全のための頭書き (CSP の sandbox など) は toro-map-web が付ける
		prefixRule("/_marumasign/", streamHandler(StreamOptions{
			upstream:      d.mapWeb,
			path:          func(c *Ctx) (string, string) { return c.path, strings.TrimPrefix(c.search, "?") },
			forward:       []hid{hRange, hOrigin, hSecFetchSite},
			headerTimeout: 300 * time.Second,
			idleTimeout:   300 * time.Second,
			log:           true,
			limit:         streamLimit,
		})),
		// プレイヤーの頭のスキンの URL。クエリは name と uuid だけを見る
		exactRule("/mcassets/skin", proxied(nsBlue, func(o *CachedOptions) {
			o.key = func(c *Ctx) string { return "skin?" + skinQuery(c.search) }
			o.path = func(c *Ctx) (string, string) { return c.path, skinQuery(c.search) }
			o.maxTtl, o.negativeTtl, o.tier = day, 5*sec, tierBest
			o.response = ResponseRules{set: blueMapHeaders}
		})),
		// 構造物の看板のブロックの形とテクスチャ (toro-map-web がクライアントの jar から作る)。Cache-Control のとおりためる
		prefixRule("/mcassets/", proxied(nsBlue, func(o *CachedOptions) {
			o.maxTtl, o.negativeTtl, o.tier = day, 5*sec, tierBest
			o.response = ResponseRules{set: blueMapHeaders}
		})),
		prefixRule("/", staticHandler(d.blueMapStatic, blueStatic, staticOptions{})),
	}

	// ───────────── assets.torosaba.net (MarumaSign Companion がホストした看板の画像など)
	// 読めるかどうかは toro-map-web が毎回確かめる (遊んでいる人の IP か、地図のページから) ので、ここではためない
	assetRules := []Rule{
		fnRule(assetFile, streamHandler(StreamOptions{
			upstream: d.mapWeb,
			path:     func(c *Ctx) (string, string) { return "/_assets/" + c.cap, "" },
			forward:  []hid{hRange, hIfNoneMatch, hOrigin, hReferer},
			// Cloudflare が付けるクライアントの IP (ここは Tunnel からしか届かない)。客が自分で書いた X-Toro-Client-IP は渡さない
			set: func(c *Ctx) map[string]string {
				if cf := c.req.h[hCfConnectingIP]; cf != "" {
					return map[string]string{"X-Toro-Client-Ip": cf}
				}
				return nil
			},
			headerTimeout: 30 * time.Second,
			idleTimeout:   60 * time.Second,
			log:           false,
			limit:         streamLimit,
		})),
		prefixRule("/", notFound),
	}

	mapSite := &Site{name: "map", route: compileRules(mapRules)}
	blueSite := &Site{name: "bluemap", route: compileRules(blueRules)}
	assetSite := &Site{name: "assets", route: compileRules(assetRules)}

	byHost := map[string]*Site{}
	for _, h := range cfg.HostsMap {
		byHost[h] = mapSite
	}
	for _, h := range cfg.HostsBlueMap {
		byHost[h] = blueSite
	}
	for _, h := range cfg.HostsAssets {
		byHost[h] = assetSite
	}

	return &Sites{
		byHost: byHost,
		// server_name _ (default_server): 知らない Host は map.torosaba.net と同じ
		fallback: mapSite,
		preload: []preloadJob{
			{
				site: d.dynmapStatic,
				rulesFor: func(rel string) *StaticRules {
					if rel == "standalone/config.js" {
						return mapNoCache
					}
					return mapStatic
				},
				prime: func() { d.dynmapStatic.prime("index.html", transformLegacy, mapNoCache) },
			},
			{
				site: d.blueMapStatic,
				rulesFor: func(rel string) *StaticRules {
					if rel == "settings.json" || rel == "lang/settings.conf" || (strings.HasPrefix(rel, "marumasign/") && strings.HasSuffix(rel, ".json")) {
						return blueNoCache
					}
					return blueStatic
				},
				prime: func() {
					d.blueMapStatic.prime("settings.json", transformSettings, blueNoCache)
					d.blueMapStatic.prime("lang/settings.conf", transformLang, blueNoCache)
				},
			},
		},
	}
}

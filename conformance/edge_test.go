package conformance

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

func unbr(t *testing.T, b []byte) string {
	t.Helper()
	out, err := io.ReadAll(brotli.NewReader(bytes.NewReader(b)))
	if err != nil {
		t.Fatalf("brotli: %v", err)
	}
	return string(out)
}

func ungz(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	return string(out)
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func hdr(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func none() overrides { return overrides{} }

func TestRouting(t *testing.T) {
	// PHP と隠しファイルは、あっても返さない / index.html は / へ
	withEnv(t, none(), func(e *env) {
		for _, path := range []string{"/standalone/MySQL_config.php", "/x.php", "/.secret", "/css/.x", "/..%2foutside.txt", "/%2e%2e/outside.txt", "/css/%2e%2e/%2e%2e/outside.txt", "/css/..%2f..%2foutside.txt"} {
			r := e.get(path)
			eq(t, r.status, 404, path)
			ok(t, !strings.Contains(string(r.body), "secret") && !strings.Contains(string(r.body), "outside the root"), path)
		}
		redirect := e.get("/index.html?worldname=main&mapname=surface")
		eq(t, redirect.status, 301)
		eq(t, redirect.h("Location"), "/?worldname=main&mapname=surface")
		eq(t, e.get("/index.html").h("Location"), "/")
		eq(t, e.blue("/index.html?x=1").h("Location"), "/?x=1")
		eq(t, e.mapweb.seenLen(), 0)
	})
}

func TestStaticDynmap(t *testing.T) {
	// ETag, 304, br / gzip / そのまま, HEAD
	withEnv(t, none(), func(e *env) {
		plain := e.get("/css/style.css")
		eq(t, plain.status, 200)
		eq(t, plain.h("Content-Type"), "text/css; charset=utf-8")
		eq(t, plain.h("Cache-Control"), "public, max-age=86400")
		ok(t, !plain.has("Content-Encoding"))
		eq(t, string(plain.body), css)
		eq(t, strings.ToLower(plain.h("Vary")), "accept-encoding")
		etag := plain.h("ETag")
		ok(t, etag != "")

		notModified := e.get("/css/style.css", reqOpts{headers: hdr("if-none-match", etag)})
		eq(t, notModified.status, 304)
		eq(t, len(notModified.body), 0)
		eq(t, notModified.h("Cache-Control"), "public, max-age=86400")

		br := e.get("/css/style.css", reqOpts{headers: hdr("accept-encoding", "gzip, br")})
		eq(t, br.h("Content-Encoding"), "br")
		eq(t, unbr(t, br.body), css)
		ok(t, len(br.body) < len(plain.body)/5)
		ok(t, br.h("ETag") != etag)
		// 圧縮した版の ETag でも、そのままの版の ETag でも 304
		eq(t, e.get("/css/style.css", reqOpts{headers: hdr("accept-encoding", "br", "if-none-match", br.h("ETag"))}).status, 304)
		eq(t, e.get("/css/style.css", reqOpts{headers: hdr("accept-encoding", "br", "if-none-match", etag)}).status, 304)

		g := e.get("/css/style.css", reqOpts{headers: hdr("accept-encoding", "gzip")})
		eq(t, g.h("Content-Encoding"), "gzip")
		eq(t, ungz(t, g.body), css)

		head := e.get("/css/style.css", reqOpts{method: "HEAD"})
		eq(t, head.status, 200)
		eq(t, len(head.body), 0)
		eq(t, head.h("Content-Length"), strconv.Itoa(len(plain.body)))

		png := e.get("/images/blank.png", reqOpts{headers: hdr("accept-encoding", "br")})
		eq(t, png.h("Content-Type"), "image/png")
		ok(t, !png.has("Content-Encoding"))
		eq(t, e.get("/images/nothing.png").status, 404)
		// Host が知らない名前でも map.torosaba.net と同じ (内側から toro-edge で呼ばれる)
		eq(t, e.get("/css/style.css", reqOpts{host: "toro-edge"}).status, 200)
		eq(t, e.mapweb.seenLen(), 0)
	})
}

func TestStaticReload(t *testing.T) {
	// ファイルが変わったら読み直す / 新しいファイルも返す
	withEnv(t, none(), func(e *env) {
		eq(t, string(e.get("/standalone/config.js").body), "var config = {}")
		changed := filepath.Join(e.dynDir, "standalone", "config.js")
		if err := os.WriteFile(changed, []byte(`var config = {"changed":true}`), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond)
		// 1 度目は古いものを返しながら裏で確かめる
		e.get("/standalone/config.js")
		time.Sleep(100 * time.Millisecond)
		r := e.get("/standalone/config.js")
		eq(t, string(r.body), `var config = {"changed":true}`)
		eq(t, r.h("Cache-Control"), "no-cache")
		eq(t, e.get("/new.txt").status, 404)
		time.Sleep(2100 * time.Millisecond)
		if err := os.WriteFile(filepath.Join(e.dynDir, "new.txt"), []byte("new file"), 0o644); err != nil {
			t.Fatal(err)
		}
		eq(t, string(e.get("/new.txt").body), "new file")
	})
}

func TestRewrites(t *testing.T) {
	// legacy.html と BlueMap の書き換え (sub_filter の代わり)
	withEnv(t, none(), func(e *env) {
		legacy := e.get("/legacy.html")
		eq(t, legacy.status, 200)
		eq(t, legacy.h("Cache-Control"), "no-cache")
		want := strings.Replace(indexHTML, `<script type="text/javascript" src="standalone/config.js`,
			"<script type=\"text/javascript\" src=\"toro/dynmap-client.js?v=2\"></script>\n\t<script type=\"text/javascript\" src=\"standalone/config.js", 1)
		eq(t, string(legacy.body), want)

		settings := e.blue("/settings.json")
		eq(t, string(settings.body), strings.Replace(settingsJSON, `"scripts":[`, `"scripts":["toro-refresh.js?v=1",`, 1))
		eq(t, settings.h("Cache-Control"), "no-cache")
		ok(t, strings.Contains(settings.h("Content-Security-Policy"), "media-src 'self' https: blob:"))
		eq(t, settings.h("X-Content-Type-Options"), "nosniff")
		// 書き換えたあとの内容から ETag を作るので、書き換える前のものを持つブラウザには 200 で新しい内容を返す
		eq(t, e.blue("/settings.json", reqOpts{headers: hdr("if-none-match", `W/"original"`)}).status, 200)
		eq(t, e.blue("/settings.json", reqOpts{headers: hdr("if-none-match", settings.h("ETag"))}).status, 304)

		eq(t, string(e.blue("/lang/settings.conf").body), "default: \"ja\"\nother: \"en\"\n")

		refresh := e.blue("/toro-refresh.js")
		ok(t, strings.Contains(string(refresh.body), "sessionStorage.getItem('toro-refresh')"))
		eq(t, refresh.h("Cache-Control"), "no-cache")

		sign := e.blue("/marumasign/overworld.json")
		eq(t, sign.h("Cache-Control"), "no-cache")
		asset := e.blue("/assets/a.png")
		eq(t, asset.h("Cache-Control"), "public, max-age=86400, stale-if-error=604800")
		eq(t, asset.h("CDN-Cache-Control"), "max-age=60")
		// maps/ は静的に返さない (toro-webmap に聞く)
		e.mapweb.setHandler(notFound)
		eq(t, e.blue("/maps/hidden.json").status, 404)
		eq(t, e.mapweb.hitCount("/maps/hidden.json"), 1)
	})
}

func TestCacheCoalesce(t *testing.T) {
	// 同時に聞かない (1 回だけ元へ)、そのあとは元に聞かない
	withEnv(t, none(), func(e *env) {
		png := bytes.Repeat([]byte{7}, 2000)
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			send(w, 200, hdr("content-type", "image/png", "cache-control", "public, max-age=300", "etag", `"t1"`, "last-modified", "Wed, 21 Oct 2026 07:28:00 GMT"), png)
		})
		url := "/tiles/main/flat/0_0/zzz_0_0.png?timestamp=1"
		replies := make([]*reply, 20)
		var wg sync.WaitGroup
		for i := range replies {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := request(e.edge.port, url, reqOpts{})
				if err != nil {
					t.Errorf("request %d: %v", i, err)
					return
				}
				replies[i] = r
			}()
		}
		wg.Wait()
		if t.Failed() {
			t.FailNow()
		}
		eq(t, e.mapweb.hitCount(url), 1)
		miss, coalesced := false, false
		for _, r := range replies {
			eq(t, r.status, 200)
			sameBytes(t, r.body, png)
			eq(t, r.h("ETag"), `"t1"`)
			miss = miss || r.h("X-Edge-Cache") == "MISS"
			coalesced = coalesced || r.h("X-Edge-Cache") == "COALESCED"
		}
		ok(t, miss, "no MISS")
		ok(t, coalesced, "no COALESCED")
		again := e.get(url)
		eq(t, again.h("X-Edge-Cache"), "HIT")
		eq(t, again.h("Cache-Control"), "public, max-age=300")
		eq(t, e.mapweb.hitCount(url), 1)
		// 条件付きで聞かれたら、元に聞かず 304
		eq(t, e.get(url, reqOpts{headers: hdr("if-none-match", `"t1"`)}).status, 304)
		eq(t, e.get(url, reqOpts{headers: hdr("if-modified-since", "Wed, 21 Oct 2026 07:28:00 GMT")}).status, 304)
		eq(t, e.mapweb.hitCount(url), 1)
		// 元へは Accept-Encoding: identity と、客の Host、X-Forwarded-For だけ。Cookie などは渡さない
		seen := e.mapweb.seenAt(0).Header
		eq(t, seen.Get("Accept-Encoding"), "identity")
		eq(t, seen.Get("Host"), "map.torosaba.net")
		ok(t, strings.Contains(seen.Get("X-Forwarded-For"), "127.0.0.1"), seen.Get("X-Forwarded-For"))
	})
}

func TestCacheStale(t *testing.T) {
	// 古くなったら古いまま返して裏で確かめる (304 で延ばす)。元が壊れていても古いものを返す
	withEnv(t, none(), func(e *env) {
		var down atomic.Bool
		e.mapweb.setHandler(func(w http.ResponseWriter, r *http.Request) {
			if down.Load() {
				e.mapweb.destroy(w)
				return
			}
			if r.Header.Get("If-None-Match") == `"w1"` {
				send(w, 304, hdr("etag", `"w1"`), nil)
				return
			}
			sendText(w, 200, hdr("content-type", "application/json", "cache-control", "no-cache", "etag", `"w1"`), `{"timestamp":1}`)
		})
		url := "/up/world/main/123"
		// 元には、鍵から作った URL で聞く
		upstream := "/up/world/main/0"
		eq(t, e.get(url).h("X-Edge-Cache"), "MISS")
		eq(t, e.mapweb.hitCount(upstream), 1)
		// 別の ts でも、同じワールドは同じもの
		eq(t, e.get("/up/world/main/999").h("X-Edge-Cache"), "HIT")
		eq(t, e.mapweb.hitCount(upstream), 1)

		time.Sleep(1100 * time.Millisecond)
		stale := e.get(url)
		eq(t, stale.h("X-Edge-Cache"), "STALE")
		eq(t, string(stale.body), `{"timestamp":1}`)
		time.Sleep(150 * time.Millisecond)
		eq(t, e.mapweb.hitCount(upstream), 2)
		eq(t, e.mapweb.seenAt(-1).Header.Get("If-None-Match"), `"w1"`)
		// 304 で新しくなった
		eq(t, e.get(url).h("X-Edge-Cache"), "HIT")

		// 古さの限りを超えて、元が落ちている: 古いものを返す
		time.Sleep(3200 * time.Millisecond)
		down.Store(true)
		errReply := e.get(url)
		eq(t, errReply.status, 200)
		eq(t, errReply.h("X-Edge-Cache"), "ERROR-STALE")
		eq(t, string(errReply.body), `{"timestamp":1}`)
		// 初めて聞くものが落ちている元に当たったら 502
		eq(t, e.get("/up/world/other/1").status, 502)
	})
}

func TestUpWorldShape(t *testing.T) {
	// /up/world/<ワールド>/<数字> だけ。元へ渡すのは鍵から作った URL で、先頭の客のパスやクエリは持ち込まない
	withEnv(t, none(), func(e *env) {
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			sendText(w, 200, hdr("content-type", "application/json"), `{"timestamp":1}`)
		})
		for _, bad := range []string{"/up/world/main", "/up/world/main/", "/up/world/main/x", "/up/world/main/1.json", "/up/world/main/1/extra", "/up/world//1"} {
			eq(t, e.get(bad).status, 404, bad)
		}
		eq(t, e.mapweb.seenLen(), 0)
		eq(t, e.get("/up/world/main/5?x=1&y=2").status, 200)
		eq(t, e.mapweb.seenAt(0).URL, "/up/world/main/0")
		// /up/configuration もクエリを渡さない
		eq(t, e.get("/up/configuration?x=1").status, 200)
		eq(t, e.mapweb.hitCount("/up/configuration"), 1)
		// 知らないワールドの 404 も少しのあいだ覚える
		e.mapweb.setHandler(notFound)
		eq(t, e.get("/up/world/nothing/1").status, 404)
		eq(t, e.get("/up/world/nothing/2").status, 404)
		eq(t, e.mapweb.hitCount("/up/world/nothing/0"), 1)
	})
}

func TestQueryIsNotPartOfTheKey(t *testing.T) {
	// 中身がクエリで変わらないものは、クエリを変えても 1 つのものとしてためる
	withEnv(t, none(), func(e *env) {
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			sendText(w, 200, hdr("content-type", "application/octet-stream", "cache-control", "public, max-age=300"), "x")
		})
		for _, c := range []struct{ host, path string }{
			{"map.torosaba.net", "/_nuxt/a.js"},
			{"map.torosaba.net", "/pwa/a.png"},
			{"map.torosaba.net", "/toro/a.js"},
			{"map.torosaba.net", "/"},
			{"3d-map.torosaba.net", "/_nuxt/b.js"},
			{"3d-map.torosaba.net", "/pwa/b.png"},
			{"3d-map.torosaba.net", "/mcassets/0123456789abcdef0123/textures.bin"},
			{"3d-map.torosaba.net", "/"},
		} {
			before := e.mapweb.hitCount(c.path)
			for i := 0; i < 3; i++ {
				eq(t, e.req(e.edge.port, c.host, c.path+"?v="+strconv.Itoa(i), nil).status, 200, c.path)
			}
			eq(t, e.mapweb.hitCount(c.path)-before, 1, c.host+c.path)
		}
		entries, _ := e.cacheStats()
		eq(t, entries, 8)

		// /tiles/ は timestamp だけを見る (描き直したタイルは別のもの)
		tile := "/tiles/main/flat/0_0/a.png"
		e.get(tile + "?x=1")
		e.get(tile + "?x=2")
		eq(t, e.mapweb.hitCount(tile), 1)
		e.get(tile + "?timestamp=5&x=1")
		e.get(tile + "?x=2&timestamp=5")
		eq(t, e.mapweb.hitCount(tile+"?timestamp=5"), 1)
		e.get(tile + "?timestamp=6")
		eq(t, e.mapweb.hitCount(tile+"?timestamp=6"), 1)

		// /mcassets/skin は name と uuid だけを見る
		e.blue("/mcassets/skin?name=Steve&x=1")
		e.blue("/mcassets/skin?x=2&name=Steve")
		eq(t, e.mapweb.hitCount("/mcassets/skin?name=Steve"), 1)
		e.blue("/mcassets/skin?name=Alex")
		eq(t, e.mapweb.hitCount("/mcassets/skin?name=Alex"), 1)
	})
}

func TestLongKeyIsNotStored(t *testing.T) {
	// 長い鍵はためずに流す
	withEnv(t, none(), func(e *env) {
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			sendText(w, 200, hdr("content-type", "image/png", "cache-control", "public, max-age=300"), "x")
		})
		url := "/tiles/" + strings.Repeat("a", 2000) + ".png"
		for i := 0; i < 2; i++ {
			r := e.get(url)
			eq(t, r.status, 200)
			eq(t, r.h("X-Edge-Cache"), "BYPASS")
		}
		eq(t, e.mapweb.hitCount(url), 2)
		entries, _ := e.cacheStats()
		eq(t, entries, 0)
	})
}

func TestEncodedDelimitersAreRefused(t *testing.T) {
	// エンコードしたドット・スラッシュ・バックスラッシュのあるパスは、どの経路でも元に渡さない
	withEnv(t, none(), func(e *env) {
		for _, path := range []string{"/maps/overworld/live/%2e%2e/x", "/maps/overworld/live/a%2Fb", "/maps/a%5cb/live/players.json", "/tiles/a%2e%2e/b.png", "/_nuxt/a%2fb.js", "/_marumasign/a%2eb", "/mcassets/a%2fb"} {
			eq(t, e.blue(path).status, 404, path)
			eq(t, e.get(path).status, 404, path)
		}
		eq(t, e.mapweb.seenLen(), 0)
		eq(t, e.live.seenLen(), 0)
	})
}

func TestBlueMapLiveNames(t *testing.T) {
	// live/ は実際に使う名前と settings.json にある地図だけ。ほかは toro-main に聞かない
	withEnv(t, none(), func(e *env) {
		e.live.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			sendText(w, 200, hdr("content-type", "application/json"), `{"players":[]}`)
		})
		for _, path := range []string{"/maps/overworld/live/", "/maps/overworld/live/secret.txt", "/maps/overworld/live/players.json/x", "/maps/nothing/live/players.json", "/maps/nothing/live/sse", "/maps/nothing/live/markers.json"} {
			eq(t, e.blue(path).status, 404, path)
		}
		eq(t, e.live.seenLen(), 0)
		eq(t, e.mapweb.seenLen(), 0)
		eq(t, e.blue("/maps/overworld/live/players.json").status, 200)
		eq(t, e.live.seenLen(), 1)
		// settings.json が読めるあいだは、書き換えたら新しい地図も通る
		put(t, e.blueDir, "settings.json", []byte(`{"maps":["overworld","nothing"]}`))
		time.Sleep(1200 * time.Millisecond)
		e.blue("/maps/overworld/live/players.json")
		time.Sleep(100 * time.Millisecond)
		eq(t, e.blue("/maps/nothing/live/players.json").status, 200)
		// 読めないときは止めない
		if err := os.Remove(filepath.Join(e.blueDir, "settings.json")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1200 * time.Millisecond)
		e.blue("/maps/overworld/live/players.json")
		time.Sleep(100 * time.Millisecond)
		eq(t, e.blue("/maps/third/live/players.json").status, 200)
	})
}

// endlessBody は終わらない返事を流す元
func endlessBody(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "video/mp4")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	for {
		if _, err := w.Write(bytes.Repeat([]byte{1}, 1024)); err != nil {
			return
		}
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// hold は返事の頭書きが届くまで待って、つなぎを開いたまま返す
func hold(t *testing.T, e *env, path, host, ip string) (net.Conn, int) {
	t.Helper()
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(e.edge.port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, raw := buildRequest(path, reqOpts{host: host, headers: hdr("cf-connecting-ip", ip, "origin", "https://"+host)}, true)
	conn.Write(raw)
	res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("%s from %s: %v", path, ip, err)
	}
	conn.SetDeadline(time.Time{})
	return conn, res.StatusCode
}

func TestStreamLimitPerClient(t *testing.T) {
	// 1 つの客 (IPv6 は /64 ごと) が同時に流せる数には上限がある。ほかの客と、ためる経路には響かない
	withEnv(t, none(), func(e *env) {
		e.mapweb.setHandler(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.RequestURI, "/_marumasign/") {
				endlessBody(w, r)
				return
			}
			sendText(w, 200, hdr("content-type", "image/png", "cache-control", "public, max-age=300"), "x")
		})
		const path, host = "/_marumasign/proxy?u=https%3A%2F%2Fexample.com%2Fa.mp4", "3d-map.torosaba.net"
		var first net.Conn
		for i := 0; i < 64; i++ {
			// 下の 64 ビットが違っても同じ客
			conn, status := hold(t, e, path, host, "2001:db8:1:2::"+strconv.FormatInt(int64(i+1), 16))
			eq(t, status, 200, i)
			if i == 0 {
				first = conn
			}
		}
		_, status := hold(t, e, path, host, "2001:db8:1:2:ffff::1")
		eq(t, status, 429)
		_, status = hold(t, e, path, host, "2001:db8:1:3::1")
		eq(t, status, 200, "another /64")
		eq(t, e.get("/tiles/main/flat/0_0/a.png").status, 200)
		// 切れたら空く
		first.Close()
		for i := 0; ; i++ {
			_, status = hold(t, e, path, host, "2001:db8:1:2:ffff::1")
			if status == 200 || i > 100 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		eq(t, status, 200, "after a stream ended")
		// Cloudflare を通っていないもの (Docker の中) は数えない
		conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(e.edge.port))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, raw := buildRequest(path, reqOpts{host: host, headers: hdr("origin", "https://"+host)}, true)
		conn.Write(raw)
		res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "GET"})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, res.StatusCode, 200)
	})
}

func TestSSEDoesNotBlockCachedFetches(t *testing.T) {
	// SSE は客ごとに少しだけ。流しているものが接続の上限まで埋まっても、ためる経路 (live/players.json) は待たされない
	withEnv(t, none(), func(e *env) {
		var closed atomic.Bool
		sse := sseUpstream(&closed)
		e.live.setHandler(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.RequestURI, "/sse") {
				sse(w, r)
				return
			}
			sendText(w, 200, hdr("content-type", "application/json"), `{"players":[]}`)
		})
		const path, host = "/maps/overworld/live/sse", "3d-map.torosaba.net"
		for i := 0; i < 8; i++ {
			_, status := hold(t, e, path, host, "203.0.113.1")
			eq(t, status, 200, i)
		}
		_, status := hold(t, e, path, host, "203.0.113.1")
		eq(t, status, 429)
		// 元への接続の上限 (32) まで埋める
		for i := 2; i <= 4; i++ {
			for j := 0; j < 8; j++ {
				_, status := hold(t, e, path, host, "203.0.113."+strconv.Itoa(i))
				eq(t, status, 200, i)
			}
		}
		t0 := time.Now()
		r := e.blue("/maps/overworld/live/players.json")
		eq(t, r.status, 200)
		eq(t, string(r.body), `{"players":[]}`)
		ok(t, time.Since(t0) < 2*time.Second, "cached fetch waited ", time.Since(t0))
	})
}

func TestWebSocketLimitPerClient(t *testing.T) {
	// 1 つの客がつなげる WebSocket の数には上限がある
	withEnv(t, none(), func(e *env) {
		e.mapweb.setHandler(e.mapweb.echoUpgrade)
		open := func(ip string) int {
			conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(e.edge.port))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			fmt.Fprintf(conn, "GET /up/ws HTTP/1.1\r\nHost: map.torosaba.net\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nCf-Connecting-Ip: %s\r\n\r\n", ip)
			res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "GET"})
			if err != nil {
				t.Fatal(err)
			}
			return res.StatusCode
		}
		for i := 0; i < 16; i++ {
			eq(t, open("203.0.113.1"), 101, i)
		}
		eq(t, open("203.0.113.1"), 429)
		eq(t, open("203.0.113.2"), 101, "another client")
	})
}

func TestCache5xx(t *testing.T) {
	// 5xx は、古いものがなければそのまま返し、ためない
	withEnv(t, none(), func(e *env) {
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			sendText(w, 503, hdr("content-type", "text/plain"), "Database unavailable")
		})
		a := e.get("/up/configuration")
		eq(t, a.status, 503)
		eq(t, string(a.body), "Database unavailable")
		e.get("/up/configuration")
		eq(t, e.mapweb.hitCount("/up/configuration"), 2)
	})
}

func markersJSON(n int) string {
	var b strings.Builder
	b.WriteString(`{"sets":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"marker-%d","label":"station","x":%d,"y":64,"z":%d}`, i, i, -i)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestCompress(t *testing.T) {
	// 動的な JSON は 1 度だけ圧縮して使い回す。Vary を付ける
	withEnv(t, none(), func(e *env) {
		json := markersJSON(400)
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			sendText(w, 200, hdr("content-type", "application/json", "cache-control", "no-cache"), json)
		})
		url := "/up/configuration"
		br := e.get(url, reqOpts{headers: hdr("accept-encoding", "br, gzip")})
		eq(t, br.h("Content-Encoding"), "br")
		eq(t, strings.ToLower(br.h("Vary")), "accept-encoding")
		eq(t, unbr(t, br.body), json)
		g := e.get(url, reqOpts{headers: hdr("accept-encoding", "gzip")})
		eq(t, ungz(t, g.body), json)
		plain := e.get(url)
		eq(t, string(plain.body), json)
		ok(t, !plain.has("Content-Encoding"))
		eq(t, e.mapweb.hitCount(url), 1)
		// 条件付き
		eq(t, e.get(url, reqOpts{headers: hdr("accept-encoding", "br", "if-none-match", br.h("ETag"))}).status, 304)
	})
}

func TestUpstreamAlreadyCompressed(t *testing.T) {
	// 元がすでに圧縮しているもの (BlueMap のタイル) はそのまま返す
	withEnv(t, none(), func(e *env) {
		body := gz(bytes.Repeat([]byte{3}, 5000))
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			send(w, 200, hdr("content-type", "application/octet-stream", "content-encoding", "gzip", "cache-control", "public, max-age=86400, stale-if-error=604800", "etag", `"p1"`, "access-control-allow-origin", "*"), body)
		})
		url := "/maps/overworld/tiles/0/x0/z0.prbm"
		for i := 0; i < 2; i++ {
			r := e.blue(url, reqOpts{headers: hdr("accept-encoding", "gzip, br")})
			eq(t, r.status, 200)
			eq(t, r.h("Content-Encoding"), "gzip")
			sameBytes(t, r.body, body)
			eq(t, r.h("Cache-Control"), "public, max-age=86400, stale-if-error=604800")
			eq(t, r.h("CDN-Cache-Control"), "max-age=60")
			ok(t, !r.has("Access-Control-Allow-Origin"))
			ok(t, r.has("Content-Security-Policy"))
		}
		eq(t, e.mapweb.hitCount(url), 1)
		// クエリは鍵に入れない (BlueMap は付けることがある)
		e.blue(url + "?cachebust=1")
		eq(t, e.mapweb.hitCount(url), 1)
		// 描いていないタイルは 204
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			send(w, 204, hdr("cache-control", "public, max-age=60"), nil)
		})
		eq(t, e.blue("/maps/overworld/tiles/0/x9/z9.prbm").status, 204)
		eq(t, e.blue("/maps/overworld/tiles/0/x9/z9.prbm").h("X-Edge-Cache"), "HIT")
	})
}

func TestBlueMapLive(t *testing.T) {
	// BlueMap 本体に聞き、1 秒まとめ、頭書きを付け替える。落ちていたら DB の版 (toro-webmap) へ
	withEnv(t, none(), func(e *env) {
		e.live.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			sendText(w, 200, hdr("content-type", "application/json", "cache-control", "max-age=86400", "content-security-policy", "default-src none", "x-content-type-options", "x"), `{"players":[]}`)
		})
		url := "/maps/overworld/live/players.json"
		r := e.blue(url)
		eq(t, r.status, 200)
		eq(t, r.h("Cache-Control"), "no-cache")
		ok(t, strings.HasPrefix(r.h("Content-Security-Policy"), "default-src 'self'"), r.h("Content-Security-Policy"))
		eq(t, r.h("X-Content-Type-Options"), "nosniff")
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := request(e.edge.port, url, reqOpts{host: "3d-map.torosaba.net"}); err != nil {
					t.Errorf("request: %v", err)
				}
			}()
		}
		wg.Wait()
		eq(t, e.live.hitCount(url), 1)
		eq(t, e.mapweb.hitCount(url), 0)
		eq(t, e.live.seenAt(0).Header.Get("Host"), "3d-map.torosaba.net")

		// マーカーは toro-webmap (Dynmap の地点を足したもの)
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			sendText(w, 200, hdr("content-type", "application/json", "cache-control", "no-cache", "access-control-allow-origin", "*"), `{"merged":true}`)
		})
		markers := e.blue("/maps/overworld/live/markers.json")
		eq(t, string(markers.body), `{"merged":true}`)
		ok(t, !markers.has("Access-Control-Allow-Origin"))
		eq(t, e.live.hitCount("/maps/overworld/live/markers.json"), 0)

		// BlueMap 本体が落ちている (toro-main の再起動中)
		e.live.stop()
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			sendText(w, 200, hdr("content-type", "application/json", "cache-control", "public, max-age=3600", "access-control-allow-origin", "*"), `{"players":["last-from-db"]}`)
		})
		// まだためていない (古いものもない) ものは、toro-webmap が DB から返す。ためてあるものは errorTtl のあいだ古い版を返す
		fallback := e.blue("/maps/other/live/players.json?x=1")
		eq(t, fallback.status, 200)
		eq(t, fallback.h("X-Edge-Cache"), "MISS")
		eq(t, string(fallback.body), `{"players":["last-from-db"]}`)
		eq(t, fallback.h("Cache-Control"), "no-cache")
		// Cloudflare に長くためさせない (nginx は 60 秒と付けていた)
		ok(t, !fallback.has("CDN-Cache-Control"))
	})
}

func TestAssets(t *testing.T) {
	// 内側の頭書きは客に書かせず、Cloudflare の IP から作る。Range は元が処理する。形が違うものは元に聞かない
	withEnv(t, none(), func(e *env) {
		e.mapweb.setHandler(func(w http.ResponseWriter, r *http.Request) {
			status := 200
			if r.Header.Get("Range") != "" {
				status = 206
			}
			sendText(w, status, hdr("content-type", "image/png", "cache-control", "private, max-age=31536000, immutable", "content-length", "4", "content-range", "bytes 0-3/10"), "abcd")
		})
		r := e.assets("/abcdefghijkl.png", reqOpts{headers: hdr(
			"x-toro-client-ip", "1.2.3.4", "cf-connecting-ip", "9.9.9.9", "cookie", "a=b", "range", "bytes=0-3",
			"origin", "https://map.torosaba.net", "referer", "https://map.torosaba.net/")})
		eq(t, r.status, 206)
		eq(t, string(r.body), "abcd")
		eq(t, r.h("Content-Range"), "bytes 0-3/10")
		seen := e.mapweb.seenAt(0)
		eq(t, seen.URL, "/_assets/abcdefghijkl.png")
		eq(t, seen.Header.Get("X-Toro-Client-Ip"), "9.9.9.9")
		eq(t, seen.Header.Get("Range"), "bytes=0-3")
		eq(t, seen.Header.Get("Origin"), "https://map.torosaba.net")
		ok(t, len(seen.Header.Values("Cookie")) == 0, "cookie was forwarded")

		// Cloudflare を通っていなければ、客が書いても渡さない
		e.assets("/abcdefghijkl.png", reqOpts{headers: hdr("x-toro-client-ip", "1.2.3.4")})
		ok(t, len(e.mapweb.seenAt(1).Header.Values("X-Toro-Client-Ip")) == 0, "client ip header was forwarded")

		n := e.mapweb.seenLen()
		for _, bad := range []string{"/abcdefghijk.png", "/ABCDEFGHIJKL.png", "/abcdefghijkl", "/abcdefghijkl.png/x", "/_assets/abcdefghijkl.png", "/"} {
			eq(t, e.assets(bad).status, 404, bad)
		}
		eq(t, e.mapweb.seenLen(), n)
	})
}

// readUntilFirstBody は、頭書きを読み、本文の最初の塊が届いたら返す (そのあと呼び出し側がつなぎを切る)
func readUntilFirstBody(t *testing.T, conn net.Conn, br *bufio.Reader, method string) (res *http.Response, chunk string) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	res, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		t.Fatalf("no response within 3s: %v", err)
	}
	buf := make([]byte, 4096)
	n, err := res.Body.Read(buf)
	if n == 0 {
		t.Fatalf("no data within 3s: %v", err)
	}
	return res, string(buf[:n])
}

func waitClosed(closed *atomic.Bool) {
	for i := 0; i < 50 && !closed.Load(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStreamClientDisconnect(t *testing.T) {
	// 客が切れたら元も切る / ためない
	withEnv(t, none(), func(e *env) {
		var closed atomic.Bool
		e.mapweb.setHandler(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			// 終わらせない返事なので、閉じるのは、つなぎが切れたとき
			for {
				select {
				case <-r.Context().Done():
					closed.Store(true)
					return
				case <-tick.C:
					if _, err := w.Write(bytes.Repeat([]byte{1}, 1024)); err != nil {
						closed.Store(true)
						return
					}
					fl.Flush()
				}
			}
		})
		conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(e.edge.port))
		if err != nil {
			t.Fatal(err)
		}
		_, raw := buildRequest("/_marumasign/proxy?u=https%3A%2F%2Fexample.com%2Fa.mp4", reqOpts{host: "3d-map.torosaba.net", headers: hdr("origin", "https://3d-map.torosaba.net", "range", "bytes=0-")}, true)
		conn.Write(raw)
		readUntilFirstBody(t, conn, bufio.NewReader(conn), "GET")
		conn.Close()
		waitClosed(&closed)
		ok(t, closed.Load(), "upstream was not closed")
		eq(t, e.mapweb.seenAt(0).Header.Get("Range"), "bytes=0-")
		eq(t, e.mapweb.seenAt(0).Header.Get("Origin"), "https://3d-map.torosaba.net")
	})
}

func TestWebSocket(t *testing.T) {
	// map.torosaba.net (Dynmap) と 3d-map.torosaba.net (BlueMap) の /up/ws だけ、つないだあとはそのまま渡す
	withEnv(t, none(), func(e *env) {
		e.mapweb.setHandler(e.mapweb.echoUpgrade)
		for i, host := range []string{"map.torosaba.net", "3d-map.torosaba.net"} {
			conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(e.edge.port))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			fmt.Fprintf(conn, "GET /up/ws HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nOrigin: https://%s\r\n\r\n", host, host)
			br := bufio.NewReader(conn)
			res, err := http.ReadResponse(br, &http.Request{Method: "GET"})
			if err != nil {
				t.Fatal(err)
			}
			eq(t, res.StatusCode, 101, host)
			msg := "hello through the proxy"
			conn.Write([]byte(msg))
			echoed := make([]byte, len(msg))
			if _, err := io.ReadFull(br, echoed); err != nil {
				t.Fatal(err)
			}
			eq(t, string(echoed), msg, host)
			conn.Close()
			seen := e.mapweb.seenAt(i)
			eq(t, seen.URL, "/up/ws", host)
			eq(t, seen.Header.Get("Sec-WebSocket-Key"), "dGhlIHNhbXBsZSBub25jZQ==", host)
			eq(t, seen.Header.Get("Host"), host)
		}

		// /up/ws 以外や、assets への Upgrade は断る
		for _, c := range [][2]string{{"/up/other", "map.torosaba.net"}, {"/up/other", "3d-map.torosaba.net"}, {"/up/ws", "assets.torosaba.net"}, {"/live/plugin", "map.torosaba.net"}, {"/live/plugin", "3d-map.torosaba.net"}} {
			r, err := request(e.edge.port, c[0], reqOpts{host: c[1], headers: hdr("connection", "Upgrade", "upgrade", "websocket")})
			if err != nil {
				t.Fatalf("%s%s: %v", c[1], c[0], err)
			}
			eq(t, r.status, 404, c[1]+c[0])
		}
		// WebSocket でない GET は 426
		eq(t, e.get("/up/ws").status, 426)
	})
}

func TestMethodsAndURL(t *testing.T) {
	// GET と HEAD だけ、長すぎる URL は断る
	withEnv(t, none(), func(e *env) {
		post := e.get("/css/style.css", reqOpts{method: "POST"})
		eq(t, post.status, 405)
		eq(t, post.h("Allow"), "GET, HEAD")
		eq(t, e.get("/live/clients", reqOpts{method: "POST"}).status, 405)
		eq(t, e.get("/"+strings.Repeat("a", 9000)).status, 414)
		// 内側のための /live/ は、外から GET しても toro-webmap に届かない (map.torosaba.net 側は静的なファイルとして探して 404)
		eq(t, e.get("/live/plugin").status, 404)
		eq(t, e.mapweb.seenLen(), 0)
	})
}

func TestCacheEviction(t *testing.T) {
	// 上限を超えたら、使われていないものから捨てる / 大きすぎるものはためずに流す
	withEnv(t, overrides{cacheBytes: 200 * 1024, maxEntityBytes: 25 * 1024}, func(e *env) {
		body := bytes.Repeat([]byte{5}, 20_000)
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			send(w, 200, hdr("content-type", "image/png", "cache-control", "public, max-age=300", "etag", `"x"`), body)
		})
		for i := 0; i < 12; i++ {
			e.get(fmt.Sprintf("/tiles/w/p/0_0/%d.png", i))
		}
		entries, size := e.cacheStats()
		ok(t, size <= 200*1024, "bytes=", size)
		ok(t, entries < 12, "entries=", entries)
		ok(t, entries > 0, "entries=", entries)

		// maxEntityBytes (上限の 1/8 = 25KB) より大きいもの
		big := bytes.Repeat([]byte{9}, 100_000)
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			send(w, 200, hdr("content-type", "image/png", "cache-control", "public, max-age=300"), big)
		})
		a := e.get("/tiles/w/p/0_0/big.png")
		sameBytes(t, a.body, big)
		eq(t, a.h("X-Edge-Cache"), "BYPASS")
		e.get("/tiles/w/p/0_0/big.png")
		ok(t, e.mapweb.hitCount("/tiles/w/p/0_0/big.png") >= 2)
	})
}

func TestHeadersPerSite(t *testing.T) {
	// Dynmap 側は / を no-cache + ja、BlueMap 側は CSP を付ける
	withEnv(t, none(), func(e *env) {
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			sendText(w, 200, hdr("content-type", "text/html; charset=utf-8", "cache-control", "public, max-age=300", "etag", `"h1"`, "x-powered-by", "nitro"), "<html></html>")
		})
		m := e.get("/")
		eq(t, m.h("Cache-Control"), "no-cache")
		eq(t, m.h("Content-Language"), "ja")
		ok(t, !m.has("Content-Security-Policy"))
		ok(t, !m.has("X-Powered-By"))
		b := e.blue("/")
		eq(t, b.h("Cache-Control"), "no-cache")
		eq(t, b.h("Content-Language"), "ja")
		ok(t, strings.Contains(b.h("Content-Security-Policy"), "frame-ancestors 'self'"))
		// 同じパスでも、site ごとに別のものをためる
		eq(t, e.mapweb.hitCount("/"), 2)
		eq(t, e.blue("/_nuxt/entry.js").status, 200)
	})
}

// sseUpstream は、終わらない text/event-stream を返す元
func sseUpstream(closed *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		for k, v := range hdr("content-type", "text/event-stream", "cache-control", "no-store", "cdn-cache-control", "no-store",
			"cloudflare-cdn-cache-control", "no-store", "content-security-policy", "default-src none", "x-accel-buffering", "no") {
			h.Set(k, v)
		}
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		w.Write([]byte("event: hello\ndata: 1\n\n"))
		fl.Flush()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				closed.Store(true)
				return
			case <-tick.C:
				if _, err := w.Write([]byte(": ping\n\n")); err != nil {
					closed.Store(true)
					return
				}
				fl.Flush()
			}
		}
	}
}

// firstChunk は、最初のチャンクが届いたら、頭書きと一緒に返して切る
func firstChunk(t *testing.T, port int, path, host string, headers map[string]string) (res *http.Response, chunk string, elapsed time.Duration) {
	t.Helper()
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	t0 := time.Now()
	_, raw := buildRequest(path, reqOpts{host: host, headers: headers}, true)
	conn.Write(raw)
	res, chunk = readUntilFirstBody(t, conn, bufio.NewReader(conn), "GET")
	return res, chunk, time.Since(t0)
}

func TestSSE(t *testing.T) {
	// BlueMap の live/sse: ためずにすぐ流す。頭書きを付け替え、客が切れたら元も切る
	withEnv(t, none(), func(e *env) {
		var closed atomic.Bool
		e.live.setHandler(sseUpstream(&closed))
		r, chunk, ms := firstChunk(t, e.edge.port, "/maps/overworld/live/sse", "3d-map.torosaba.net",
			hdr("accept", "text/event-stream", "last-event-id", "41", "cookie", "a=b"))
		eq(t, r.StatusCode, 200)
		eq(t, chunk, "event: hello\ndata: 1\n\n")
		ok(t, ms < time.Second, "first chunk took ", ms)
		// EventSource の頭書きは元に渡す。Cookie は渡さない
		seen := e.live.seenAt(0).Header
		eq(t, seen.Get("Accept"), "text/event-stream")
		eq(t, seen.Get("Last-Event-Id"), "41")
		ok(t, len(seen.Values("Cookie")) == 0, "cookie was forwarded")
		eq(t, r.Header.Get("Content-Type"), "text/event-stream")
		ok(t, r.ContentLength == -1, "content-length was set")
		eq(t, r.Header.Get("Cache-Control"), "no-cache")
		ok(t, len(r.Header.Values("CDN-Cache-Control")) == 0)
		eq(t, r.Header.Get("Cloudflare-CDN-Cache-Control"), "no-store")
		ok(t, strings.Contains(r.Header.Get("Content-Security-Policy"), "media-src"))
		waitClosed(&closed)
		ok(t, closed.Load(), "upstream was not closed")
	})
}

func TestSSESafetyNet(t *testing.T) {
	// 安全網: ためる経路に SSE が来ても、待ち続けずに流す
	withEnv(t, none(), func(e *env) {
		var closed atomic.Bool
		e.mapweb.setHandler(sseUpstream(&closed))
		r, chunk, _ := firstChunk(t, e.edge.port, "/tiles/main/stream", "map.torosaba.net", nil)
		eq(t, r.StatusCode, 200)
		eq(t, chunk, "event: hello\ndata: 1\n\n")
		eq(t, r.Header.Get("X-Edge-Cache"), "BYPASS")
		waitClosed(&closed)
		ok(t, closed.Load(), "upstream was not closed")
		// 流しているだけで、ためてはいない
		entries, _ := e.cacheStats()
		eq(t, entries, 0)
	})
}

func TestErrorReplies(t *testing.T) {
	// 404 に Cache-Control を付けない。付け足す頭書きは成功のときだけ
	withEnv(t, none(), func(e *env) {
		nf := e.get("/missing.txt")
		eq(t, nf.status, 404)
		ok(t, !nf.has("Cache-Control"))
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			sendText(w, 404, hdr("content-type", "application/json", "content-security-policy", "script-src 'none'"), `{"statusCode":404}`)
		})
		miss := e.blue("/_nuxt/old-chunk.js")
		eq(t, miss.status, 404)
		// BlueMap の CSP に置き換えず、toro-webmap が返したままにする
		eq(t, miss.h("Content-Security-Policy"), "script-src 'none'")
		// 404 は少しのあいだだけ覚える (古い画面が消えたファイルを何度も聞きに来る)
		e.blue("/_nuxt/old-chunk.js")
		eq(t, e.mapweb.hitCount("/_nuxt/old-chunk.js"), 1)
	})
}

// checkBigBodies は、大きい本文 (sendfile の対象) を、いろいろな形で何度取っても、同じ中身で返ること
func checkBigBodies(t *testing.T, e *env) {
	json := markersJSON(6000)
	ok(t, len(json) > 200_000, len(json))
	tile := randomBytes(300_000)
	e.mapweb.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/up/configuration" {
			sendText(w, 200, hdr("content-type", "application/json", "cache-control", "no-cache"), json)
			return
		}
		send(w, 200, hdr("content-type", "application/octet-stream", "cache-control", "public, max-age=300", "etag", `"t1"`), tile)
	})
	tilePath := "/maps/overworld/tiles/0/x0/z0.prbm"
	for i := 0; i < 3; i++ {
		br := e.get("/up/configuration", reqOpts{headers: hdr("accept-encoding", "br, gzip")})
		eq(t, br.h("Content-Encoding"), "br")
		eq(t, br.h("Content-Length"), strconv.Itoa(len(br.body)))
		eq(t, unbr(t, br.body), json)
		g := e.get("/up/configuration", reqOpts{headers: hdr("accept-encoding", "gzip")})
		eq(t, g.h("Content-Encoding"), "gzip")
		eq(t, ungz(t, g.body), json)
		plain := e.get("/up/configuration")
		ok(t, !plain.has("Content-Encoding"))
		eq(t, string(plain.body), json)
		tr := e.blue(tilePath, reqOpts{headers: hdr("accept-encoding", "gzip")})
		eq(t, tr.h("Content-Length"), strconv.Itoa(len(tile)))
		sameBytes(t, tr.body, tile)
	}
	eq(t, e.mapweb.hitCount("/up/configuration"), 1)
	eq(t, e.mapweb.hitCount(tilePath), 1)

	// HEAD は頭書きだけ。条件付きは 304
	head := e.blue(tilePath, reqOpts{method: "HEAD"})
	eq(t, head.status, 200)
	eq(t, head.h("Content-Length"), strconv.Itoa(len(tile)))
	eq(t, len(head.body), 0)
	eq(t, e.blue(tilePath, reqOpts{headers: hdr("if-none-match", `"t1"`)}).status, 304)

	// 大きい返事のあとも、同じつなぎで次の返事が正しく届く (框組みが崩れていない)
	seq := sequence(t, e.edge.port, "3d-map.torosaba.net", []seqItem{
		{path: tilePath},
		{path: "/assets/a.png"},
		{path: tilePath, method: "HEAD"},
		{path: tilePath},
		{path: tilePath, headers: hdr("if-none-match", `"t1"`)},
		{path: tilePath},
	})
	sameBytes(t, seq[0].body, tile, "1st")
	sameBytes(t, seq[1].body, []byte{1, 2, 3}, "asset")
	eq(t, len(seq[2].body), 0, "HEAD")
	sameBytes(t, seq[3].body, tile, "2nd")
	eq(t, seq[4].status, 304)
	sameBytes(t, seq[5].body, tile, "3rd")
}

func TestSendfile(t *testing.T) {
	// 大きい本文 (そのまま・gzip・brotli) を、何度でも・HEAD や 304 や続きのリクエストと混ぜても、正しく返す
	withEnv(t, none(), func(e *env) { checkBigBodies(t, e) })
}

func TestSendfileDisabled(t *testing.T) {
	// sendfile を使わない設定 (EDGE_SENDFILE_MIN=0) でも、同じ返事になる
	zero := 0
	withEnv(t, overrides{sendfileMin: &zero}, func(e *env) { checkBigBodies(t, e) })
}

func TestSendfileSlowReader(t *testing.T) {
	// 読むのが遅い客にも、全部を正しく送る (書けるまで待つ)
	withEnv(t, overrides{maxEntityBytes: 32 << 20}, func(e *env) {
		big := randomBytes(20_000_000)
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			send(w, 200, hdr("content-type", "application/octet-stream", "cache-control", "public, max-age=300"), big)
		})
		url := "/maps/overworld/tiles/0/x1/z1.prbm"
		sameBytes(t, e.blue(url).body, big, "first")
		conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(e.edge.port))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(60 * time.Second))
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: 3d-map.torosaba.net\r\nConnection: close\r\n\r\n", url)
		// 読まずに待つ (送る側の送信用のバッファと、こちらの受信用のバッファが、いっぱいになる)
		time.Sleep(600 * time.Millisecond)
		all, err := io.ReadAll(conn)
		if err != nil {
			t.Fatal(err)
		}
		split := bytes.Index(all, []byte("\r\n\r\n"))
		ok(t, split > 0, "no header end")
		ok(t, strings.HasPrefix(string(all[:split]), "HTTP/1.1 200"), string(all[:20]))
		sameBytes(t, all[split+4:], big, "slow reader")
	})
}

func TestSendfileConcurrent(t *testing.T) {
	// 送っている最中に別の客が来ても、捨てられても、どの返事も壊れない
	withEnv(t, overrides{cacheBytes: 1 << 20, maxEntityBytes: 512 << 10}, func(e *env) {
		bodies := make([][]byte, 10)
		for i := range bodies {
			bodies[i] = randomBytes(300_000)
		}
		e.mapweb.setHandler(func(w http.ResponseWriter, r *http.Request) {
			i := 0
			if s := r.RequestURI; strings.Contains(s, "/z") {
				i, _ = strconv.Atoi(strings.TrimSuffix(s[strings.LastIndex(s, "/z")+2:], ".prbm"))
			}
			send(w, 200, hdr("content-type", "application/octet-stream", "cache-control", "public, max-age=300"), bodies[i])
		})
		// 上限 (1MB) を何度も超える。同時に、いろいろなものを取る
		var wg sync.WaitGroup
		for round := 0; round < 6; round++ {
			for i := range bodies {
				k := (i*7 + round*3) % len(bodies)
				wg.Add(1)
				go func() {
					defer wg.Done()
					r, err := request(e.edge.port, fmt.Sprintf("/maps/overworld/tiles/0/x0/z%d.prbm", k), reqOpts{host: "3d-map.torosaba.net"})
					if err != nil {
						t.Errorf("tile %d: %v", k, err)
						return
					}
					if r.status != 200 || !bytes.Equal(r.body, bodies[k]) {
						t.Errorf("tile %d: status %d, body differs (%d bytes)", k, r.status, len(r.body))
					}
				}()
			}
			if round%2 == 1 {
				wg.Wait()
			}
		}
		wg.Wait()
		_, size := e.cacheStats()
		ok(t, size <= 1<<20, "bytes=", size)
	})
}

func TestAdminPort(t *testing.T) {
	// /healthz /stats /purge
	withEnv(t, none(), func(e *env) {
		eq(t, e.admin("/healthz", "GET").status, 200)
		e.cacheStats()
		e.mapweb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			sendText(w, 200, hdr("content-type", "image/png", "cache-control", "public, max-age=300"), "x")
		})
		e.get("/tiles/a/b/0_0/1.png")
		entries, _ := e.cacheStats()
		eq(t, entries, 1)
		eq(t, e.admin("/purge", "POST").status, 200)
		entries, _ = e.cacheStats()
		eq(t, entries, 0)
		// 公開のポートには出ない
		eq(t, e.get("/healthz").status, 404)
		eq(t, e.get("/stats").status, 404)
	})
}

package main

import "testing"

func TestAcceptedEncodings(t *testing.T) {
	cases := map[string]int{
		"gzip, deflate, br": encBr | encGzip,
		"gzip;q=0, br":      encBr,
		"identity":          0,
		"":                  0,
	}
	for in, want := range cases {
		if got := acceptedEncodings(in); got != want {
			t.Errorf("acceptedEncodings(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestHostName(t *testing.T) {
	if got := hostName("Map.TOROSABA.net:8080"); got != "map.torosaba.net" {
		t.Errorf("got %q", got)
	}
	if got := hostName("[::1]:80"); got != "[::1]" {
		t.Errorf("got %q", got)
	}
}

func TestParseCacheControl(t *testing.T) {
	cc := parseCacheControl("public, max-age=300, stale-if-error=60")
	if cc.maxAge != 300 || cc.noStore {
		t.Errorf("got %+v", cc)
	}
	if !parseCacheControl("no-store").noStore {
		t.Error("no-store")
	}
	if !parseCacheControl("private, max-age=5").isPrivate {
		t.Error("private")
	}
	if parseCacheControl("").maxAge != -1 {
		t.Error("empty max-age must be -1")
	}
}

func TestVariantEtag(t *testing.T) {
	if got := variantEtag(`"abc"`, "br"); got != `"abc-br"` {
		t.Errorf("got %q", got)
	}
	if got := variantEtag(`W/"abc"`, "gz"); got != `W/"abc-gz"` {
		t.Errorf("got %q", got)
	}
}

// relativePath は外へ出るもの・隠しファイル・PHP を断る
func TestRelativePath(t *testing.T) {
	good := map[string]string{"/css/a.css": "css/a.css", "/a/b/": "a/b/index.html", "/": "index.html"}
	for in, want := range good {
		if got, ok := relativePath(in); !ok || got != want {
			t.Errorf("relativePath(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	bad := []string{"/a//b/./c", "/../x", "/a/../x", "/%2e%2e/x", "/a/%2e%2e/%2e%2e/x", "/..%2fx", "/a%5c..%5cb", "/a%00b", "/.env", "/a/.git/config", "/x.php", "/x.PHP", "/x.php5", "/%zz"}
	for _, in := range bad {
		if got, ok := relativePath(in); ok {
			t.Errorf("relativePath(%q) = %q, want rejection", in, got)
		}
	}
}

// 正規表現をやめた判定が、前の正規表現と同じ答えを返す
func TestRouteMatchers(t *testing.T) {
	deny := map[string]bool{
		"/x.php": true, "/X.PHP": true, "/a/b.php5": true, "/.env": true, "/a/.git/x": true, "/a/..": true,
		"/x.php.txt": false, "/phpinfo": false, "/a.php55": false, "/a.ph": false, "/maps/x/tiles/0/x1/z2.prbm": false, "/_nuxt/entry.abc.js": false,
		// エンコードしたドット・スラッシュ・バックスラッシュ・NUL
		"/a%2eb": true, "/a%2Eb": true, "/a%2fb": true, "/a%2Fb": true, "/a%5cb": true, "/a%5Cb": true, "/a%00b": true,
		"/tiles/a%20b.png": false, "/a%": false, "/a%2": false, "/a%41": false,
	}
	for p, want := range deny {
		if got := denyPath(p); got != want {
			t.Errorf("denyPath(%q) = %v, want %v", p, got, want)
		}
	}
	live := map[string]string{
		"/maps/overworld/live/markers.json": "markers.json", "/maps/o/live/sse": "sse", "/maps/o/live/players.json": "players.json", "/maps/o/live/": "",
	}
	for p, want := range live {
		if _, got, ok := mapsLive(p); !ok || got != want {
			t.Errorf("mapsLive(%q) = %q, %v; want %q", p, got, ok, want)
		}
	}
	if id, _, ok := mapsLive("/maps/overworld/live/sse"); !ok || id != "overworld" {
		t.Errorf("mapsLive id = %q, %v", id, ok)
	}
	for _, p := range []string{"/maps/o/tiles/0/x1/z2.prbm", "/maps//live/x", "/maps/o", "/maps/o/livex/a", "/map/o/live/a", "/maps/live/x"} {
		if _, got, ok := mapsLive(p); ok {
			t.Errorf("mapsLive(%q) = %q, want no match", p, got)
		}
	}
	for _, p := range []string{"/up/world/main/123", "/up/world/main/0", "/up/world/main/-3"} {
		if w, ok := upWorld(p); !ok || w != "main" {
			t.Errorf("upWorld(%q): %q %v", p, w, ok)
		}
	}
	for _, p := range []string{"/up/world/", "/up/world//1", "/up/world/main", "/up/world/main/", "/up/configuration", "/up/world/main/123.json", "/up/world/main/1/x", "/up/world/main/abc", "/up/world/main/-", "/up/world/main/123456789012345678901"} {
		if _, ok := upWorld(p); ok {
			t.Errorf("upWorld(%q) must not match", p)
		}
	}
	good := []string{"/abcdefghijkl.png", "/abcdefghijkl.a", "/abcdefghijkl.abcdefghij", "/a2b3c4d5e6f7.m4v"}
	for _, p := range good {
		if f, ok := assetFile(p); !ok || f != p[1:] {
			t.Errorf("assetFile(%q) = %q, %v", p, f, ok)
		}
	}
	for _, p := range []string{"/abcdefghijk.png", "/ABCDEFGHIJKL.png", "/abcdefghijkl", "/abcdefghijkl.", "/abcdefghijkl.png/x", "/abcdefghijkl.PNG", "/abcdefghijk1.png", "/abcdefghijkl.abcdefghijk", "/", "/_assets/abcdefghijkl.png"} {
		if _, ok := assetFile(p); ok {
			t.Errorf("assetFile(%q) must not match", p)
		}
	}
}

// /tiles/ と /mcassets/skin は、toro-webmap が見るものだけをクエリに残す (ほかを変えても別のものにならない)
func TestQueryFilters(t *testing.T) {
	tiles := map[string]string{
		"":                       "",
		"?timestamp=123":         "timestamp=123",
		"?a=1&timestamp=123&b=2": "timestamp=123",
		"?timestamp=":            "",
		"?timestamp=x":           "",
		"?timestamps=1":          "",
		"?a=1":                   "",
	}
	for in, want := range tiles {
		if got := tileQuery(in); got != want {
			t.Errorf("tileQuery(%q) = %q, want %q", in, got, want)
		}
	}
	skins := map[string]string{
		"":                 "",
		"?name=Steve":      "name=Steve",
		"?name=Steve&x=1":  "name=Steve",
		"?x=1&uuid=ab":     "uuid=ab",
		"?name=a&uuid=b":   "name=a&uuid=b",
		"?names=a&uuidx=b": "",
	}
	for in, want := range skins {
		if got := skinQuery(in); got != want {
			t.Errorf("skinQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasToken(t *testing.T) {
	if !hasToken("keep-alive, Upgrade", "upgrade") || !hasToken("CLOSE", "close") || hasToken("closed", "close") || hasToken("", "close") {
		t.Error("hasToken")
	}
}

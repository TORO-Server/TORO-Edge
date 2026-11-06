package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// 1 つのファイルの上限 (これより大きいものは返さない)
	maxFile = 64 << 20
	// 最初に読み込んでおく量の上限
	preloadMax = 256 << 20
	// 変わったかどうかを確かめる間隔。リクエストは待たせず、裏で確かめる
	recheckMs = 1000
	// 見つからなかったファイルを、聞き直さずに 404 にしておく時間
	missingMs  = 2000
	missingMax = 2000
)

var rePHP = regexp.MustCompile(`(?i)\.php\d?$`)

// Transform はファイルの内容を返す前に書き換える (nginx の sub_filter の代わり)
type Transform struct {
	id    string
	apply func(body []byte) []byte
}

// StaticRules は返す頭書き。決まりごとに 1 つ作って、そのポインタで返事を覚える
type StaticRules struct {
	headers []hdr
}

func newStaticRules(pairs ...[2]string) *StaticRules {
	r := &StaticRules{}
	for _, p := range pairs {
		r.headers = append(r.headers, mkHdr(p[0], p[1]))
	}
	return r
}

// relativePath は URL のパスから、ルートからの相対パスを作る。外へ出るもの・隠しファイル・PHP は ok=false
func relativePath(path string) (string, bool) {
	p := path
	if strings.Contains(p, "%") {
		d, err := url.PathUnescape(p)
		if err != nil {
			return "", false
		}
		p = d
	}
	if len(p) > 1024 || strings.ContainsRune(p, 0) || strings.ContainsRune(p, '\\') {
		return "", false
	}
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s == "" {
			continue
		}
		if s == "." || s == ".." || strings.HasPrefix(s, ".") || len(s) > 255 {
			return "", false
		}
		out = append(out, s)
	}
	rel := strings.Join(out, "/")
	if strings.HasSuffix(p, "/") || rel == "" {
		if rel != "" {
			rel += "/index.html"
		} else {
			rel = "index.html"
		}
	}
	if rePHP.MatchString(rel) {
		return "", false
	}
	return rel, true
}

type rec struct {
	abs       string
	mtime     int64
	size      int64
	body      []byte
	typ       string
	etag      string
	lastMod   string
	checkedAt atomic.Int64
	checking  atomic.Bool

	mu       sync.Mutex
	entities map[*StaticRules]*Entity
	// ファイルが変わって、新しいものに入れ替えたあと
	retired bool
}

// entity はこの決まりで返すときの返事 (圧縮した版は、はじめて作ったときに裏で用意する)
func (r *rec) entity(rules *StaticRules) *Entity {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entities[rules]
	if e == nil {
		e = newEntity(entityInit{status: 200, typ: r.typ, body: r.body, etag: r.etag, lastModified: r.lastMod, headers: rules.headers, tier: tierBest})
		r.entities[rules] = e
		// 入れ替えたあとに来た客の分は、メモリから返す (閉じる人のいないファイルを作らない)
		if !r.retired {
			e.attachFiles()
			e.warm()
		}
	}
	return e
}

// retire は新しいものに入れ替えたとき。返事のファイルを閉じる (送っている最中の客がいれば、終わってから)
func (r *rec) retire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retired = true
	for _, e := range r.entities {
		e.retire(nil)
	}
}

type loadFlight struct {
	done chan struct{}
	rec  *rec
}

// StaticSite はフォルダのファイルをメモリに置いて返す。ファイルは ETag と圧縮した版を付けて持ち、変わったら 1 秒ほどで読み直す
type StaticSite struct {
	root    string
	exclude []string

	mu      sync.RWMutex
	recs    map[string]*rec
	missing map[string]int64
	loading map[string]*loadFlight

	rootOnce sync.Once
	realRoot string

	maps atomic.Pointer[mapIDs]
}

func newStaticSite(root string, exclude ...string) *StaticSite {
	return &StaticSite{root: root, exclude: exclude, recs: map[string]*rec{}, missing: map[string]int64{}, loading: map[string]*loadFlight{}}
}

func staticKey(rel string, tr *Transform) string {
	if tr == nil {
		return "\x00" + rel
	}
	return tr.id + "\x00" + rel
}

func (s *StaticSite) excluded(rel string) bool {
	for _, x := range s.exclude {
		if rel == x || strings.HasPrefix(rel, x+"/") {
			return true
		}
	}
	return false
}

func (s *StaticSite) size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.recs)
}

// peek はメモリにあれば、すぐ返す (古くなっていれば、裏で確かめる)
func (s *StaticSite) peek(rel string, tr *Transform) *rec {
	key := staticKey(rel, tr)
	s.mu.RLock()
	r := s.recs[key]
	s.mu.RUnlock()
	if r == nil {
		return nil
	}
	if nowMs()-r.checkedAt.Load() >= recheckMs && r.checking.CompareAndSwap(false, true) {
		go s.recheck(key, rel, r, tr)
	}
	return r
}

// mapIDs は settings.json の maps (ファイルが変わったら作り直す)。ids が nil なら読めなかった
type mapIDs struct {
	from *rec
	ids  map[string]bool
}

// knownMap は BlueMap の settings.json に書かれた地図か。読めないとき (ファイルがない、形が違う) は止めないよう true
func (s *StaticSite) knownMap(id string) bool {
	r := s.peek("settings.json", nil)
	if r == nil {
		r = s.load("settings.json", nil)
	}
	if r == nil {
		return true
	}
	m := s.maps.Load()
	if m == nil || m.from != r {
		m = &mapIDs{from: r}
		var v struct {
			Maps []string `json:"maps"`
		}
		if json.Unmarshal(r.body, &v) == nil && v.Maps != nil {
			m.ids = make(map[string]bool, len(v.Maps))
			for _, name := range v.Maps {
				m.ids[name] = true
			}
		}
		s.maps.Store(m)
	}
	return m.ids == nil || m.ids[id]
}

// load はディスクから読む (同じファイルを同時に読まない)
func (s *StaticSite) load(rel string, tr *Transform) *rec {
	key := staticKey(rel, tr)
	now := nowMs()
	s.mu.Lock()
	if until, ok := s.missing[key]; ok {
		if until > now {
			s.mu.Unlock()
			return nil
		}
		delete(s.missing, key)
	}
	if f, ok := s.loading[key]; ok {
		s.mu.Unlock()
		<-f.done
		return f.rec
	}
	f := &loadFlight{done: make(chan struct{})}
	s.loading[key] = f
	s.mu.Unlock()

	r := s.read(rel, tr)

	s.mu.Lock()
	if r != nil {
		s.recs[key] = r
	} else {
		if len(s.missing) >= missingMax {
			for k := range s.missing {
				delete(s.missing, k)
				break
			}
		}
		s.missing[key] = nowMs() + missingMs
	}
	delete(s.loading, key)
	s.mu.Unlock()
	f.rec = r
	close(f.done)
	return r
}

func (s *StaticSite) recheck(key, rel string, r *rec, tr *Transform) {
	defer r.checking.Store(false)
	st, err := os.Stat(r.abs)
	if err == nil && st.ModTime().UnixNano() == r.mtime && st.Size() == r.size {
		r.checkedAt.Store(nowMs())
		return
	}
	var fresh *rec
	if err == nil {
		fresh = s.read(rel, tr)
	}
	s.mu.Lock()
	if fresh != nil {
		s.recs[key] = fresh
	} else {
		delete(s.recs, key)
	}
	s.mu.Unlock()
	r.retire()
}

func (s *StaticSite) read(rel string, tr *Transform) *rec {
	if s.excluded(rel) {
		return nil
	}
	file := filepath.Join(s.root, filepath.FromSlash(rel))
	st, err := os.Stat(file)
	if err != nil {
		return nil
	}
	if st.IsDir() {
		file = filepath.Join(file, "index.html")
		if st, err = os.Stat(file); err != nil {
			return nil
		}
	}
	if !st.Mode().IsRegular() || st.Size() > maxFile {
		return nil
	}
	// シンボリックリンクでルートの外へ出るものは返さない
	s.rootOnce.Do(func() {
		if p, err := filepath.EvalSymlinks(s.root); err == nil {
			s.realRoot = p
		} else {
			s.realRoot = s.root
		}
	})
	real, err := filepath.EvalSymlinks(file)
	if err != nil || (real != s.realRoot && !strings.HasPrefix(real, s.realRoot+string(filepath.Separator))) {
		return nil
	}
	body, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	if tr != nil {
		body = tr.apply(body)
	}
	sum := sha1.Sum(body)
	r := &rec{
		abs:      file,
		mtime:    st.ModTime().UnixNano(),
		size:     st.Size(),
		body:     body,
		typ:      mimeType(file),
		etag:     `"` + base64.RawURLEncoding.EncodeToString(sum[:])[:22] + `"`,
		lastMod:  time.Unix(st.ModTime().Unix(), 0).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"),
		entities: map[*StaticRules]*Entity{},
	}
	r.checkedAt.Store(nowMs())
	return r
}

// preload は起動のときに、フォルダのファイルを読んでおく (最初のリクエストをディスクから読まないため)。返事は待たせない
func (s *StaticSite) preload(rulesFor func(rel string) *StaticRules) {
	var total int64
	filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		relOS, _ := filepath.Rel(s.root, path)
		rel := filepath.ToSlash(relOS)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if s.excluded(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || rePHP.MatchString(d.Name()) || total >= preloadMax {
			return nil
		}
		if r := s.load(rel, nil); r != nil {
			total += r.size
			if rules := rulesFor(rel); rules != nil {
				r.entity(rules)
			}
		}
		return nil
	})
}

// prime は書き換えて返すファイルを、先に読んでおく
func (s *StaticSite) prime(rel string, tr *Transform, rules *StaticRules) {
	if r := s.load(rel, tr); r != nil {
		r.entity(rules)
	}
}

type staticOptions struct {
	// URL のパスではなく、このファイルを返す
	file      string
	transform *Transform
}

func staticHandler(site *StaticSite, rules *StaticRules, opt staticOptions) Handler {
	return func(c *Ctx) {
		rel := opt.file
		if rel == "" {
			var ok bool
			rel, ok = relativePath(c.path)
			if !ok {
				notFound(c)
				return
			}
		}
		r := site.peek(rel, opt.transform)
		if r == nil {
			r = site.load(rel, opt.transform)
		}
		if r == nil {
			notFound(c)
			return
		}
		c.cache = "HIT"
		sendEntity(c, r.entity(rules))
	}
}

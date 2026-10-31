import assert from 'node:assert/strict'
import http from 'node:http'
import type { Socket } from 'node:net'
import { test } from 'node:test'
import { brotliDecompressSync, gunzipSync, gzipSync } from 'node:zlib'
import { CSS, echoUpgrade, INDEX_HTML, request, SETTINGS_JSON, sleep, withEnv } from './helpers.js'

// 実装の言語によらない適合試験: 偽の元のサーバーと一時フォルダを用意して、実行ファイル (EDGE_BIN) を外から確かめる。
// 実装の中の単体試験は、Go のテスト (*_test.go) にある

const text = (b: Buffer): string => b.toString('utf8')

test('routing: PHP と隠しファイルは、あっても返さない / index.html は / へ', async () => {
  await withEnv(async (env) => {
    for (const path of ['/standalone/MySQL_config.php', '/x.php', '/.secret', '/css/.x', '/..%2foutside.txt', '/%2e%2e/outside.txt', '/css/%2e%2e/%2e%2e/outside.txt', '/css/..%2f..%2foutside.txt']) {
      const r = await env.get(path)
      assert.equal(r.status, 404, path)
      assert.ok(!text(r.body).includes('secret') && !text(r.body).includes('outside the root'), path)
    }
    const redirect = await env.get('/index.html?worldname=main&mapname=surface')
    assert.equal(redirect.status, 301)
    assert.equal(redirect.headers.location, '/?worldname=main&mapname=surface')
    assert.equal((await env.get('/index.html')).headers.location, '/')
    assert.equal((await env.blue('/index.html?x=1')).headers.location, '/?x=1')
    assert.equal(env.mapweb.seen.length, 0)
  })
})

test('static (Dynmap): ETag, 304, br / gzip / そのまま, HEAD', async () => {
  await withEnv(async (env) => {
    const plain = await env.get('/css/style.css')
    assert.equal(plain.status, 200)
    assert.equal(plain.headers['content-type'], 'text/css; charset=utf-8')
    assert.equal(plain.headers['cache-control'], 'public, max-age=86400')
    assert.equal(plain.headers['content-encoding'], undefined)
    assert.equal(text(plain.body), CSS)
    assert.equal(String(plain.headers.vary).toLowerCase(), 'accept-encoding')
    const etag = plain.headers.etag
    assert.ok(etag)

    const notModified = await env.get('/css/style.css', { headers: { 'if-none-match': etag } })
    assert.equal(notModified.status, 304)
    assert.equal(notModified.body.length, 0)
    assert.equal(notModified.headers['cache-control'], 'public, max-age=86400')

    const br = await env.get('/css/style.css', { headers: { 'accept-encoding': 'gzip, br' } })
    assert.equal(br.headers['content-encoding'], 'br')
    assert.equal(text(brotliDecompressSync(br.body)), CSS)
    assert.ok(br.body.length < plain.body.length / 5)
    assert.notEqual(br.headers.etag, etag)
    // 圧縮した版の ETag でも、そのままの版の ETag でも 304
    assert.equal((await env.get('/css/style.css', { headers: { 'accept-encoding': 'br', 'if-none-match': br.headers.etag! } })).status, 304)
    assert.equal((await env.get('/css/style.css', { headers: { 'accept-encoding': 'br', 'if-none-match': etag } })).status, 304)

    const gz = await env.get('/css/style.css', { headers: { 'accept-encoding': 'gzip' } })
    assert.equal(gz.headers['content-encoding'], 'gzip')
    assert.equal(text(gunzipSync(gz.body)), CSS)

    const head = await env.get('/css/style.css', { method: 'HEAD' })
    assert.equal(head.status, 200)
    assert.equal(head.body.length, 0)
    assert.equal(head.headers['content-length'], String(plain.body.length))

    const png = await env.get('/images/blank.png', { headers: { 'accept-encoding': 'br' } })
    assert.equal(png.headers['content-type'], 'image/png')
    assert.equal(png.headers['content-encoding'], undefined)
    assert.equal((await env.get('/images/nothing.png')).status, 404)
    // Host が知らない名前でも map.torosaba.net と同じ (内側から toro-nginx で呼ばれる)
    assert.equal((await env.get('/css/style.css', { host: 'toro-nginx' })).status, 200)
    assert.equal(env.mapweb.seen.length, 0)
  })
})

test('static: ファイルが変わったら読み直す / 新しいファイルも返す', async () => {
  await withEnv(async (env) => {
    const { writeFile } = await import('node:fs/promises')
    const { join } = await import('node:path')
    assert.equal(text((await env.get('/standalone/config.js')).body), 'var config = {}')
    await writeFile(join(env.dynDir, 'standalone/config.js'), 'var config = {"changed":true}')
    await sleep(1100)
    // 1 度目は古いものを返しながら裏で確かめる
    await env.get('/standalone/config.js')
    await sleep(100)
    const r = await env.get('/standalone/config.js')
    assert.equal(text(r.body), 'var config = {"changed":true}')
    assert.equal(r.headers['cache-control'], 'no-cache')
    assert.equal((await env.get('/new.txt')).status, 404)
    await sleep(2100)
    await writeFile(join(env.dynDir, 'new.txt'), 'new file')
    assert.equal(text((await env.get('/new.txt')).body), 'new file')
  })
})

test('static: legacy.html と BlueMap の書き換え (sub_filter の代わり)', async () => {
  await withEnv(async (env) => {
    const legacy = await env.get('/legacy.html')
    assert.equal(legacy.status, 200)
    assert.equal(legacy.headers['cache-control'], 'no-cache')
    assert.equal(text(legacy.body), INDEX_HTML.replace('<script type="text/javascript" src="standalone/config.js',
      '<script type="text/javascript" src="toro/dynmap-client.js?v=2"></script>\n\t<script type="text/javascript" src="standalone/config.js'))

    const settings = await env.blue('/settings.json')
    assert.equal(text(settings.body), SETTINGS_JSON.replace('"scripts":[', '"scripts":["toro-refresh.js?v=1",'))
    assert.equal(settings.headers['cache-control'], 'no-cache')
    assert.ok(settings.headers['content-security-policy']?.includes('media-src \'self\' https: blob:'))
    assert.equal(settings.headers['x-content-type-options'], 'nosniff')
    // 書き換えたあとの内容から ETag を作るので、書き換える前のものを持つブラウザには 200 で新しい内容を返す
    assert.equal((await env.blue('/settings.json', { headers: { 'if-none-match': 'W/"original"' } })).status, 200)
    assert.equal((await env.blue('/settings.json', { headers: { 'if-none-match': settings.headers.etag! } })).status, 304)

    assert.equal(text((await env.blue('/lang/settings.conf')).body), 'default: "ja"\nother: "en"\n')

    const refresh = await env.blue('/toro-refresh.js')
    assert.match(text(refresh.body), /sessionStorage\.getItem\('toro-refresh'\)/)
    assert.equal(refresh.headers['cache-control'], 'no-cache')

    const sign = await env.blue('/marumasign/overworld.json')
    assert.equal(sign.headers['cache-control'], 'no-cache')
    const asset = await env.blue('/assets/a.png')
    assert.equal(asset.headers['cache-control'], 'public, max-age=86400, stale-if-error=604800')
    assert.equal(asset.headers['cdn-cache-control'], 'max-age=60')
    // maps/ は静的に返さない (map-web に聞く)
    env.mapweb.handler = (_req, res) => {
      res.writeHead(404)
      res.end()
    }
    assert.equal((await env.blue('/maps/hidden.json')).status, 404)
    assert.equal(env.mapweb.hitCount('/maps/hidden.json'), 1)
  })
})

test('cache: 同時に聞かない (1 回だけ元へ)、そのあとは元に聞かない', async () => {
  await withEnv(async (env) => {
    const png = Buffer.alloc(2000, 7)
    env.mapweb.handler = (_req, res) => {
      setTimeout(() => {
        res.writeHead(200, { 'content-type': 'image/png', 'cache-control': 'public, max-age=300', 'etag': '"t1"', 'last-modified': 'Wed, 21 Oct 2026 07:28:00 GMT' })
        res.end(png)
      }, 100)
    }
    const url = '/tiles/main/flat/0_0/zzz_0_0.png?timestamp=1'
    const replies = await Promise.all(Array.from({ length: 20 }, () => env.get(url)))
    assert.equal(env.mapweb.hitCount(url), 1)
    for (const r of replies) {
      assert.equal(r.status, 200)
      assert.deepEqual(r.body, png)
      assert.equal(r.headers.etag, '"t1"')
    }
    assert.ok(replies.some(r => r.headers['x-edge-cache'] === 'MISS'))
    assert.ok(replies.some(r => r.headers['x-edge-cache'] === 'COALESCED'))
    const again = await env.get(url)
    assert.equal(again.headers['x-edge-cache'], 'HIT')
    assert.equal(again.headers['cache-control'], 'public, max-age=300')
    assert.equal(env.mapweb.hitCount(url), 1)
    // 条件付きで聞かれたら、元に聞かず 304
    const cond = await env.get(url, { headers: { 'if-none-match': '"t1"' } })
    assert.equal(cond.status, 304)
    const ims = await env.get(url, { headers: { 'if-modified-since': 'Wed, 21 Oct 2026 07:28:00 GMT' } })
    assert.equal(ims.status, 304)
    assert.equal(env.mapweb.hitCount(url), 1)
    // 元へは Accept-Encoding: identity と、客の Host、X-Forwarded-For だけ。Cookie などは渡さない
    const seen = env.mapweb.seen[0]!.headers
    assert.equal(seen['accept-encoding'], 'identity')
    assert.equal(seen.host, 'map.torosaba.net')
    assert.match(String(seen['x-forwarded-for']), /127\.0\.0\.1/)
  })
})

test('cache: 古くなったら古いまま返して裏で確かめる (304 で延ばす)。元が壊れていても古いものを返す', async () => {
  await withEnv(async (env) => {
    let down = false
    env.mapweb.handler = (req, res) => {
      if (down) {
        req.socket.destroy()
        return
      }
      if (req.headers['if-none-match'] === '"w1"') {
        res.writeHead(304, { etag: '"w1"' })
        res.end()
        return
      }
      res.writeHead(200, { 'content-type': 'application/json', 'cache-control': 'no-cache', 'etag': '"w1"' })
      res.end('{"timestamp":1}')
    }
    const url = '/up/world/main/123.json'
    const first = await env.get(url)
    assert.equal(first.headers['x-edge-cache'], 'MISS')
    assert.equal(env.mapweb.hitCount(url), 1)
    // 別の ts でも、同じワールドは同じもの
    assert.equal((await env.get('/up/world/main/999.json')).headers['x-edge-cache'], 'HIT')
    assert.equal(env.mapweb.hitCount(url), 1)

    await sleep(1100)
    const stale = await env.get(url)
    assert.equal(stale.headers['x-edge-cache'], 'STALE')
    assert.equal(text(stale.body), '{"timestamp":1}')
    await sleep(150)
    assert.equal(env.mapweb.hitCount(url), 2)
    assert.equal(env.mapweb.seen.at(-1)!.headers['if-none-match'], '"w1"')
    // 304 で新しくなった
    assert.equal((await env.get(url)).headers['x-edge-cache'], 'HIT')

    // 古さの限りを超えて、元が落ちている: 古いものを返す
    await sleep(3200)
    down = true
    const err = await env.get(url)
    assert.equal(err.status, 200)
    assert.equal(err.headers['x-edge-cache'], 'ERROR-STALE')
    assert.equal(text(err.body), '{"timestamp":1}')
    // 初めて聞くものが落ちている元に当たったら 502
    assert.equal((await env.get('/up/world/other/1.json')).status, 502)
  })
})

test('cache: 5xx は、古いものがなければそのまま返し、ためない', async () => {
  await withEnv(async (env) => {
    env.mapweb.handler = (_req, res) => {
      res.writeHead(503, { 'content-type': 'text/plain' })
      res.end('Database unavailable')
    }
    const a = await env.get('/up/configuration')
    assert.equal(a.status, 503)
    assert.equal(text(a.body), 'Database unavailable')
    await env.get('/up/configuration')
    assert.equal(env.mapweb.hitCount('/up/configuration'), 2)
  })
})

test('compress: 動的な JSON は 1 度だけ圧縮して使い回す。Vary を付ける', async () => {
  await withEnv(async (env) => {
    const json = JSON.stringify({ sets: Array.from({ length: 400 }, (_, i) => ({ id: `marker-${i}`, label: 'station', x: i, y: 64, z: -i })) })
    env.mapweb.handler = (_req, res) => {
      res.writeHead(200, { 'content-type': 'application/json', 'cache-control': 'no-cache' })
      res.end(json)
    }
    const url = '/up/configuration'
    const br = await env.get(url, { headers: { 'accept-encoding': 'br, gzip' } })
    assert.equal(br.headers['content-encoding'], 'br')
    assert.equal(String(br.headers.vary).toLowerCase(), 'accept-encoding')
    assert.equal(text(brotliDecompressSync(br.body)), json)
    const gz = await env.get(url, { headers: { 'accept-encoding': 'gzip' } })
    assert.equal(text(gunzipSync(gz.body)), json)
    const plain = await env.get(url)
    assert.equal(text(plain.body), json)
    assert.equal(plain.headers['content-encoding'], undefined)
    assert.equal(env.mapweb.hitCount(url), 1)
    // 条件付き
    assert.equal((await env.get(url, { headers: { 'accept-encoding': 'br', 'if-none-match': br.headers.etag! } })).status, 304)
  })
})

test('cache: 元がすでに圧縮しているもの (BlueMap のタイル) はそのまま返す', async () => {
  await withEnv(async (env) => {
    const body = gzipSync(Buffer.alloc(5000, 3))
    env.mapweb.handler = (_req, res) => {
      res.writeHead(200, { 'content-type': 'application/octet-stream', 'content-encoding': 'gzip', 'cache-control': 'public, max-age=86400, stale-if-error=604800', 'etag': '"p1"', 'access-control-allow-origin': '*' })
      res.end(body)
    }
    const url = '/maps/overworld/tiles/0/x0/z0.prbm'
    for (let i = 0; i < 2; i++) {
      const r = await env.blue(url, { headers: { 'accept-encoding': 'gzip, br' } })
      assert.equal(r.status, 200)
      assert.equal(r.headers['content-encoding'], 'gzip')
      assert.deepEqual(r.body, body)
      assert.equal(r.headers['cache-control'], 'public, max-age=86400, stale-if-error=604800')
      assert.equal(r.headers['cdn-cache-control'], 'max-age=60')
      assert.equal(r.headers['access-control-allow-origin'], undefined)
      assert.ok(r.headers['content-security-policy'])
    }
    assert.equal(env.mapweb.hitCount(url), 1)
    // クエリは鍵に入れない (BlueMap は付けることがある)
    await env.blue(`${url}?cachebust=1`)
    assert.equal(env.mapweb.hitCount(url), 1)
    // 描いていないタイルは 204
    env.mapweb.handler = (_req, res) => {
      res.writeHead(204, { 'cache-control': 'public, max-age=60' })
      res.end()
    }
    const empty = await env.blue('/maps/overworld/tiles/0/x9/z9.prbm')
    assert.equal(empty.status, 204)
    assert.equal((await env.blue('/maps/overworld/tiles/0/x9/z9.prbm')).headers['x-edge-cache'], 'HIT')
  })
})

test('bluemap live: BlueMap 本体に聞き、1 秒まとめ、頭書きを付け替える。落ちていたら DB の版 (map-web) へ', async () => {
  await withEnv(async (env) => {
    env.live.handler = (_req, res) => {
      res.writeHead(200, { 'content-type': 'application/json', 'cache-control': 'max-age=86400', 'content-security-policy': 'default-src none', 'x-content-type-options': 'x' })
      res.end('{"players":[]}')
    }
    const url = '/maps/overworld/live/players.json'
    const r = await env.blue(url)
    assert.equal(r.status, 200)
    assert.equal(r.headers['cache-control'], 'no-cache')
    assert.ok(String(r.headers['content-security-policy']).startsWith('default-src \'self\''))
    assert.equal(r.headers['x-content-type-options'], 'nosniff')
    await Promise.all(Array.from({ length: 10 }, () => env.blue(url)))
    assert.equal(env.live.hitCount(url), 1)
    assert.equal(env.mapweb.hitCount(url), 0)
    assert.equal(env.live.seen[0]!.headers.host, '3d-map.torosaba.net')

    // マーカーは map-web (Dynmap の地点を足したもの)
    env.mapweb.handler = (_req, res) => {
      res.writeHead(200, { 'content-type': 'application/json', 'cache-control': 'no-cache', 'access-control-allow-origin': '*' })
      res.end('{"merged":true}')
    }
    const markers = await env.blue('/maps/overworld/live/markers.json')
    assert.equal(text(markers.body), '{"merged":true}')
    assert.equal(markers.headers['access-control-allow-origin'], undefined)
    assert.equal(env.live.hitCount('/maps/overworld/live/markers.json'), 0)

    // BlueMap 本体が落ちている (toro-main の再起動中)
    env.live.server.closeAllConnections()
    await new Promise<void>(resolve => env.live.server.close(() => resolve()))
    env.mapweb.handler = (_req, res) => {
      res.writeHead(200, { 'content-type': 'application/json', 'cache-control': 'public, max-age=3600', 'access-control-allow-origin': '*' })
      res.end('{"players":["last-from-db"]}')
    }
    // まだためていない (古いものもない) ものは、map-web が DB から返す。ためてあるものは errorTtl のあいだ古い版を返す
    const fallback = await env.blue('/maps/overworld/live/other.json?x=1')
    assert.equal(fallback.status, 200)
    assert.equal(fallback.headers['x-edge-cache'], 'MISS')
    assert.equal(text(fallback.body), '{"players":["last-from-db"]}')
    assert.equal(fallback.headers['cache-control'], 'no-cache')
    // Cloudflare に長くためさせない (nginx は 60 秒と付けていた)
    assert.equal(fallback.headers['cdn-cache-control'], undefined)
  })
})

test('assets: 内側の頭書きは客に書かせず、Cloudflare の IP から作る。Range は元が処理する。形が違うものは元に聞かない', async () => {
  await withEnv(async (env) => {
    env.mapweb.handler = (req, res) => {
      res.writeHead(req.headers.range ? 206 : 200, { 'content-type': 'image/png', 'cache-control': 'private, max-age=31536000, immutable', 'content-length': '4', 'content-range': 'bytes 0-3/10' })
      res.end('abcd')
    }
    const r = await env.assets('/abcdefghijkl.png', {
      headers: { 'x-toro-client-ip': '1.2.3.4', 'cf-connecting-ip': '9.9.9.9', 'cookie': 'a=b', 'range': 'bytes=0-3', 'origin': 'https://map.torosaba.net', 'referer': 'https://map.torosaba.net/' },
    })
    assert.equal(r.status, 206)
    assert.equal(text(r.body), 'abcd')
    assert.equal(r.headers['content-range'], 'bytes 0-3/10')
    const seen = env.mapweb.seen[0]!
    assert.equal(seen.url, '/_assets/abcdefghijkl.png')
    assert.equal(seen.headers['x-toro-client-ip'], '9.9.9.9')
    assert.equal(seen.headers.range, 'bytes=0-3')
    assert.equal(seen.headers.origin, 'https://map.torosaba.net')
    assert.equal(seen.headers.cookie, undefined)

    // Cloudflare を通っていなければ、客が書いても渡さない
    await env.assets('/abcdefghijkl.png', { headers: { 'x-toro-client-ip': '1.2.3.4' } })
    assert.equal(env.mapweb.seen[1]!.headers['x-toro-client-ip'], undefined)

    const n = env.mapweb.seen.length
    for (const bad of ['/abcdefghijk.png', '/ABCDEFGHIJKL.png', '/abcdefghijkl', '/abcdefghijkl.png/x', '/_assets/abcdefghijkl.png', '/']) {
      assert.equal((await env.assets(bad)).status, 404, bad)
    }
    assert.equal(env.mapweb.seen.length, n)
  })
})

test('stream: 客が切れたら元も切る / ためない', async () => {
  await withEnv(async (env) => {
    let closed = false
    env.mapweb.handler = (_req, res) => {
      res.writeHead(200, { 'content-type': 'video/mp4' })
      const timer = setInterval(() => res.write(Buffer.alloc(1024, 1)), 20)
      // 終わらせない返事なので、閉じるのは、つなぎが切れたとき
      res.on('close', () => {
        closed = true
        clearInterval(timer)
      })
    }
    await new Promise<void>((resolve, reject) => {
      const req = http.request({ host: '127.0.0.1', port: env.edge.port, path: '/_marumasign/proxy?u=https%3A%2F%2Fexample.com%2Fa.mp4', headers: { host: '3d-map.torosaba.net', origin: 'https://3d-map.torosaba.net', range: 'bytes=0-' }, agent: false }, (res) => {
        res.once('data', () => {
          req.destroy()
          resolve()
        })
      })
      req.on('error', () => {})
      req.on('timeout', () => reject(new Error('timeout')))
      req.end()
    })
    for (let i = 0; i < 50 && !closed; i++) await sleep(20)
    assert.equal(closed, true)
    assert.equal(env.mapweb.seen[0]!.headers.range, 'bytes=0-')
    assert.equal(env.mapweb.seen[0]!.headers.origin, 'https://3d-map.torosaba.net')
  })
})

test('WebSocket: /up/ws だけ、つないだあとはそのまま渡す', async () => {
  await withEnv(async (env) => {
    echoUpgrade(env.mapweb)
    const socket = await new Promise<Socket>((resolve, reject) => {
      const req = http.request({
        host: '127.0.0.1',
        port: env.edge.port,
        path: '/up/ws',
        headers: { 'host': 'map.torosaba.net', 'connection': 'Upgrade', 'upgrade': 'websocket', 'sec-websocket-key': 'dGhlIHNhbXBsZSBub25jZQ==', 'sec-websocket-version': '13', 'origin': 'https://map.torosaba.net' },
        agent: false,
      })
      req.on('upgrade', (res, s) => {
        assert.equal(res.statusCode, 101)
        resolve(s)
      })
      req.on('error', reject)
      req.end()
    })
    const echoed = new Promise<string>(resolve => socket.once('data', d => resolve(d.toString())))
    socket.write('hello through the proxy')
    assert.equal(await echoed, 'hello through the proxy')
    socket.destroy()
    const seen = env.mapweb.seen[0]!
    assert.equal(seen.url, '/up/ws')
    assert.equal(seen.headers['sec-websocket-key'], 'dGhlIHNhbXBsZSBub25jZQ==')
    assert.equal(seen.headers.host, 'map.torosaba.net')

    // /up/ws 以外や、ほかのサイトへの Upgrade は断る
    for (const [path, host] of [['/up/other', 'map.torosaba.net'], ['/up/ws', '3d-map.torosaba.net'], ['/live/plugin', 'map.torosaba.net']]) {
      const status = await new Promise<number>((resolve) => {
        const req = http.request({ host: '127.0.0.1', port: env.edge.port, path, headers: { host, connection: 'Upgrade', upgrade: 'websocket' }, agent: false })
        req.on('response', res => resolve(res.statusCode ?? 0))
        req.on('upgrade', () => resolve(101))
        req.on('error', () => resolve(0))
        req.end()
      })
      assert.equal(status, 404, `${host}${path}`)
    }
    // WebSocket でない GET は 426
    assert.equal((await env.get('/up/ws')).status, 426)
  })
})

test('methods / URL: GET と HEAD だけ、長すぎる URL は断る', async () => {
  await withEnv(async (env) => {
    const post = await env.get('/css/style.css', { method: 'POST' })
    assert.equal(post.status, 405)
    assert.equal(post.headers.allow, 'GET, HEAD')
    assert.equal((await env.get('/live/clients', { method: 'POST' })).status, 405)
    assert.equal((await env.get(`/${'a'.repeat(9000)}`)).status, 414)
    // 内側のための /live/ は、外から GET しても map-web に届かない (map.torosaba.net 側は静的なファイルとして探して 404)
    assert.equal((await env.get('/live/plugin')).status, 404)
    assert.equal(env.mapweb.seen.length, 0)
  })
})

test('cache: 上限を超えたら、使われていないものから捨てる / 大きすぎるものはためずに流す', async () => {
  await withEnv(async (env) => {
    const body = Buffer.alloc(20_000, 5)
    env.mapweb.handler = (_req, res) => {
      res.writeHead(200, { 'content-type': 'image/png', 'cache-control': 'public, max-age=300', 'etag': '"x"' })
      res.end(body)
    }
    for (let i = 0; i < 12; i++) await env.get(`/tiles/w/p/0_0/${i}.png`)
    const cache = await env.cacheStats()
    assert.ok(cache.bytes <= 200 * 1024, `bytes=${cache.bytes}`)
    assert.ok(cache.entries < 12)
    assert.ok(cache.entries > 0)

    // maxEntityBytes (上限の 1/8 = 25KB) より大きいもの
    const big = Buffer.alloc(100_000, 9)
    env.mapweb.handler = (_req, res) => {
      res.writeHead(200, { 'content-type': 'image/png', 'cache-control': 'public, max-age=300' })
      res.end(big)
    }
    const a = await env.get('/tiles/w/p/0_0/big.png')
    assert.deepEqual(a.body, big)
    assert.equal(a.headers['x-edge-cache'], 'BYPASS')
    await env.get('/tiles/w/p/0_0/big.png')
    assert.ok(env.mapweb.hitCount('/tiles/w/p/0_0/big.png') >= 2)
  }, { cacheBytes: 200 * 1024, maxEntityBytes: 25 * 1024 })
})

test('頭書き: Dynmap 側は / を no-cache + ja、BlueMap 側は CSP を付ける', async () => {
  await withEnv(async (env) => {
    env.mapweb.handler = (_req, res) => {
      res.writeHead(200, { 'content-type': 'text/html; charset=utf-8', 'cache-control': 'public, max-age=300', 'etag': '"h1"', 'x-powered-by': 'nitro' })
      res.end('<html></html>')
    }
    const map = await env.get('/')
    assert.equal(map.headers['cache-control'], 'no-cache')
    assert.equal(map.headers['content-language'], 'ja')
    assert.equal(map.headers['content-security-policy'], undefined)
    assert.equal(map.headers['x-powered-by'], undefined)
    const blue = await env.blue('/')
    assert.equal(blue.headers['cache-control'], 'no-cache')
    assert.equal(blue.headers['content-language'], 'ja')
    assert.ok(blue.headers['content-security-policy']?.includes('frame-ancestors \'self\''))
    // 同じパスでも、site ごとに別のものをためる
    assert.equal(env.mapweb.hitCount('/'), 2)
    assert.equal((await env.blue('/_nuxt/entry.js')).status, 200)
  })
})

/** 終わらない text/event-stream を返す元 */
function sseUpstream(onClose: () => void): (req: http.IncomingMessage, res: http.ServerResponse) => void {
  return (_req, res) => {
    res.writeHead(200, {
      'content-type': 'text/event-stream',
      'cache-control': 'no-store',
      'cdn-cache-control': 'no-store',
      'cloudflare-cdn-cache-control': 'no-store',
      'content-security-policy': 'default-src none',
      'x-accel-buffering': 'no',
    })
    res.write('event: hello\ndata: 1\n\n')
    const timer = setInterval(() => res.write(': ping\n\n'), 50)
    res.on('close', () => {
      clearInterval(timer)
      onClose()
    })
  }
}

/** 最初のチャンクが届いたら、頭書きと一緒に返して切る */
function firstChunk(port: number, path: string, host: string, headers: Record<string, string> = {}): Promise<{ status: number, headers: http.IncomingHttpHeaders, chunk: string, ms: number }> {
  return new Promise((resolve, reject) => {
    const t0 = performance.now()
    const req = http.request({ host: '127.0.0.1', port, path, headers: { host, ...headers }, agent: false }, (res) => {
      res.once('data', (c: Buffer) => {
        resolve({ status: res.statusCode ?? 0, headers: res.headers, chunk: c.toString(), ms: performance.now() - t0 })
        req.destroy()
      })
    })
    req.on('error', () => {})
    req.setTimeout(3000, () => reject(new Error('no data within 3s')))
    req.end()
  })
}

test('SSE (BlueMap の live/sse): ためずにすぐ流す。頭書きを付け替え、客が切れたら元も切る', async () => {
  await withEnv(async (env) => {
    let closed = false
    env.live.handler = sseUpstream(() => {
      closed = true
    })
    const r = await firstChunk(env.edge.port, '/maps/overworld/live/sse', '3d-map.torosaba.net', { 'accept': 'text/event-stream', 'last-event-id': '41', 'cookie': 'a=b' })
    assert.equal(r.status, 200)
    assert.equal(r.chunk, 'event: hello\ndata: 1\n\n')
    assert.ok(r.ms < 1000, `first chunk took ${r.ms}ms`)
    // EventSource の頭書きは元に渡す。Cookie は渡さない
    assert.equal(env.live.seen[0]!.headers.accept, 'text/event-stream')
    assert.equal(env.live.seen[0]!.headers['last-event-id'], '41')
    assert.equal(env.live.seen[0]!.headers.cookie, undefined)
    assert.equal(r.headers['content-type'], 'text/event-stream')
    assert.equal(r.headers['content-length'], undefined)
    assert.equal(r.headers['cache-control'], 'no-cache')
    assert.equal(r.headers['cdn-cache-control'], undefined)
    assert.equal(r.headers['cloudflare-cdn-cache-control'], 'no-store')
    assert.ok(String(r.headers['content-security-policy']).includes('media-src'))
    for (let i = 0; i < 50 && !closed; i++) await sleep(20)
    assert.equal(closed, true)
  })
})

test('安全網: ためる経路に SSE が来ても、待ち続けずに流す', async () => {
  await withEnv(async (env) => {
    let closed = false
    env.mapweb.handler = sseUpstream(() => {
      closed = true
    })
    const r = await firstChunk(env.edge.port, '/tiles/main/stream', 'map.torosaba.net')
    assert.equal(r.status, 200)
    assert.equal(r.chunk, 'event: hello\ndata: 1\n\n')
    assert.equal(r.headers['x-edge-cache'], 'BYPASS')
    for (let i = 0; i < 50 && !closed; i++) await sleep(20)
    assert.equal(closed, true)
    // 流しているだけで、ためてはいない
    assert.equal((await env.cacheStats()).entries, 0)
  })
})

test('エラーの返事: 404 に Cache-Control を付けない。付け足す頭書きは成功のときだけ', async () => {
  await withEnv(async (env) => {
    const nf = await env.get('/missing.txt')
    assert.equal(nf.status, 404)
    assert.equal(nf.headers['cache-control'], undefined)
    env.mapweb.handler = (_req, res) => {
      res.writeHead(404, { 'content-type': 'application/json', 'content-security-policy': 'script-src \'none\'' })
      res.end('{"statusCode":404}')
    }
    const miss = await env.blue('/_nuxt/old-chunk.js')
    assert.equal(miss.status, 404)
    // BlueMap の CSP に置き換えず、map-web が返したままにする
    assert.equal(miss.headers['content-security-policy'], 'script-src \'none\'')
    // 404 は少しのあいだだけ覚える (古い画面が消えたファイルを何度も聞きに来る)
    await env.blue('/_nuxt/old-chunk.js')
    assert.equal(env.mapweb.hitCount('/_nuxt/old-chunk.js'), 1)
  })
})

test('管理用ポート: /healthz /stats /purge', async () => {
  await withEnv(async (env) => {
    const health = await request(env.edge.adminPort, '/healthz')
    assert.equal(health.status, 200)
    const stats = JSON.parse(text((await request(env.edge.adminPort, '/stats')).body)) as { requests: number, cache: { entries: number } }
    assert.ok(stats.requests >= 0)
    env.mapweb.handler = (_req, res) => {
      res.writeHead(200, { 'content-type': 'image/png', 'cache-control': 'public, max-age=300' })
      res.end('x')
    }
    await env.get('/tiles/a/b/0_0/1.png')
    assert.equal((await env.cacheStats()).entries, 1)
    assert.equal((await request(env.edge.adminPort, '/purge', { method: 'POST' })).status, 200)
    assert.equal((await env.cacheStats()).entries, 0)
    // 公開のポートには出ない
    assert.equal((await env.get('/healthz')).status, 404)
    assert.equal((await env.get('/stats')).status, 404)
  })
})

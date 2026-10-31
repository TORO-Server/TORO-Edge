// nginx (古い入り口) と edge (新しい入り口) に、同じ URL を同じ Host で聞いて、返事の違いを集める。
// 使い方 (Docker の minecraft-network の中で):
//   node dist/tools/parity.js --old toro-nginx:80 --new toro-edge:80 --paths paths.txt
// paths.txt は 1 行 1 つ。"<パス>" か "<Host> <パス>"。Host を書かないものは、3 つのサイトすべてで聞く
import http from 'node:http'
import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { brotliDecompressSync, gunzipSync, inflateSync } from 'node:zlib'

const args = new Map<string, string>()
for (let i = 2; i < process.argv.length; i += 2) args.set(process.argv[i]!.replace(/^--/, ''), process.argv[i + 1] ?? '')
const oldTarget = args.get('old') ?? 'toro-nginx:80'
const newTarget = args.get('new') ?? 'toro-edge:80'
const pathsFile = args.get('paths')
if (!pathsFile) throw new Error('--paths <file>')

const HOSTS = ['map.torosaba.net', '3d-map.torosaba.net', 'assets.torosaba.net']
/** 返事が刻々と変わる (ライブ情報、更新) ので、本文は比べない */
const VOLATILE = /\/live\/|^\/up\/(?:world|configuration)|^\/maps\/[^/]+\/settings\.json$/
/** 違って当然の頭書き */
const IGNORE = new Set([
  'date', 'server', 'connection', 'keep-alive', 'content-length', 'transfer-encoding', 'etag', 'last-modified', 'vary', 'x-edge-cache',
  'accept-ranges', 'x-powered-by', 'age', 'content-encoding',
])

interface Reply {
  status: number
  headers: http.IncomingHttpHeaders
  body: Buffer
}

function get(target: string, host: string, path: string, encoding: string): Promise<Reply> {
  const [h, p] = target.split(':')
  return new Promise((resolve, reject) => {
    const req = http.request({ host: h, port: Number(p ?? 80), path, headers: { host, 'accept-encoding': encoding }, agent: false }, (res) => {
      const chunks: Buffer[] = []
      res.on('data', (c: Buffer) => chunks.push(c))
      res.on('end', () => resolve({ status: res.statusCode ?? 0, headers: res.headers, body: Buffer.concat(chunks) }))
      res.on('error', reject)
    })
    req.setTimeout(20_000, () => req.destroy(new Error('timeout')))
    req.on('error', reject)
    req.end()
  })
}

function decode(r: Reply): Buffer {
  try {
    switch (r.headers['content-encoding']) {
      case 'br': return brotliDecompressSync(r.body)
      case 'gzip': return gunzipSync(r.body)
      case 'deflate': return inflateSync(r.body)
      default: return r.body
    }
  }
  catch {
    return r.body
  }
}

const sha = (b: Buffer): string => createHash('sha1').update(b).digest('hex').slice(0, 12)
const mediaType = (v: string | string[] | undefined): string => String(v ?? '').split(';')[0]!.trim().toLowerCase()

const targets: [string, string][] = []
for (const line of readFileSync(pathsFile, 'utf8').split('\n')) {
  const t = line.trim()
  if (!t || t.startsWith('#')) continue
  const parts = t.split(/\s+/)
  if (parts.length >= 2 && parts[0]!.includes('.')) targets.push([parts[0]!, parts[1]!])
  else for (const h of HOSTS) targets.push([h, parts[0]!])
}

const headerDiffs = new Map<string, number>()
const examples = new Map<string, string>()
const statusDiffs: string[] = []
const bodyDiffs: string[] = []
const errors: string[] = []
let compared = 0
let identical = 0

for (const [host, path] of targets) {
  for (const encoding of ['gzip, br', 'identity']) {
    let a: Reply
    let b: Reply
    try {
      ;[a, b] = await Promise.all([get(oldTarget, host, path, encoding), get(newTarget, host, path, encoding)])
    }
    catch (e) {
      errors.push(`${host} ${path}: ${(e as Error).message}`)
      continue
    }
    compared++
    let same = true
    if (a.status !== b.status) {
      statusDiffs.push(`${host} ${path} [${encoding}]: nginx ${a.status} → edge ${b.status}`)
      continue
    }
    if (a.status === 200 || a.status === 204) {
      if (mediaType(a.headers['content-type']) !== mediaType(b.headers['content-type'])) {
        same = false
        const k = `content-type: ${mediaType(a.headers['content-type'])} → ${mediaType(b.headers['content-type'])}`
        headerDiffs.set(k, (headerDiffs.get(k) ?? 0) + 1)
        examples.set(k, `${host}${path}`)
      }
      if (!VOLATILE.test(path)) {
        const da = decode(a)
        const db = decode(b)
        if (sha(da) !== sha(db)) {
          same = false
          bodyDiffs.push(`${host} ${path} [${encoding}]: nginx ${da.length}B ${sha(da)} → edge ${db.length}B ${sha(db)}`)
        }
      }
    }
    const names = new Set([...Object.keys(a.headers), ...Object.keys(b.headers)])
    for (const n of names) {
      if (IGNORE.has(n) || n === 'content-type') continue
      const va = String(a.headers[n] ?? '(none)')
      const vb = String(b.headers[n] ?? '(none)')
      if (va !== vb) {
        same = false
        const k = `${n}: ${va.slice(0, 90)} → ${vb.slice(0, 90)}`
        headerDiffs.set(k, (headerDiffs.get(k) ?? 0) + 1)
        examples.set(k, `${a.status} ${host}${path}`)
      }
    }
    if (same) identical++
  }
}

console.log(`compared ${compared} (${targets.length} URL × 2 encodings): identical ${identical}`)
console.log(`\nstatus differences: ${statusDiffs.length}`)
for (const s of statusDiffs.slice(0, 40)) console.log(`  ${s}`)
console.log(`\nbody differences: ${bodyDiffs.length}`)
for (const s of bodyDiffs.slice(0, 40)) console.log(`  ${s}`)
console.log(`\nheader differences (distinct): ${headerDiffs.size}`)
for (const [k, n] of [...headerDiffs].sort((x, y) => y[1] - x[1]).slice(0, 60)) console.log(`  ×${n}  ${k}   e.g. ${examples.get(k)}`)
console.log(`\nerrors: ${errors.length}`)
for (const s of errors.slice(0, 20)) console.log(`  ${s}`)

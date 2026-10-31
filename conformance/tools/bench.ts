// 入り口 (nginx か edge) に負荷をかけて、毎秒の数と待ち時間を測る。
// 使い方 (Docker の minecraft-network の中で):
//   node dist/tools/bench.js --target toro-edge:80 --label edge --scenarios scenarios.json [--seconds 8] [--connections 64]
// scenarios.json: [{ "name": "...", "host": "map.torosaba.net", "path": "/css/dynmap_style.css", "headers": { ... } }, ...]
//   "paths": [...] を書くと、その中から順に取る。"conditional": true なら、先に 1 度取って ETag を If-None-Match に付ける (304 の経路)
import http from 'node:http'
import { readFileSync } from 'node:fs'
import autocannon from 'autocannon'

const args = new Map<string, string>()
for (let i = 2; i < process.argv.length; i += 2) args.set(process.argv[i]!.replace(/^--/, ''), process.argv[i + 1] ?? '')
const target = args.get('target') ?? 'toro-edge:80'
const label = args.get('label') ?? target
const seconds = Number(args.get('seconds') ?? 8)
const connections = Number(args.get('connections') ?? 64)
const only = args.get('only')
const [targetHost, targetPort] = target.split(':')

interface Scenario {
  name: string
  host: string
  path?: string
  paths?: string[]
  headers?: Record<string, string>
  conditional?: boolean
}

const scenarios = JSON.parse(readFileSync(args.get('scenarios') ?? 'scenarios.json', 'utf8')) as Scenario[]

function etagOf(host: string, path: string, headers: Record<string, string>): Promise<string | undefined> {
  return new Promise((resolve) => {
    const req = http.request({ host: targetHost, port: Number(targetPort ?? 80), path, headers: { host, ...headers }, agent: false }, (res) => {
      res.resume()
      res.on('end', () => resolve(res.headers.etag))
    })
    req.on('error', () => resolve(undefined))
    req.end()
  })
}

console.log(`target=${target} label=${label} connections=${connections} seconds=${seconds}`)
console.log('scenario'.padEnd(26), 'req/s'.padStart(9), 'p50 ms'.padStart(8), 'p97.5'.padStart(8), 'p99'.padStart(8), 'max'.padStart(8), 'MB/s'.padStart(8), '2xx/3xx'.padStart(10), 'non2xx'.padStart(8), 'errors'.padStart(7))
const rows: Record<string, unknown>[] = []
for (const s of scenarios) {
  if (only && !s.name.includes(only)) continue
  const paths = s.paths ?? [s.path!]
  const headers: Record<string, string> = { host: s.host, ...s.headers }
  const requests: Record<string, unknown>[] = []
  for (const path of paths) {
    const h = { ...headers }
    if (s.conditional) {
      const etag = await etagOf(s.host, path, headers)
      if (etag) h['if-none-match'] = etag
    }
    requests.push({ method: 'GET', path, headers: h })
  }
  // 温める (最初の 1 回の遅さを測らない)
  for (const path of paths) await etagOf(s.host, path, headers)
  const r = await autocannon({
    url: `http://${targetHost}:${targetPort ?? 80}`,
    connections,
    duration: seconds,
    workers: 2,
    requests,
    timeout: 20,
  })
  const lat = r.latency as Record<string, number>
  const non2xx = r.non2xx as number
  const ok = (r['2xx'] as number) + ((r['3xx'] as number) ?? 0)
  console.log(
    s.name.padEnd(26),
    String(Math.round((r.requests as Record<string, number>).average!)).padStart(9),
    String(lat.p50).padStart(8),
    String(lat.p97_5).padStart(8),
    String(lat.p99).padStart(8),
    String(lat.max).padStart(8),
    ((r.throughput as Record<string, number>).average! / 1048576).toFixed(1).padStart(8),
    String(ok).padStart(10),
    String(non2xx).padStart(8),
    String(r.errors).padStart(7),
  )
  rows.push({ label, scenario: s.name, total: (r.requests as Record<string, number>).total, rps: (r.requests as Record<string, number>).average, p50: lat.p50, p975: lat.p97_5, p99: lat.p99, max: lat.max, mbps: (r.throughput as Record<string, number>).average! / 1048576, ok, non2xx, errors: r.errors })
}
console.log(`RESULT ${JSON.stringify(rows)}`)

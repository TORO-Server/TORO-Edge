// 空のキャッシュに、同じ URL の一覧を 1 回ずつ取って、かかった時間を測る (元に聞きに行く経路。ヒットではない)。
// 使い方: node dist/tools/coldpass.js --target toro-edge:80 --scenarios scenarios.json --only mix [--concurrency 8]
// 先に、キャッシュを空にしておく (edge は POST /purge、nginx は空のキャッシュで起動し直す)
import http from 'node:http'
import { readFileSync } from 'node:fs'

const args = new Map<string, string>()
for (let i = 2; i < process.argv.length; i += 2) args.set(process.argv[i]!.replace(/^--/, ''), process.argv[i + 1] ?? '')
const [host, port] = (args.get('target') ?? 'toro-edge:80').split(':')
const concurrency = Number(args.get('concurrency') ?? 8)
const only = args.get('only') ?? 'mix'

interface Scenario { name: string, host: string, path?: string, paths?: string[], headers?: Record<string, string> }
const scenario = (JSON.parse(readFileSync(args.get('scenarios') ?? 'scenarios.json', 'utf8')) as Scenario[]).find(s => s.name.includes(only))
if (!scenario) throw new Error(`no scenario ${only}`)
const paths = scenario.paths ?? [scenario.path!]
const agent = new http.Agent({ keepAlive: true, maxSockets: concurrency })

function get(path: string): Promise<number> {
  const start = performance.now()
  return new Promise((resolve, reject) => {
    const req = http.request({ host, port: Number(port), path, headers: { host: scenario!.host, ...scenario!.headers }, agent }, (res) => {
      res.resume()
      res.on('end', () => resolve(performance.now() - start))
    })
    req.on('error', reject)
    req.end()
  })
}

const times: number[] = []
let next = 0
const begin = performance.now()
await Promise.all(Array.from({ length: concurrency }, async () => {
  while (next < paths.length) times.push(await get(paths[next++]!))
}))
const total = performance.now() - begin
times.sort((a, b) => a - b)
const q = (p: number): number => times[Math.min(times.length - 1, Math.floor(times.length * p))]!
console.log(`RESULT ${JSON.stringify({ target: args.get('target'), n: paths.length, concurrency, totalMs: +total.toFixed(1), p50: +q(0.5).toFixed(1), p99: +q(0.99).toFixed(1), max: +times[times.length - 1]!.toFixed(1) })}`)
agent.destroy()

import { spawn } from 'node:child_process'
import http from 'node:http'
import type { IncomingHttpHeaders, IncomingMessage, ServerResponse } from 'node:http'
import type { AddressInfo, Socket } from 'node:net'
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join, resolve as resolvePath } from 'node:path'

/** 試験で変える設定 (そのまま EDGE_* の環境変数になる) */
export interface Config {
  mapWeb: { host: string, port: number }
  blueMapLive: { host: string, port: number }
  dynmapWeb: string
  blueMapWeb: string
  cacheBytes: number
  maxEntityBytes: number
  /** 省略は実装の既定。0 なら、大きい本文も write で送る */
  sendfileMin?: number
}

/** 試験の相手の実行ファイル。EDGE_BIN で指す (省略は、リポジトリの直下でビルドした ./toro-edge) */
const EDGE_BIN = resolvePath(process.env.EDGE_BIN ?? '../toro-edge')

export interface Seen {
  url: string
  headers: IncomingHttpHeaders
}

/** 元のサーバーの代わり。handler は試験ごとに差し替える */
export class Fake {
  hits = new Map<string, number>()
  seen: Seen[] = []
  handler: (req: IncomingMessage, res: ServerResponse) => void = (_req, res) => {
    res.writeHead(404)
    res.end()
  }

  readonly server = http.createServer((req, res) => {
    const url = req.url ?? ''
    this.hits.set(url, (this.hits.get(url) ?? 0) + 1)
    this.seen.push({ url, headers: req.headers })
    this.handler(req, res)
  })

  port = 0
  /** Upgrade したあとのつなぎは http のサーバーが数えないので、自分で覚えて閉じる */
  private readonly sockets = new Set<Socket>()

  async start(): Promise<this> {
    this.server.on('connection', (s) => {
      this.sockets.add(s)
      s.once('close', () => this.sockets.delete(s))
    })
    await new Promise<void>(resolve => this.server.listen(0, '127.0.0.1', resolve))
    this.port = (this.server.address() as AddressInfo).port
    return this
  }

  hitCount(url: string): number {
    return this.hits.get(url) ?? 0
  }

  async close(): Promise<void> {
    this.server.closeAllConnections()
    for (const s of this.sockets) s.destroy()
    await new Promise<void>(resolve => this.server.close(() => resolve()))
  }
}

export interface Reply {
  status: number
  headers: IncomingHttpHeaders
  body: Buffer
}

export interface RequestOptions {
  host?: string
  method?: string
  headers?: Record<string, string>
}

export function request(port: number, path: string, o: RequestOptions = {}): Promise<Reply> {
  return new Promise((resolve, reject) => {
    const req = http.request({
      host: '127.0.0.1',
      port,
      path,
      method: o.method ?? 'GET',
      headers: { host: o.host ?? 'map.torosaba.net', ...o.headers },
      agent: false,
    }, (res) => {
      const chunks: Buffer[] = []
      res.on('data', (c: Buffer) => chunks.push(c))
      res.on('end', () => resolve({ status: res.statusCode ?? 0, headers: res.headers, body: Buffer.concat(chunks) }))
      res.on('error', reject)
    })
    req.on('error', reject)
    req.end()
  })
}

export interface EdgeHandle {
  port: number
  adminPort: number
  close: () => Promise<void>
}

/** 実行ファイルを、試験用の設定で起動する (実装の言語によらない適合試験) */
async function startBinary(bin: string, config: Config): Promise<EdgeHandle> {
  const child = spawn(bin, [], {
    env: {
      PATH: process.env.PATH ?? '',
      EDGE_PORT: '0',
      EDGE_ADMIN_PORT: '0',
      EDGE_MAPWEB: `${config.mapWeb.host}:${config.mapWeb.port}`,
      EDGE_BLUEMAP_LIVE: `${config.blueMapLive.host}:${config.blueMapLive.port}`,
      EDGE_DYNMAP_WEB: config.dynmapWeb,
      EDGE_BLUEMAP_WEB: config.blueMapWeb,
      EDGE_ACCESS_LOG: '0',
      EDGE_DEBUG_HEADERS: '1',
      EDGE_CACHE_BYTES: String(config.cacheBytes),
      EDGE_MAX_ENTITY_BYTES: String(config.maxEntityBytes),
      EDGE_SHUTDOWN_GRACE_MS: '200',
      ...(config.sendfileMin === undefined ? {} : { EDGE_SENDFILE_MIN: String(config.sendfileMin) }),
    },
    stdio: ['ignore', 'pipe', 'inherit'],
  })
  const exited = new Promise<void>(resolve => child.once('exit', () => resolve()))
  const ports = await new Promise<{ port: number, adminPort: number }>((resolve, reject) => {
    let out = ''
    const timer = setTimeout(() => reject(new Error(`${bin} did not start: ${out}`)), 10_000)
    child.stdout!.on('data', (d: Buffer) => {
      out += d.toString()
      const m = /listening on :(\d+) \(admin :(\d+)\)/.exec(out)
      if (m) {
        clearTimeout(timer)
        resolve({ port: Number(m[1]), adminPort: Number(m[2]) })
      }
    })
    child.once('exit', code => reject(new Error(`${bin} exited early (${code}): ${out}`)))
  })
  return {
    ...ports,
    async close() {
      child.kill('SIGTERM')
      const kill = setTimeout(() => child.kill('SIGKILL'), 3000)
      await exited
      clearTimeout(kill)
    },
  }
}

export interface Env {
  edge: EdgeHandle
  /** キャッシュにあるものの数と大きさ (管理用のポートの /stats) */
  cacheStats: () => Promise<{ entries: number, bytes: number }>
  mapweb: Fake
  live: Fake
  dynDir: string
  blueDir: string
  get: (path: string, o?: RequestOptions) => Promise<Reply>
  blue: (path: string, o?: RequestOptions) => Promise<Reply>
  assets: (path: string, o?: RequestOptions) => Promise<Reply>
}

export const SETTINGS_JSON = '{"version":"5.x","scripts":["a.js"],"maps":["overworld"]}'
export const INDEX_HTML = '<html><head>\n\t<script type="text/javascript" src="standalone/config.js"></script>\n</head></html>\n'
export const CSS = `${'.dynmap { color: red; margin: 0 auto; }\n'.repeat(120)}`

async function put(root: string, rel: string, content: string | Buffer): Promise<void> {
  const file = join(root, rel)
  await mkdir(dirname(file), { recursive: true })
  await writeFile(file, content)
}

/** 試験用のフォルダ・元のサーバー・edge をひとまとめに用意して、終わったら片づける */
export async function withEnv(fn: (env: Env) => Promise<void>, overrides: { cacheBytes?: number, maxEntityBytes?: number, sendfileMin?: number } = {}): Promise<void> {
  const root = await mkdtemp(join(tmpdir(), 'edge-test-'))
  const dynDir = join(root, 'dynmap')
  const blueDir = join(root, 'bluemap')
  await put(dynDir, 'index.html', INDEX_HTML)
  await put(dynDir, 'css/style.css', CSS)
  await put(dynDir, 'images/blank.png', Buffer.from([0x89, 0x50, 0x4E, 0x47, 1, 2, 3, 4]))
  await put(dynDir, 'standalone/config.js', 'var config = {}')
  await put(dynDir, 'standalone/MySQL_config.php', '<?php $password = "secret"; ?>')
  await put(dynDir, '.secret', 'hidden')
  await put(root, 'outside.txt', 'outside the root')
  await put(blueDir, 'index.html', '<html>old bluemap</html>')
  await put(blueDir, 'settings.json', SETTINGS_JSON)
  await put(blueDir, 'lang/settings.conf', 'default: "en"\nother: "en"\n')
  await put(blueDir, 'marumasign/overworld.json', '{"signs":[]}')
  await put(blueDir, 'assets/a.png', Buffer.from([1, 2, 3]))
  await put(blueDir, 'maps/hidden.json', '{"old":true}')
  const mapweb = await new Fake().start()
  const live = await new Fake().start()
  const config: Config = {
    mapWeb: { host: '127.0.0.1', port: mapweb.port },
    blueMapLive: { host: '127.0.0.1', port: live.port },
    dynmapWeb: dynDir,
    blueMapWeb: blueDir,
    cacheBytes: overrides.cacheBytes ?? 64 * 1024 * 1024,
    maxEntityBytes: overrides.maxEntityBytes ?? 8 * 1024 * 1024,
    sendfileMin: overrides.sendfileMin,
  }
  const edge = await startBinary(EDGE_BIN, config)
  const env: Env = {
    edge,
    cacheStats: async () => (JSON.parse((await request(edge.adminPort, '/stats')).body.toString()) as { cache: { entries: number, bytes: number } }).cache,
    mapweb,
    live,
    dynDir,
    blueDir,
    get: (path, o) => request(edge.port, path, o),
    blue: (path, o) => request(edge.port, path, { host: '3d-map.torosaba.net', ...o }),
    assets: (path, o) => request(edge.port, path, { host: 'assets.torosaba.net', ...o }),
  }
  try {
    await fn(env)
  }
  finally {
    await edge.close()
    await mapweb.close()
    await live.close()
    await rm(root, { recursive: true, force: true })
  }
}

export const sleep = (ms: number): Promise<void> => new Promise(resolve => setTimeout(resolve, ms))

/** 試験用の WebSocket の上流 (つないだら 101 を返し、届いたものをそのまま返す) */
export function echoUpgrade(fake: Fake): void {
  fake.server.on('upgrade', (req, duplex) => {
    const socket = duplex as Socket
    fake.seen.push({ url: req.url ?? '', headers: req.headers })
    socket.write('HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: test\r\n\r\n')
    socket.on('data', d => socket.write(d))
    // http のサーバーが作るつなぎは半分閉じを許すので、相手が閉じたら自分も閉じる
    socket.on('end', () => socket.end())
    socket.on('error', () => {})
  })
}

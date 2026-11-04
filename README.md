# toro-edge

map.torosaba.net (Dynmap)、3d-map.torosaba.net (BlueMap)、assets.torosaba.net の入り口になる、自前のリバースプロキシです。
Go 製。標準ライブラリと brotli のライブラリだけで、イメージは 8 MB ほどです。

```
ブラウザ → Cloudflare → cloudflared ─→ toro-edge:80 ─┬─ ファイルをメモリから返す (Dynmap の web/、BlueMap の web/)
                                                       ├─ toro-map-web:3000 (Nuxt。TiDB の地図のデータ、画面) ← メモリにためる
                                                       └─ toro-main:8100 (BlueMap のライブ情報、SSE)
```

## イメージ

GitHub Actions が、試験に通ったものだけを GHCR に置きます (linux/amd64 と linux/arm64)。
リポジトリもイメージ (`ghcr.io/toro-server/toro-edge`) も非公開です。使うサーバーでは、先に `read:packages` だけの Personal access token (classic) で入っておきます。

```bash
echo "<read:packages だけの PAT>" | docker login ghcr.io -u <GitHub のユーザー名> --password-stdin
docker pull ghcr.io/toro-server/toro-edge:1.0.0
```

タグは、`latest` (main の先頭)、`sha-<短い ID>`、リリース (`v1.2.3` のタグを打つと `1.2.3` と `1.2`) です。サーバーでは、リリースのタグに固定して使います。

```yaml
# docker-compose.yaml
  toro-edge:
    image: ghcr.io/toro-server/toro-edge:1.0.0
    container_name: toro-edge
    user: "1000:1001"
    restart: unless-stopped
    read_only: true
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    mem_limit: 3g
    stop_grace_period: 15s
    volumes:
      - "./main/plugins/dynmap/web:/srv/dynmap-web:ro"
      - "./main/bluemap/web:/srv/bluemap-web:ro"
    environment:
      - EDGE_MAPWEB=toro-map-web:3000
      - EDGE_BLUEMAP_LIVE=toro-main:8100
      - EDGE_CACHE_MB=2048
    healthcheck:
      test: ["CMD", "/toro-edge", "healthcheck"]
      interval: 15s
      timeout: 3s
      retries: 3
      start_period: 5s
    networks: [minecraft-network]
```

`latest` は main の最新、`vX.Y.Z` はタグ、`sha-<7 桁>` はコミットです。イメージには `healthcheck` のサブコマンドが入っています (`wget` などは入っていません)。

## 設定 (環境変数)

| 変数 | 既定 | |
|---|---|---|
| `EDGE_PORT` | 80 | 公開のポート |
| `EDGE_ADMIN_PORT` | 8081 | 管理用のポート (`/healthz` `/stats` `POST /purge`)。Docker の中だけ。Tunnel には出さない |
| `EDGE_MAPWEB` | `toro-map-web:3000` | toro-map-web (Nuxt) |
| `EDGE_BLUEMAP_LIVE` | `toro-main:8100` | BlueMap 内蔵の Web サーバー (ライブ情報、SSE) |
| `EDGE_DYNMAP_WEB` / `EDGE_BLUEMAP_WEB` | `/srv/dynmap-web` / `/srv/bluemap-web` | ファイルのまま返すフォルダ (読むだけ) |
| `EDGE_HOST_MAP` / `EDGE_HOST_BLUEMAP` / `EDGE_HOST_ASSETS` | 3 つの名前 | サイトごとの Host (カンマ区切りで複数) |
| `EDGE_CACHE_MB` | 2048 | 地図のデータをためる上限 (MiB)。1 つの返事が 1/8 (最大 32 MiB) を超えたら、ためずに流す |
| `EDGE_ACCESS_LOG` | 1 | `0` でアクセスログを出さない |
| `EDGE_DEBUG_HEADERS` | 0 | `1` で返事に `x-edge-cache` を付ける (試験用) |
| `EDGE_SHUTDOWN_GRACE_MS` | 10000 | 止めるとき、返し終えていないものを待つ時間 |
| `EDGE_SENDFILE_MIN` | 65536 | この大きさ (バイト) 以上の本文を、メモリ上のファイル (memfd) から `sendfile` で送る。`0` で使わない (Linux だけ) |
| `EDGE_PPROF` | 0 | `1` で管理用のポートに `/debug/pprof/` を出す |
| `GOMEMLIMIT` / `GOGC` | `2600MiB` / `200` | Go のメモリの目安 (イメージで設定済み) |

## 経路の決まり

サイトごとの location は `sites.go` にあります (`first` → 完全一致 → 上から順の最初)。

- `proxied(...)`: toro-map-web の返事をためる。`fixedTtl` (元の Cache-Control を無視する長さ)、`defaultTtl` / `maxTtl` (元の max-age に従う)、`swr` (古いまま返してよい長さ)、`errorTtl` (元が壊れているとき古いまま返してよい長さ)、`response` (頭書きの付け替え)
- `staticHandler(...)`: フォルダのファイル。`transform` で書き換える
- `streamHandler(...)`: ためずに流す (`/_marumasign/`、assets、BlueMap の SSE)
- `.php`、`/.` を含むパスは、ファイルがあっても 404。`..` を含むパスと、ルートの外へ出るシンボリックリンクも返さない

### 挙動

- 静的なファイルは起動時にメモリへ読み込み、brotli 11 / gzip 9 を 1 度だけ作る。ETag は内容から。変わったら約 1 秒で読み直す
- 地図のデータはメモリの LRU。古くなったら古いまま返し、裏で `If-None-Match` で確かめる (304 なら TiDB を読まない)。同じものを同時に聞かない。元が壊れていても、ためたものを返す
- BlueMap のライブ情報は 1 秒まとめる (toro-main に毎秒 1 回)
- WebSocket (`/up/ws`) と SSE (`live/sse`) は、解かずにそのまま流す
- 元に渡す頭書きは決めたものだけ。`X-Toro-Client-IP` を客が書いても渡さない (assets は Cloudflare の IP から作る)
- GET と HEAD だけ。本文のあるリクエストは断ってつなぎを閉じる。8 KB を超える URL は 414
- 付け足す頭書き (CSP など) は成功の返事にだけ付ける。404 は 2〜5 秒覚える。`/index.html` の 301 は相対の `Location`

## 作り

HTTP/1.1 のサーバーも、このプロキシのために作ってあります (`httpserver.go`)。Cloudflare と Docker の中の自分たちだけが相手なので、必要なものを厳しめに実装しています。

- 読む頭書きは決まったものだけ。折り返し (obs-fold)、LF だけの改行、重複した Content-Length / Host、頭書き名の空白、制御文字は 400。頭書きが 16 KB を超えたら 431、リクエストの行が長すぎたら 414
- 返事の頭書きは、返事ができたときに文字列にしてあり、返すときは `Date` を足して並べるだけ。頭書きと本文を 1 回の `writev` で送る (標準の `net/http` は 4 KB を超えると 2 回に分けるうえ、毎回頭書きを並べ替える)
- 64 KB 以上の本文は、メモリ上のファイル (`memfd_create`) にも置いて、`sendfile` で送る。`write` で送ると本文をカーネルへ写す分の CPU がかかる。ファイルは返事と同じ寿命で、捨てたあとも、送っている最中のものがあるので `2 × writeTimeout + 30 秒` 待ってから閉じる。`sendfile` が使えないときは、読んで `write` で送る
- キャッシュの検索は `sync.Map` (ヒットのときはロックなし、鍵も作り直さない)。経路の判定は正規表現を使わない。全コアが増やす数は、コアごとにばらして数える
- 流している返事 (動画、SSE) は、客が切れたら元も切る。止めるときは、新しいつなぎを受けず、返し終えていないものを待つ

| ファイル | |
|---|---|
| `httpserver.go` | HTTP/1.1 のサーバー (パース、つなぎ、止め方) |
| `respond.go` / `sendfile_*.go` | 返事の送り方 (writev、sendfile、チャンク、切れたかの見張り) |
| `sites.go` / `router.go` | 経路の決まり |
| `cached.go` / `cache.go` / `entity.go` | ためる (単一飛行、古いまま返す、304 で延ばす、圧縮した版) |
| `static.go` | ファイルのストア (読み込み、変更の検知、安全なパス) |
| `stream.go` / `upstream.go` / `ws.go` | ためずに流す、元への接続、WebSocket |
| `conformance/` | 実行ファイルを外から確かめる試験と、負荷の道具 (すべて Go) |

## 試験

```bash
go vet ./... && go test -race .               # 単体 (パーサーの異常系、パイプライン、チャンク、止め方 ほか)
go build -o toro-edge .
cd conformance && EDGE_BIN=../toro-edge go test -count=1 .   # 偽の元のサーバーを相手にした 24 件 (sendfile の 4 件を含む)
```

`conformance` の試験は実装の言語に依らず、`EDGE_BIN` に実行ファイルを渡すだけで流せます (省略すると、リポジトリの直下をビルドして使います)。
外の部品は、Go の標準ライブラリと、すでに使っている brotli だけです。
`conformance/tools/bench` と `cpuprobe.sh` は、負荷をかけて、入り口のコンテナが使った CPU を 1 リクエストあたりで測ります (`cpuprobe.sh` の最後の引数で、負荷をかける側の CPU と workers を増やせます)。`coldpass` は、空のキャッシュに同じ URL を 1 回ずつ取って、元に聞きに行く経路の時間を測ります。
これらの道具は、`docker build -f conformance/tools/Dockerfile -t toro-edge:tools .` で 1 つのイメージにできます。

## ライセンス

未設定です (非公開のリポジトリなので、TORO サーバーの中だけで使います。公開するときに決めます)。

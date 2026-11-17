# toro-edge

TORO の地図サイトと素材の配信サイトの入り口になる、自前のリバースプロキシ (外からの要求を受け、中の各サービスへ振り分ける中継サービス) です。Go で書かれています。標準ライブラリと brotli (圧縮の部品) だけを使っています。

静的なファイルはメモリから返し、地図のデータは [TORO-WebMap](https://github.com/TORO-Server/TORO-WebMap) の返事をメモリにためて返します。

## 動かしてみる

```bash
go vet ./... && go test .                 # 単体の試験
go build -o toro-edge .
cd conformance && EDGE_BIN=../toro-edge go test -count=1 .   # 実行ファイルを外から確かめる試験
```

設定できる環境変数の名前と既定値の正本は [config.go](config.go) です。イメージは、試験に通ったものだけが GitHub のイメージ置き場 (GHCR) に置かれます。使うバージョンは [Releases](https://github.com/TORO-Server/TORO-Edge/releases) のタグを見てください。

## 関連文書

- 経路の決まり、挙動、作り、試験の詳細: [docs/design.md](docs/design.md)
- サービス全体の構成と動かし方: [ServerImage-TORO の docs/architecture.md](https://github.com/TORO-Server/ServerImage-TORO/blob/main/docs/architecture.md)

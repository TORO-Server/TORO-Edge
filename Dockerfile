# toro-edge: map.torosaba.net / 3d-map.torosaba.net / assets.torosaba.net の入り口 (自前のリバースプロキシ)
#
#   docker build -t toro-edge .
#   docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/<owner>/toro-edge .
#
# ビルドは常にビルドする側の CPU で行い (--platform=$BUILDPLATFORM)、Go の交差コンパイルで amd64 / arm64 を作る
# (QEMU の中でコンパイルしない。速く、結果も同じ)

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS compile
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN go vet ./... \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /toro-edge .

# 単体の試験 (ビルドする側の CPU で動かす)
FROM --platform=$BUILDPLATFORM compile AS unit
RUN go test ./...

FROM scratch
# unit を経由する (単体の試験に通らないと、ここまで来ない)
COPY --from=unit /toro-edge /toro-edge
# 非 root で動かす (80 番は、Docker が非 root にも開けている)。読み込むだけのフォルダ (/srv/...) は、実行する側が渡す
USER 65532:65532
# GOMEMLIMIT: ためるメモリ (EDGE_CACHE_MB。既定 2048) に、圧縮した版と作業の分を足した、Go のメモリの目安
ENV GOMEMLIMIT=2600MiB GOGC=200 EDGE_PORT=80 EDGE_ADMIN_PORT=8081
EXPOSE 80
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 CMD ["/toro-edge", "healthcheck"]
ENTRYPOINT ["/toro-edge"]

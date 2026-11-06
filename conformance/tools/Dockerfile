# 負荷の道具 (bench / coldpass) を、1 つのイメージにする。リポジトリの直下から:
#
#   docker build -f conformance/tools/Dockerfile -t toro-edge:tools .
#
# cpuprobe.sh は、これを minecraft-network の中で動かす (負荷をかける側の CPU は --cpus で絞る)

FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN for t in bench coldpass; do \
      CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/$t ./conformance/tools/$t || exit 1; \
    done

FROM scratch
COPY --from=build /out/ /

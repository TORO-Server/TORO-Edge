//go:build linux && !amd64 && !arm64

package main

// 番号が分からない構成では、memfd を使わない (本文は write で送る)
const sysMemfdCreate = 0

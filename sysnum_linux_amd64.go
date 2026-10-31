//go:build linux && amd64

package main

// memfd_create のシステムコール番号 (標準の syscall には定義がない)
const sysMemfdCreate = 319

package main

import "strings"

var mimeTypes = map[string]string{
	"html":        "text/html; charset=utf-8",
	"htm":         "text/html; charset=utf-8",
	"css":         "text/css; charset=utf-8",
	"js":          "text/javascript; charset=utf-8",
	"mjs":         "text/javascript; charset=utf-8",
	"json":        "application/json",
	"map":         "application/json",
	"webmanifest": "application/manifest+json",
	"txt":         "text/plain; charset=utf-8",
	"conf":        "text/plain; charset=utf-8",
	"xml":         "application/xml",
	"svg":         "image/svg+xml",
	"png":         "image/png",
	"jpg":         "image/jpeg",
	"jpeg":        "image/jpeg",
	"gif":         "image/gif",
	"webp":        "image/webp",
	"avif":        "image/avif",
	"ico":         "image/x-icon",
	"wasm":        "application/wasm",
	"woff":        "font/woff",
	"woff2":       "font/woff2",
	"ttf":         "font/ttf",
	"otf":         "font/otf",
	"mp3":         "audio/mpeg",
	"ogg":         "audio/ogg",
	"wav":         "audio/wav",
	"mp4":         "video/mp4",
	"webm":        "video/webm",
	"glb":         "model/gltf-binary",
	"gltf":        "model/gltf+json",
	"pdf":         "application/pdf",
	"zip":         "application/zip",
	"aspx":        "text/plain; charset=utf-8",
}

func mimeType(file string) string {
	dot := strings.LastIndexByte(file, '.')
	if dot < 0 || strings.LastIndexByte(file, '/') > dot {
		return "application/octet-stream"
	}
	if t, ok := mimeTypes[strings.ToLower(file[dot+1:])]; ok {
		return t
	}
	return "application/octet-stream"
}

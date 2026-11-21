package main

import (
	"path"
	"strings"
)

// Built-in Content-Type table; fixed so responses do not depend on the host's /etc/mime.types.
var builtinTypes = map[string]string{
	// documents and code
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".xml":         "application/xml",
	".rss":         "application/rss+xml",
	".atom":        "application/atom+xml",
	".txt":         "text/plain; charset=utf-8",
	".md":          "text/markdown; charset=utf-8",
	".csv":         "text/csv; charset=utf-8",
	".vtt":         "text/vtt; charset=utf-8",
	".wasm":        "application/wasm",
	".pdf":         "application/pdf",
	// images
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".ico":  "image/x-icon",
	// fonts
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	// audio and video
	".mp3":  "audio/mpeg",
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".opus": "audio/ogg",
	".wav":  "audio/wav",
	".flac": "audio/flac",
	".m4a":  "audio/mp4",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".ogv":  "video/ogg",
	// archives
	".gz":  "application/gzip",
	".zst": "application/zstd",
	".zip": "application/zip",
	".tar": "application/x-tar",
}

const defaultType = "application/octet-stream"

// contentType looks the extension of name up in overrides, then in the built-in table,
// and returns fallback for any other extension or none.
func contentType(name string, overrides map[string]string, fallback string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := overrides[ext]; ok {
		return t
	}
	if t, ok := builtinTypes[ext]; ok {
		return t
	}
	return fallback
}

// Non-text media types that compress well.
var compressibleTypes = map[string]bool{
	"application/javascript": true,
	"application/json":       true,
	"application/xml":        true,
	"application/wasm":       true,
	"font/ttf":               true,
	"font/otf":               true,
	"image/x-icon":           true,
}

// compressible reports whether a gzip variant is worth trying for the media type.
func compressible(ctype string) bool {
	base, _, _ := strings.Cut(ctype, ";")
	base = strings.ToLower(strings.TrimSpace(base))
	return strings.HasPrefix(base, "text/") ||
		strings.HasSuffix(base, "+json") || strings.HasSuffix(base, "+xml") ||
		compressibleTypes[base]
}

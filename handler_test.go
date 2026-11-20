package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var (
	bundle = strings.Repeat("console.log('hello, compressible world');\n", 200)
	index  = "<!doctype html><title>app</title>" + strings.Repeat("<!-- padding -->", 20)
)

// writeTree creates files (relative path to content) under a fresh directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func spaConfig(root string) Config {
	return Config{
		Root:         root,
		Fallback:     "/index.html",
		Immutable:    []string{"/assets/"},
		CacheControl: "no-cache",
		Headers:      http.Header{"Referrer-Policy": {"no-referrer"}},
	}
}

func spaTree() map[string]string {
	return map[string]string{
		"index.html":                 index,
		"config.json":                `{"homeserver":"example.org"}`,
		"assets/app-abc123.js":       bundle,
		"docs/index.html":            "<p>docs</p>",
		".well-known/security.txt":   "Contact: mailto:security@example.org\n",
		"public/locales/en.json":     `{"hello":"Hello"}`,
		"public/models/model.tar.gz": "\x1f\x8b not really gzip",
	}
}

func load(t *testing.T, cfg Config) *Site {
	t.Helper()
	site, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { site.Close() })
	return site
}

func do(site *Site, method, target string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	site.ServeHTTP(rec, req)
	return rec
}

func assertResponse(t *testing.T, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d", rec.Code, status)
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("body = %.60q, want %.60q", got, body)
	}
}

func TestServesExistingFiles(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	for _, tc := range []struct{ path, body, ctype string }{
		{"/config.json", `{"homeserver":"example.org"}`, "application/json"},
		{"/assets/app-abc123.js", bundle, "text/javascript; charset=utf-8"},
		{"/.well-known/security.txt", "Contact: mailto:security@example.org\n", "text/plain; charset=utf-8"},
		{"/public/models/model.tar.gz", "\x1f\x8b not really gzip", "application/gzip"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := do(site, "GET", tc.path)
			assertResponse(t, rec, 200, tc.body)
			h := rec.Header()
			if h.Get("Content-Type") != tc.ctype {
				t.Errorf("Content-Type = %q, want %q", h.Get("Content-Type"), tc.ctype)
			}
			if h.Get("Content-Encoding") != "" {
				t.Errorf("Content-Encoding = %q without Accept-Encoding", h.Get("Content-Encoding"))
			}
			if h.Get("Etag") == "" || h.Get("Last-Modified") != "" {
				t.Errorf("want a content ETag and no Last-Modified, got %v", h)
			}
		})
	}
}

func TestCachePolicy(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	for path, want := range map[string]string{
		"/assets/app-abc123.js":   immutableCacheControl,
		"/index.html":             "no-cache",
		"/some/client/route":      "no-cache",
		"/assets/missing-old.js":  "no-cache",
		"/public/locales/en.json": "no-cache",
	} {
		if got := do(site, "GET", path).Header().Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control = %q, want %q", path, got, want)
		}
	}
}

func TestFallbackServesDocumentForMissingPaths(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	for _, path := range []string{"/", "/room/!abc:example.org", "/favicon.ico", "/public/missing.png", "/config.json/"} {
		rec := do(site, "GET", path)
		assertResponse(t, rec, 200, index)
		if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type = %q", path, ct)
		}
	}
}

func TestMissingImmutableFileIsNotFound(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	rec := do(site, "GET", "/assets/app-old999.js")
	if rec.Code != 404 || strings.Contains(rec.Body.String(), "<title>app") {
		t.Errorf("got %d %q, want a 404 that is not the fallback", rec.Code, rec.Body.String())
	}
}

func TestNotFoundDocument(t *testing.T) {
	tree := spaTree()
	tree["404.html"] = "<p>gone</p>"
	cfg := spaConfig(writeTree(t, tree))
	cfg.Fallback = ""
	cfg.NotFound = "/404.html"
	site := load(t, cfg)

	assertResponse(t, do(site, "GET", "/nope"), 404, "<p>gone</p>")
	rec := do(site, "HEAD", "/nope")
	assertResponse(t, rec, 404, "")
	if rec.Header().Get("Content-Length") != "11" {
		t.Errorf("HEAD Content-Length = %q", rec.Header().Get("Content-Length"))
	}
	assertResponse(t, do(site, "GET", "/config.json"), 200, `{"homeserver":"example.org"}`)
}

func TestWithoutDocumentsMissingIsPlain404(t *testing.T) {
	cfg := spaConfig(writeTree(t, spaTree()))
	cfg.Fallback = ""
	site := load(t, cfg)
	if rec := do(site, "GET", "/nope"); rec.Code != 404 {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestDirectoryIndex(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	assertResponse(t, do(site, "GET", "/docs/"), 200, "<p>docs</p>")
	assertResponse(t, do(site, "GET", "/"), 200, index)

	rec := do(site, "GET", "/docs?page=2")
	if rec.Code != 301 || rec.Header().Get("Location") != "/docs/?page=2" {
		t.Errorf("got %d Location %q, want 301 to /docs/?page=2", rec.Code, rec.Header().Get("Location"))
	}
}

func TestConditionalRequests(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	first := do(site, "GET", "/assets/app-abc123.js", "Accept-Encoding", "gzip")
	etag := first.Header().Get("Etag")

	rec := do(site, "GET", "/assets/app-abc123.js", "Accept-Encoding", "gzip", "If-None-Match", etag)
	assertResponse(t, rec, 304, "")
	h := rec.Header()
	if h.Get("Etag") != etag || h.Get("Cache-Control") != immutableCacheControl || h.Get("Vary") != "Accept-Encoding" {
		t.Errorf("304 must keep cache headers, got %v", h)
	}

	// The gzip ETag does not validate the identity representation.
	if rec := do(site, "GET", "/assets/app-abc123.js", "If-None-Match", etag); rec.Code != 200 {
		t.Errorf("identity with gzip ETag: status = %d, want 200", rec.Code)
	}
	// A client route revalidates against the fallback document.
	fallbackTag := do(site, "GET", "/room/1").Header().Get("Etag")
	if rec := do(site, "GET", "/room/2", "If-None-Match", fallbackTag); rec.Code != 304 {
		t.Errorf("fallback revalidation: status = %d, want 304", rec.Code)
	}
}

func TestRangeRequest(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	rec := do(site, "GET", "/config.json", "Range", "bytes=1-12")
	assertResponse(t, rec, 206, `"homeserver"`)
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestGzipNegotiation(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	for _, tc := range []struct {
		acceptEncoding string
		gzip           bool
	}{
		{"", false},
		{"gzip", true},
		{"GZip", true},
		{"deflate, gzip;q=0.5, br", true},
		{"gzip;q=0", false},
		{"gzip; q=0.000", false},
		{"gzip;q=nope", false},
		{"gzip;q=-1", false},
		{"gzip;q=1.5", false},
		{"gzip;q=1.000", true},
		{"*", true},
		{"*;q=0", false},
		{"*;q=0, gzip", true},
		{"gzip;q=0, *", false},
		{"br, zstd", false},
		{"identity", false},
	} {
		t.Run(tc.acceptEncoding, func(t *testing.T) {
			var header []string
			if tc.acceptEncoding != "" {
				header = []string{"Accept-Encoding", tc.acceptEncoding}
			}
			rec := do(site, "GET", "/assets/app-abc123.js", header...)
			if rec.Header().Get("Vary") != "Accept-Encoding" {
				t.Errorf("Vary = %q", rec.Header().Get("Vary"))
			}
			gotGzip := rec.Header().Get("Content-Encoding") == "gzip"
			if gotGzip != tc.gzip {
				t.Fatalf("Content-Encoding = %q, want gzip=%v", rec.Header().Get("Content-Encoding"), tc.gzip)
			}
			if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
				t.Errorf("Content-Length = %q, body is %d bytes", cl, rec.Body.Len())
			}
			body := rec.Body.String()
			if gotGzip {
				body = gunzip(t, rec.Body.Bytes())
			}
			if body != bundle {
				t.Errorf("decoded body differs from the file")
			}
		})
	}
}

func TestAcceptEncodingAcrossHeaderLines(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	rec := do(site, "GET", "/assets/app-abc123.js", "Accept-Encoding", "br", "Accept-Encoding", "gzip")
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q", rec.Header().Get("Content-Encoding"))
	}
}

func TestIncompressibleFilesHaveNoVariants(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, map[string]string{
		"index.html": "<p>",                     // gzip would be larger
		"logo.png":   strings.Repeat("A", 4096), // compressible bytes, but not a compressible type
	})))
	for _, path := range []string{"/", "/logo.png"} {
		rec := do(site, "GET", path, "Accept-Encoding", "gzip")
		if rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Vary") != "" {
			t.Errorf("%s: unexpected encoding headers %v", path, rec.Header())
		}
	}
}

func TestPrecompressedSiblings(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(bundle))
	zw.Close()
	tree := map[string]string{
		"app.js":      bundle,
		"app.js.br":   "pretend-brotli",
		"app.js.zst":  "pretend-zstd-but-longer",
		"app.js.gz":   gz.String(),
		"data.tar.gz": "an archive, not an encoding",
		"plain.js":    bundle,
		"tiny.css":    "a{}",
		"tiny.css.br": "larger than the original",
	}
	root := writeTree(t, tree)

	cfg := spaConfig(root)
	cfg.Fallback = ""
	cfg.Precompressed = true
	site := load(t, cfg)
	for acceptEncoding, want := range map[string]string{
		"gzip, br, zstd": "pretend-brotli",
		"gzip, zstd":     "pretend-zstd-but-longer",
		"gzip":           gz.String(),
		"":               bundle,
	} {
		rec := do(site, "GET", "/app.js", "Accept-Encoding", acceptEncoding)
		if rec.Body.String() != want {
			t.Errorf("Accept-Encoding %q: body = %.30q, want %.30q", acceptEncoding, rec.Body.String(), want)
		}
	}
	rec := do(site, "GET", "/data.tar.gz", "Accept-Encoding", "gzip")
	if rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Content-Type") != "application/gzip" {
		t.Errorf("a .gz without a base file is served as itself, got %v", rec.Header())
	}
	assertResponse(t, do(site, "GET", "/app.js.br"), 200, "pretend-brotli")
	if rec := do(site, "GET", "/tiny.css", "Accept-Encoding", "br"); rec.Body.String() != "a{}" || rec.Header().Get("Vary") != "" {
		t.Errorf("a sibling larger than its file must be ignored, got %q %v", rec.Body.String(), rec.Header())
	}
	// Encodings come only from siblings; nothing is compressed at startup.
	if enc := do(site, "GET", "/plain.js", "Accept-Encoding", "gzip").Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("startup gzip with -precompressed: Content-Encoding = %q", enc)
	}
	if heap := site.Stats().HeapBytes; heap != 0 {
		t.Errorf("HeapBytes = %d, want 0", heap)
	}

	cfg.Precompressed = false
	site = load(t, cfg)
	if enc := do(site, "GET", "/app.js", "Accept-Encoding", "br").Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("siblings used without -precompressed: Content-Encoding = %q", enc)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	for _, method := range []string{"POST", "PUT", "DELETE", "OPTIONS"} {
		rec := do(site, method, "/index.html")
		if rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: got %d Allow %q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	rec := do(site, "HEAD", "/config.json")
	assertResponse(t, rec, 200, "")
	if rec.Header().Get("Content-Length") != "28" {
		t.Errorf("HEAD Content-Length = %q", rec.Header().Get("Content-Length"))
	}
}

func TestConfiguredHeadersOnEveryResponse(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	etag := do(site, "GET", "/config.json").Header().Get("Etag")
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"200":      do(site, "GET", "/config.json"),
		"304":      do(site, "GET", "/config.json", "If-None-Match", etag),
		"404":      do(site, "GET", "/assets/nope.js"),
		"405":      do(site, "POST", "/"),
		"redirect": do(site, "GET", "/docs"),
		"fallback": do(site, "GET", "/room"),
	} {
		if rec.Header().Get("Referrer-Policy") != "no-referrer" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: header missing, got %v", name, rec.Header())
		}
	}
}

func TestRequestPathIsOnlyALookupKey(t *testing.T) {
	outside := writeTree(t, map[string]string{"secret": "top secret"})
	tree := spaTree()
	cfg := spaConfig(writeTree(t, tree))
	cfg.Fallback = ""
	site := load(t, cfg)
	rel, _ := filepath.Rel(cfg.Root, filepath.Join(outside, "secret"))
	for _, path := range []string{
		"/" + filepath.ToSlash(rel),
		"/../../../../etc/passwd",
		"/assets/../config.json",
		"//config.json",
		"/./config.json",
		"/config.json%00",
		"/config.json\x00",
		"/CONFIG.JSON",
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.URL.Path = path
		rec := httptest.NewRecorder()
		site.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Errorf("%q: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestSymlinks(t *testing.T) {
	outside := writeTree(t, map[string]string{"secret": "top secret"})

	root := writeTree(t, map[string]string{"real.txt": "real"})
	if err := os.Symlink("real.txt", filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	site := load(t, Config{Root: root, CacheControl: "no-cache"})
	assertResponse(t, do(site, "GET", "/alias.txt"), 200, "real")

	escaping := writeTree(t, map[string]string{"index.html": "x"})
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(escaping, "leak"))
	if _, err := Load(Config{Root: escaping}); err == nil {
		t.Error("symlink leaving the root must fail the load")
	}

	dirLink := writeTree(t, map[string]string{"sub/a.txt": "a"})
	os.Symlink("sub", filepath.Join(dirLink, "linked"))
	if _, err := Load(Config{Root: dirLink}); err == nil {
		t.Error("symlinked directory must fail the load")
	}
}

// Mirrors how kubelet lays out a ConfigMap volume mounted into the root.
func TestKubernetesConfigMapVolume(t *testing.T) {
	root := writeTree(t, map[string]string{
		"index.html": index,
		"conf/..2026_11_20_00_00_00.1/config.json": `{"a":1}`,
	})
	if err := os.Symlink("..2026_11_20_00_00_00.1", filepath.Join(root, "conf", "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/config.json", filepath.Join(root, "conf", "config.json")); err != nil {
		t.Fatal(err)
	}
	site := load(t, Config{Root: root, CacheControl: "no-cache"})
	assertResponse(t, do(site, "GET", "/conf/config.json"), 200, `{"a":1}`)
	if rec := do(site, "GET", "/conf/..data/config.json"); rec.Code != 404 {
		t.Errorf("volume internals served: status %d", rec.Code)
	}
}

func TestLoadRejectsMissingDocuments(t *testing.T) {
	root := writeTree(t, map[string]string{"app.html": "x"})
	if _, err := Load(Config{Root: root, Fallback: "/index.html"}); err == nil {
		t.Error("missing -fallback document must fail the load")
	}
	if _, err := Load(Config{Root: root, NotFound: "/404.html"}); err == nil {
		t.Error("missing -not-found document must fail the load")
	}
}

func TestETagsDependOnContentOnly(t *testing.T) {
	tagOf := func(content string) string {
		site := load(t, Config{Root: writeTree(t, map[string]string{"a.js": content}), CacheControl: "no-cache"})
		return do(site, "GET", "/a.js", "Accept-Encoding", "gzip").Header().Get("Etag")
	}
	if tagOf(bundle) != tagOf(bundle) {
		t.Error("same content in different directories must give the same ETag")
	}
	if tagOf(bundle) == tagOf(bundle+"//") {
		t.Error("different content must give different ETags")
	}
}

func TestContentTypes(t *testing.T) {
	root := writeTree(t, map[string]string{
		"LOGO.PNG":      "png",
		"noext":         "x",
		"app.foo":       "x",
		"m.webmanifest": "{}",
		"crypto.wasm":   "\x00asm",
	})
	site := load(t, Config{Root: root, CacheControl: "no-cache", Types: map[string]string{".foo": "text/plain; charset=utf-8"}})
	for path, want := range map[string]string{
		"/LOGO.PNG":      "image/png",
		"/noext":         "application/octet-stream",
		"/app.foo":       "text/plain; charset=utf-8",
		"/m.webmanifest": "application/manifest+json",
		"/crypto.wasm":   "application/wasm",
	} {
		if got := do(site, "GET", path).Header().Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type = %q, want %q", path, got, want)
		}
	}
}

// Files are opened at startup; replacing or deleting them afterwards, as a ConfigMap
// update or a rebuild does, changes nothing until restart.
func TestSnapshotSurvivesReplacementAndDeletion(t *testing.T) {
	tree := spaTree()
	cfg := spaConfig(writeTree(t, tree))
	site := load(t, cfg)
	before := do(site, "GET", "/config.json")

	replacement := filepath.Join(cfg.Root, "config.json.new")
	if err := os.WriteFile(replacement, []byte(`{"homeserver":"changed.example"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, filepath.Join(cfg.Root, "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cfg.Root, "assets", "app-abc123.js")); err != nil {
		t.Fatal(err)
	}

	after := do(site, "GET", "/config.json")
	assertResponse(t, after, 200, tree["config.json"])
	if after.Header().Get("Etag") != before.Header().Get("Etag") {
		t.Error("ETag changed without a restart")
	}
	assertResponse(t, do(site, "GET", "/assets/app-abc123.js"), 200, bundle)
	assertResponse(t, do(site, "GET", "/assets/app-abc123.js", "Range", "bytes=0-6"), 206, bundle[:7])
}

func TestStartupGzipIsTheOnlyHeapContent(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	gz := do(site, "GET", "/assets/app-abc123.js", "Accept-Encoding", "gzip")
	st := site.Stats()
	if st.Files != len(spaTree()) {
		t.Errorf("Files = %d, want %d", st.Files, len(spaTree()))
	}
	if st.HeapBytes < int64(gz.Body.Len()) || st.HeapBytes >= st.Bytes {
		t.Errorf("HeapBytes = %d: want the gzip variants only (bundle alone is %d of %d bytes)", st.HeapBytes, gz.Body.Len(), st.Bytes)
	}
}

// Opening a FIFO blocks until a writer appears, so it must be rejected before opening.
func TestLoadRejectsSpecialFiles(t *testing.T) {
	root := writeTree(t, map[string]string{"index.html": "x"})
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Load(Config{Root: root, CacheControl: "no-cache"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO under the root must fail the load")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load blocked on a FIFO")
	}
}

// A declared Content-Length must match the body sent, for every status and representation.
func TestContentLengthMatchesBody(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	etag := do(site, "GET", "/assets/app-abc123.js", "Accept-Encoding", "gzip").Header().Get("Etag")
	for name, header := range map[string][]string{
		"200":       nil,
		"206":       {"Range", "bytes=0-9"},
		"304":       {"If-None-Match", etag},
		"412":       {"If-Match", `"other"`},
		"416":       {"Range", "bytes=999999-"},
		"multipart": {"Range", "bytes=0-1,5-6"},
	} {
		for _, encoding := range []string{"identity", "gzip"} {
			rec := do(site, "GET", "/assets/app-abc123.js", append([]string{"Accept-Encoding", encoding}, header...)...)
			if cl := rec.Header().Get("Content-Length"); cl != "" && cl != strconv.Itoa(rec.Body.Len()) {
				t.Errorf("%s %s: status %d declares Content-Length %s, body is %d bytes", name, encoding, rec.Code, cl, rec.Body.Len())
			}
		}
	}
	rec := do(site, "HEAD", "/assets/app-abc123.js", "Accept-Encoding", "gzip")
	full := do(site, "GET", "/assets/app-abc123.js", "Accept-Encoding", "gzip")
	if rec.Header().Get("Content-Length") != strconv.Itoa(full.Body.Len()) {
		t.Errorf("HEAD Content-Length = %q, GET body is %d bytes", rec.Header().Get("Content-Length"), full.Body.Len())
	}
}

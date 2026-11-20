package main

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
)

const (
	immutableCacheControl = "public, max-age=31536000, immutable"
	identity              = "identity"
)

// Site is a snapshot of the root taken at startup. Every file is opened once and its
// descriptor kept, so renames and deletions under the root do not change what is served.
// File contents are cached by the kernel's page cache, which the container's memory limit
// bounds and reclaims; only gzip variants made at startup live on the Go heap.
type Site struct {
	// Keyed by URL path; "/dir/" is an alias of "/dir/index.html".
	files map[string]*entry
	// nil: no such document configured.
	fallback, notFound *entry
	immutable          []string
	cacheControl       string
	headers            http.Header
	open               []*os.File
	stats              Stats
}

// Stats describes the loaded snapshot.
type Stats struct {
	Files int
	// Size of the files on disk.
	Bytes int64
	// Size of the gzip variants held on the Go heap.
	HeapBytes int64
}

type entry struct {
	contentType  string
	cacheControl string
	identity     representation
	// Content-coded variants smaller than identity, smallest first.
	encoded []representation
}

type representation struct {
	coding  string      // "identity", "gzip", "br" or "zstd"
	content io.ReaderAt // a file held open since startup, or a gzip variant in memory
	size    int64
	etag    string
}

func (r representation) reader() *io.SectionReader {
	return io.NewSectionReader(r.content, 0, r.size)
}

func etagOf(sum []byte) string {
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

func fileRepresentation(f *os.File, size int64) (representation, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, size)); err != nil {
		return representation{}, err
	}
	return representation{coding: identity, content: f, size: size, etag: etagOf(h.Sum(nil))}, nil
}

func memoryRepresentation(coding string, body []byte) representation {
	sum := sha256.Sum256(body)
	return representation{coding: coding, content: bytes.NewReader(body), size: int64(len(body)), etag: etagOf(sum[:])}
}

// Sibling suffixes recognized with -precompressed, mapped to their content coding.
var precompressedSuffixes = map[string]string{".br": "br", ".zst": "zstd", ".gz": "gzip"}

// Load opens every file under cfg.Root. Entries named "..*" are skipped: they are
// Kubernetes' atomic-writer internals in ConfigMap and Secret volumes.
func Load(cfg Config) (*Site, error) {
	root, err := os.OpenRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	site := &Site{
		files:        map[string]*entry{},
		immutable:    cfg.Immutable,
		cacheControl: cfg.CacheControl,
		headers:      cfg.Headers,
	}
	if err := site.load(root, cfg); err != nil {
		site.Close()
		return nil, err
	}
	return site, nil
}

func (s *Site) load(root *os.Root, cfg Config) error {
	// Symlinks are followed but may not leave the root.
	fsys := root.FS()
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), "..") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Checked before opening: opening a FIFO would block.
		info, err := fs.Stat(fsys, name)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return fmt.Errorf("%s: symlinked directories are not supported", name)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: not a regular file", name)
		}
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		s.open = append(s.open, f)
		if info, err = f.Stat(); err != nil {
			return err
		}
		rep, err := fileRepresentation(f, info.Size())
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		urlPath := "/" + name
		s.files[urlPath] = &entry{
			contentType:  contentType(name, cfg.Types),
			cacheControl: s.cacheControlFor(urlPath),
			identity:     rep,
		}
		s.stats.Files++
		s.stats.Bytes += rep.size
		return nil
	})
	if err != nil {
		return err
	}

	if cfg.Precompressed {
		s.attachSiblings()
	} else if err := s.gzipCompressible(); err != nil {
		return err
	}
	for _, e := range s.files {
		slices.SortFunc(e.encoded, func(a, b representation) int {
			return cmp.Or(cmp.Compare(a.size, b.size), cmp.Compare(a.coding, b.coding))
		})
	}
	for _, urlPath := range slices.Collect(maps.Keys(s.files)) {
		if dir, ok := strings.CutSuffix(urlPath, "/index.html"); ok {
			s.files[dir+"/"] = s.files[urlPath]
		}
	}

	if s.fallback, err = s.document("-fallback", cfg.Fallback); err != nil {
		return err
	}
	s.notFound, err = s.document("-not-found", cfg.NotFound)
	return err
}

// attachSiblings makes X.br, X.zst and X.gz the encodings of X; they stay files themselves too.
func (s *Site) attachSiblings() {
	for urlPath, sibling := range s.files {
		for suffix, coding := range precompressedSuffixes {
			base, ok := strings.CutSuffix(urlPath, suffix)
			if e, exists := s.files[base]; ok && exists {
				rep := sibling.identity
				rep.coding = coding
				e.addEncoding(rep)
			}
		}
	}
}

func (s *Site) gzipCompressible() error {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	for _, e := range s.files {
		if !compressible(e.contentType) {
			continue
		}
		buf.Reset()
		zw.Reset(&buf)
		if _, err := io.Copy(zw, e.identity.reader()); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		if e.addEncoding(memoryRepresentation("gzip", bytes.Clone(buf.Bytes()))) {
			s.stats.HeapBytes += int64(buf.Len())
		}
	}
	return nil
}

// addEncoding keeps rep only if it is smaller than identity, and reports whether it did.
func (e *entry) addEncoding(rep representation) bool {
	if rep.size >= e.identity.size {
		return false
	}
	e.encoded = append(e.encoded, rep)
	return true
}

func (s *Site) document(flagName, urlPath string) (*entry, error) {
	if urlPath == "" {
		return nil, nil
	}
	e, ok := s.files[urlPath]
	if !ok {
		return nil, fmt.Errorf("%s %s: no such file under the root", flagName, urlPath)
	}
	return e, nil
}

func (s *Site) isImmutable(urlPath string) bool {
	return slices.ContainsFunc(s.immutable, func(prefix string) bool { return strings.HasPrefix(urlPath, prefix) })
}

func (s *Site) cacheControlFor(urlPath string) string {
	if s.isImmutable(urlPath) {
		return immutableCacheControl
	}
	return s.cacheControl
}

func (s *Site) Stats() Stats {
	return s.stats
}

// Close releases the file descriptors held since Load.
func (s *Site) Close() error {
	var errs []error
	for _, f := range s.open {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
}

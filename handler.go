package main

import (
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ServeHTTP answers from the in-memory snapshot; the request path is only ever a map key.
func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	for name, values := range s.headers {
		h[name] = slices.Clone(values)
	}
	h.Set("Cache-Control", s.cacheControl)
	// Every response carries an explicit Content-Type; clients must not guess another.
	h.Set("X-Content-Type-Options", "nosniff")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	p := r.URL.Path
	if e, ok := s.files[p]; ok {
		serveEntry(w, r, e)
		return
	}
	if _, ok := s.files[p+"/"]; ok && !strings.HasSuffix(p, "/") {
		target := url.URL{Path: p + "/", RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, target.String(), http.StatusMovedPermanently)
		return
	}
	// A missing content-hashed file must stay a 404: the fallback would serve HTML as script.
	if s.fallback != nil && !s.isImmutable(p) {
		serveEntry(w, r, s.fallback)
		return
	}
	if s.notFound != nil {
		h.Set("Content-Type", s.notFound.contentType)
		h.Set("Content-Length", strconv.FormatInt(s.notFound.identity.size, 10))
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			io.Copy(w, s.notFound.identity.reader())
		}
		return
	}
	http.NotFound(w, r)
}

// serveEntry sends the smallest acceptable representation; ServeContent handles
// conditional and range requests against its ETag.
func serveEntry(w http.ResponseWriter, r *http.Request, e *entry) {
	rep := e.identity
	if len(e.encoded) > 0 {
		w.Header().Add("Vary", "Accept-Encoding")
		acceptEncoding := strings.Join(r.Header.Values("Accept-Encoding"), ",")
		for _, candidate := range e.encoded {
			if acceptsCoding(acceptEncoding, candidate.coding) {
				rep = candidate
				break
			}
		}
	}
	h := w.Header()
	h.Set("Content-Type", e.contentType)
	h.Set("Cache-Control", e.cacheControl)
	h.Set("Etag", rep.etag)
	if rep.coding != identity {
		h.Set("Content-Encoding", rep.coding)
		w = fullLength{w, rep.size}
	}
	// Zero modtime: validation is by content hash only, so file timestamps never matter.
	http.ServeContent(w, r, "", time.Time{}, rep.reader())
}

// fullLength adds Content-Length to a 200 response. ServeContent omits it when
// Content-Encoding is set; every other status either sets its own or has no body.
type fullLength struct {
	http.ResponseWriter
	size int64
}

func (w fullLength) WriteHeader(code int) {
	if code == http.StatusOK {
		w.Header().Set("Content-Length", strconv.FormatInt(w.size, 10))
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w fullLength) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// acceptsCoding reports whether an Accept-Encoding value (RFC 9110 §12.5.3) allows coding.
// A coding listed explicitly wins over "*"; q=0 or a malformed q means "not acceptable".
func acceptsCoding(acceptEncoding, coding string) bool {
	wildcard := false
	for element := range strings.SplitSeq(acceptEncoding, ",") {
		name, params, _ := strings.Cut(element, ";")
		name = strings.TrimSpace(name)
		switch {
		case strings.EqualFold(name, coding):
			return qvalue(params) > 0
		case name == "*":
			wildcard = qvalue(params) > 0
		}
	}
	return wildcard
}

func qvalue(params string) float64 {
	for param := range strings.SplitSeq(params, ";") {
		key, value, _ := strings.Cut(strings.TrimSpace(param), "=")
		if strings.EqualFold(strings.TrimSpace(key), "q") {
			q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || q < 0 || q > 1 {
				return 0
			}
			return q
		}
	}
	return 1
}

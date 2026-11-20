package main

import (
	"flag"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseConfig(t *testing.T) {
	cfg, unknown, err := parseConfig([]string{
		"-root", "dist",
		"-fallback", "/index.html",
		"-immutable", "/assets/",
		"-immutable", "/fonts/",
		"-header", "X-Frame-Options: DENY",
		"-header", "content-security-policy:  frame-ancestors 'none'  ",
		"-type", ".WEBC=application/x-thing",
		"-precompressed",
	}, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Listen:        ":8080",
		Root:          "dist",
		Fallback:      "/index.html",
		Immutable:     []string{"/assets/", "/fonts/"},
		CacheControl:  "no-cache",
		Headers:       http.Header{"X-Frame-Options": {"DENY"}, "Content-Security-Policy": {"frame-ancestors 'none'"}},
		Types:         map[string]string{".webc": "application/x-thing"},
		Precompressed: true,
		ReadTimeout:   10 * time.Second,
		IdleTimeout:   2 * time.Hour,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got  %+v\nwant %+v", cfg, want)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %q", unknown)
	}
}

// The image sets defaults with ENV; a Deployment overrides single keys with env or args.
func TestEnvironmentLayering(t *testing.T) {
	image := []string{
		"PATH=/usr/bin",
		"JUSTSERVE_FALLBACK=/index.html",
		"JUSTSERVE_CACHE_CONTROL=no-cache, private",
		"JUSTSERVE_PRECOMPRESSED=true",
		"JUSTSERVE_IMMUTABLE_ASSETS=/assets/",
		"JUSTSERVE_IMMUTABLE_FONTS=/fonts/",
		"JUSTSERVE_HEADER_REFERRER=Referrer-Policy: no-referrer",
		"JUSTSERVE_READ_TIMEOUT=5s",
		"JUSTSERVE_HEADER_CSP=Content-Security-Policy: frame-ancestors 'none'",
		"JUSTSERVE_HEADER_FRAME=X-Frame-Options: DENY",
		"JUSTSERVE_TYPE_WEBC=.webc=application/x-thing",
		// Order matters for repeated header values; labels sort as a < b < … < f.
		"JUSTSERVE_HEADER_LINK_D=Link: <d>",
		"JUSTSERVE_HEADER_LINK_B=Link: <b>",
		"JUSTSERVE_HEADER_LINK_F=Link: <f>",
		"JUSTSERVE_HEADER_LINK_A=Link: <a>",
		"JUSTSERVE_HEADER_LINK_E=Link: <e>",
		"JUSTSERVE_HEADER_LINK_C=Link: <c>",
	}
	deployment := []string{
		// Later entries win, as when a container's env overrides the image's ENV.
		"JUSTSERVE_HEADER_CSP=Content-Security-Policy: frame-ancestors 'self'",
		"JUSTSERVE_HEADER_FRAME=",
		"JUSTSERVE_LISTEN=",
		"JUSTSERVE_SERVICE_HOST=10.0.0.1",
		"JUSTSERVE_HEADERS_TYPO=X-Typo: 1",
		"JUSTSERVE_HEADER=X-No-Label: 1",
	}
	cfg, unknown, err := parseConfig(
		[]string{"-fallback", "/app.html", "-immutable", "/static/"},
		append(image, deployment...), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Listen:       ":8080",
		Root:         "/srv",
		Fallback:     "/app.html",
		Immutable:    []string{"/assets/", "/fonts/", "/static/"},
		CacheControl: "no-cache, private",
		Headers: http.Header{
			"Referrer-Policy":         {"no-referrer"},
			"Content-Security-Policy": {"frame-ancestors 'self'"},
			"Link":                    {"<a>", "<b>", "<c>", "<d>", "<e>", "<f>"},
		},
		Types:         map[string]string{".webc": "application/x-thing"},
		Precompressed: true,
		ReadTimeout:   5 * time.Second,
		IdleTimeout:   2 * time.Hour,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got  %+v\nwant %+v", cfg, want)
	}
	wantUnknown := []string{"JUSTSERVE_HEADER", "JUSTSERVE_HEADERS_TYPO", "JUSTSERVE_SERVICE_HOST"}
	if !reflect.DeepEqual(unknown, wantUnknown) {
		t.Errorf("unknown = %q, want %q", unknown, wantUnknown)
	}
}

func TestEnvironmentErrorsNameTheVariable(t *testing.T) {
	for _, kv := range []string{
		"JUSTSERVE_HEADER_BAD=no colon here",
		"JUSTSERVE_PRECOMPRESSED=yes please",
		"JUSTSERVE_IMMUTABLE_X=assets",
	} {
		_, _, err := parseConfig(nil, []string{kv}, io.Discard)
		name, _, _ := strings.Cut(kv, "=")
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v, want an error naming the variable", kv, err)
		}
	}
}

// Each variable must belong to exactly one flag: no flag's name may be another's labelled form.
func TestEnvironmentNamesAreUnambiguous(t *testing.T) {
	fs := newFlagSet(&Config{}, io.Discard)
	fs.VisitAll(func(a *flag.Flag) {
		fs.VisitAll(func(b *flag.Flag) {
			if a != b && strings.HasPrefix(envName(a.Name), envName(b.Name)+"_") {
				t.Errorf("%s would be read as a value of -%s", envName(a.Name), b.Name)
			}
		})
	})
}

func TestParseConfigRejects(t *testing.T) {
	for name, args := range map[string][]string{
		"header without colon":     {"-header", "X-Thing"},
		"header name with space":   {"-header", "X Thing: 1"},
		"header value with CRLF":   {"-header", "X-Thing: a\r\nSet-Cookie: b"},
		"server-owned header":      {"-header", "etag: x"},
		"nosniff is always sent":   {"-header", "X-Content-Type-Options: nosniff"},
		"zero read timeout":        {"-read-timeout", "0"},
		"listen without port":      {"-listen", "garbage"},
		"fallback under immutable": {"-fallback", "/assets/index.html", "-immutable", "/assets/"},
		"negative idle timeout":    {"-idle-timeout", "-1s"},
		"cache-control via header": {"-header", "Cache-Control: max-age=1"},
		"cache-control with LF":    {"-cache-control", "no-cache\nX: y"},
		"empty cache-control":      {"-cache-control", ""},
		"empty listen":             {"-listen", ""},
		"immutable without slash":  {"-immutable", "/assets"},
		"relative immutable":       {"-immutable", "assets/"},
		"relative fallback":        {"-fallback", "index.html"},
		"relative not-found":       {"-not-found", "404.html"},
		"type without dot":         {"-type", "js=text/javascript"},
		"type double extension":    {"-type", ".tar.gz=application/gzip"},
		"type invalid media type":  {"-type", ".x=not a type"},
		"positional argument":      {"dist"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseConfig(args, nil, io.Discard); err == nil {
				t.Errorf("%q accepted", args)
			}
		})
	}
}

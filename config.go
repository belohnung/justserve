package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Config is the complete server configuration, from flags and JUSTSERVE_* variables.
type Config struct {
	Listen string
	Root   string
	// URL path of the document served with 200 for missing paths (SPA mode); "" disables it.
	Fallback string
	// URL path of the document served with 404 for missing paths; "" sends a plain 404.
	NotFound string
	// URL path prefixes, each ending in "/", that hold content-hashed files.
	Immutable []string
	// Cache-Control for every response that is not an immutable file.
	CacheControl string
	// Sent with every response.
	Headers http.Header
	// Extension ("." + lowercase) to Content-Type; overrides and extends the built-in table.
	Types map[string]string
	// Take encodings only from X.br, X.zst and X.gz instead of gzipping at startup.
	Precompressed bool
	// A connection that has not sent its complete request, headers and body, within this time is closed.
	ReadTimeout time.Duration
	// A keep-alive connection idle for this long is closed.
	IdleTimeout time.Duration
}

const envPrefix = "JUSTSERVE_"

const usageHeader = `Usage: justserve [flags]   (version %s)

Every flag can also be set in the environment: -cache-control as JUSTSERVE_CACHE_CONTROL,
and each value of a repeatable flag as JUSTSERVE_<FLAG>_<label>, e.g. JUSTSERVE_HEADER_CSP.
The environment applies first: single-valued flags on the command line replace it, and
repeatable ones add to it. An empty variable counts as unset.

`

// repeated is a flag that may be given several times; its environment variables carry a label.
type repeated func(string) error

func (r repeated) Set(v string) error { return r(v) }
func (r repeated) String() string     { return "" }

// Headers the server sets itself; -header must not fight over them.
var ownedHeaders = []string{
	"Accept-Ranges", "Allow", "Cache-Control", "Content-Encoding", "Content-Length",
	"Content-Range", "Content-Type", "Date", "Etag", "Last-Modified", "Location", "Vary",
	"X-Content-Type-Options",
}

// parseConfig reads environ (KEY=value pairs), then args. It also returns the JUSTSERVE_*
// variables that match no flag; they are not an error, since Kubernetes injects
// JUSTSERVE_SERVICE_HOST and similar for a Service named "justserve".
func parseConfig(args, environ []string, output io.Writer) (Config, []string, error) {
	cfg := Config{Headers: http.Header{}, Types: map[string]string{}}
	fs := newFlagSet(&cfg, output)
	unknown, err := applyEnv(fs, environ)
	if err != nil {
		return Config{}, nil, err
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, nil, err
	}
	if fs.NArg() > 0 {
		return Config{}, nil, fmt.Errorf("unexpected arguments: %q", fs.Args())
	}
	if cfg.Listen == "" || cfg.Root == "" || cfg.CacheControl == "" {
		return Config{}, nil, errors.New("-listen, -root and -cache-control must not be empty")
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return Config{}, nil, fmt.Errorf("-listen %q: %w", cfg.Listen, err)
	}
	for _, prefix := range cfg.Immutable {
		if cfg.Fallback != "" && strings.HasPrefix(cfg.Fallback, prefix) {
			// Every client route would be cached for a year.
			return Config{}, nil, fmt.Errorf("-fallback %s is under -immutable %s", cfg.Fallback, prefix)
		}
	}
	if cfg.ReadTimeout <= 0 || cfg.IdleTimeout <= 0 {
		return Config{}, nil, errors.New("-read-timeout and -idle-timeout must be positive")
	}
	for _, p := range []string{cfg.Fallback, cfg.NotFound} {
		if p != "" && !strings.HasPrefix(p, "/") {
			return Config{}, nil, fmt.Errorf("%q: document paths are URL paths and start with /", p)
		}
	}
	if !validFieldValue(cfg.CacheControl) {
		return Config{}, nil, fmt.Errorf("-cache-control %q: invalid header value", cfg.CacheControl)
	}
	return cfg, unknown, nil
}

func newFlagSet(cfg *Config, output io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("justserve", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), usageHeader, version)
		fs.PrintDefaults()
	}
	fs.StringVar(&cfg.Listen, "listen", ":8080", "`address` to listen on")
	fs.StringVar(&cfg.Root, "root", "/srv", "`directory` to serve; snapshotted at startup")
	fs.StringVar(&cfg.Fallback, "fallback", "", "`path` served with 200 for missing paths, e.g. /index.html for an SPA")
	fs.StringVar(&cfg.NotFound, "not-found", "", "`path` served with 404 for missing paths, e.g. /404.html")
	fs.Var(repeated(func(v string) error {
		if !strings.HasPrefix(v, "/") || !strings.HasSuffix(v, "/") {
			return fmt.Errorf("%q must start and end with /", v)
		}
		cfg.Immutable = append(cfg.Immutable, v)
		return nil
	}), "immutable", "URL `prefix` of content-hashed files, e.g. /assets/; long-cached, never falls back (repeatable)")
	fs.StringVar(&cfg.CacheControl, "cache-control", "no-cache", "Cache-Control `value` for everything except immutable files")
	fs.Var(repeated(func(v string) error {
		name, value, err := parseHeader(v)
		if err != nil {
			return err
		}
		cfg.Headers.Add(name, value)
		return nil
	}), "header", "`Name: value` sent with every response (repeatable)")
	fs.Var(repeated(func(v string) error {
		ext, ctype, ok := strings.Cut(v, "=")
		if !ok || !strings.HasPrefix(ext, ".") || len(ext) < 2 || strings.Contains(ext[1:], ".") {
			return fmt.Errorf("%q: want .ext=media/type", v)
		}
		if _, _, err := mime.ParseMediaType(ctype); err != nil {
			return fmt.Errorf("%q: %w", v, err)
		}
		cfg.Types[strings.ToLower(ext)] = ctype
		return nil
	}), "type", "`.ext=media/type` Content-Type for an extension (repeatable)")
	fs.BoolVar(&cfg.Precompressed, "precompressed", false, "take encodings only from X.br, X.zst and X.gz siblings")
	fs.DurationVar(&cfg.ReadTimeout, "read-timeout", 10*time.Second, "`time` a client has to send its complete request, headers and body")
	// Longer than Envoy's default upstream idle timeout (1h), so the gateway closes first and never reuses a closing connection.
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", 2*time.Hour, "`time` an idle keep-alive connection is kept open; keep it above the gateway's upstream idle timeout")
	return fs
}

// envName maps a flag name to its variable: cache-control to JUSTSERVE_CACHE_CONTROL.
func envName(flagName string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// applyEnv sets flags from JUSTSERVE_* variables and returns the ones that match no flag.
// Labelled values of a repeatable flag apply in label order.
func applyEnv(fs *flag.FlagSet, environ []string) ([]string, error) {
	vars := map[string]string{}
	for _, kv := range environ {
		key, value, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, envPrefix) {
			vars[key] = value
		}
	}
	maps.DeleteFunc(vars, func(_, value string) bool { return value == "" })
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		name := envName(f.Name)
		_, isRepeated := f.Value.(repeated)
		for _, key := range slices.Sorted(maps.Keys(vars)) {
			label, labelled := strings.CutPrefix(key, name+"_")
			if isRepeated && labelled && label != "" || !isRepeated && key == name {
				if err := fs.Set(f.Name, vars[key]); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", key, err))
				}
				delete(vars, key)
			}
		}
	})
	return slices.Sorted(maps.Keys(vars)), errors.Join(errs...)
}

func parseHeader(v string) (name, value string, err error) {
	name, value, ok := strings.Cut(v, ":")
	if !ok {
		return "", "", fmt.Errorf("%q: want Name: value", v)
	}
	name, value = http.CanonicalHeaderKey(strings.TrimSpace(name)), strings.TrimSpace(value)
	if !isToken(name) {
		return "", "", fmt.Errorf("%q: invalid header name", v)
	}
	if !validFieldValue(value) {
		return "", "", fmt.Errorf("%q: invalid header value", v)
	}
	for _, owned := range ownedHeaders {
		if name == owned {
			return "", "", fmt.Errorf("%q: %s is set by the server", v, name)
		}
	}
	return name, value, nil
}

// isToken reports whether s is an RFC 9110 token.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		alnum := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
		if !alnum && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

// validFieldValue rejects control characters other than tab; they would split or corrupt the response.
func validFieldValue(s string) bool {
	for _, c := range []byte(s) {
		if c < ' ' && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

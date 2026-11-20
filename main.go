// Command justserve serves a directory of static files, typically a single-page app,
// from a snapshot of open files taken at startup. It speaks plain HTTP and expects TLS to end at the gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

// version identifies the build; release builds set it with -ldflags "-X main.version=…".
var version = "devel"

func main() {
	// logfmt on stderr, for kubectl logs and log collectors alike.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, unknown, err := parseConfig(os.Args[1:], os.Environ(), os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		slog.Error("invalid configuration", "version", version, "err", err)
		os.Exit(2)
	}
	if len(unknown) > 0 {
		slog.Warn("ignoring environment variables that match no flag", "names", unknown)
	}
	slog.Info("config", "version", version, "listen", cfg.Listen, "root", cfg.Root, "fallback", cfg.Fallback,
		"notFound", cfg.NotFound, "immutable", cfg.Immutable, "cacheControl", cfg.CacheControl,
		"headers", cfg.Headers, "types", cfg.Types, "precompressed", cfg.Precompressed,
		"readTimeout", cfg.ReadTimeout, "idleTimeout", cfg.IdleTimeout)

	// SIGTERM is how Kubernetes stops a pod; as PID 1 it is ignored unless handled.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		slog.Error("exiting", "err", err)
		os.Exit(1)
	}
}

// run loads the snapshot completely before it opens the listener, so nothing is served
// from a partial or rejected root.
func run(ctx context.Context, cfg Config) error {
	site, err := Load(cfg)
	if err != nil {
		return err
	}
	defer site.Close()
	// Compressing at startup leaves garbage; release it before serving.
	debug.FreeOSMemory()
	stats := site.Stats()
	slog.Info("loaded", "root", cfg.Root, "files", stats.Files, "bytes", stats.Bytes, "heapBytes", stats.HeapBytes)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	slog.Info("listening", "addr", ln.Addr().String())
	return serve(ctx, newServer(cfg, site), ln)
}

func newServer(cfg Config, handler http.Handler) *http.Server {
	return &http.Server{
		Handler: handler,
		// Bounds headers and body; Go applies it to headers when ReadHeaderTimeout is unset.
		ReadTimeout: cfg.ReadTimeout,
		IdleTimeout: cfg.IdleTimeout,
		// Otherwise net/http answers "OPTIONS *" itself, bypassing the handler.
		DisableGeneralOptionsHandler: true,
	}
}

// serve runs srv on ln until ctx ends, then stops accepting and drains in-flight requests;
// the pod's terminationGracePeriodSeconds bounds the drain.
func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	return srv.Shutdown(context.Background())
}

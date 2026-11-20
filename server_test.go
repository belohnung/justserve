package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// startServer serves handler with cfg's timeouts on a loopback port until the test ends.
func startServer(t *testing.T, cfg Config, handler http.Handler) (addr string, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc, finished := make(chan error, 1), make(chan struct{})
	go func() {
		errc <- serve(ctx, newServer(cfg, handler), ln)
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		<-finished
	})
	return ln.Addr().String(), cancel, errc
}

// closedWithin reports whether the server closes conn within d.
func closedWithin(t *testing.T, conn net.Conn, d time.Duration) bool {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(d))
	_, err := io.ReadAll(conn)
	var netErr net.Error
	return !(errors.As(err, &netErr) && netErr.Timeout())
}

func TestIncompleteRequestsAreDropped(t *testing.T) {
	cfg := Config{ReadTimeout: 200 * time.Millisecond, IdleTimeout: time.Hour}
	addr, _, _ := startServer(t, cfg, http.NotFoundHandler())
	for name, request := range map[string]string{
		"headers never finished":   "GET / HTTP/1.1\r\nHost: x\r\n",
		"declared body never sent": "GET / HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.Write([]byte(request))
			if !closedWithin(t, conn, 2*time.Second) {
				t.Error("connection stayed open")
			}
		})
	}
}

func TestIdleConnectionsAreClosed(t *testing.T) {
	cfg := Config{ReadTimeout: time.Hour, IdleTimeout: 200 * time.Millisecond}
	addr, _, _ := startServer(t, cfg, http.NotFoundHandler())
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	if !closedWithin(t, conn, 2*time.Second) {
		t.Error("an idle keep-alive connection stayed open")
	}
}

func TestShutdownDrainsInFlightRequests(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, "finished")
	})
	addr, cancel, done := startServer(t, Config{ReadTimeout: time.Second, IdleTimeout: time.Second}, slow)

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			body <- "error: " + err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	<-entered
	cancel()

	select {
	case err := <-done:
		t.Fatalf("serve returned %v before the in-flight request finished", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := net.Dial("tcp", addr); err == nil {
		t.Error("still accepting connections after shutdown began")
	}
	close(release)
	if got := <-body; got != "finished" {
		t.Errorf("in-flight request got %q", got)
	}
	if err := <-done; err != nil {
		t.Errorf("serve = %v, want nil after a clean shutdown", err)
	}
}

func TestRunRefusesToListenOnInvalidContent(t *testing.T) {
	root := writeTree(t, map[string]string{"app.html": "x"})
	cfg := Config{Root: root, Listen: "127.0.0.1:0", Fallback: "/index.html", CacheControl: "no-cache",
		ReadTimeout: time.Second, IdleTimeout: time.Second}
	err := run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "/index.html") {
		t.Errorf("run = %v, want the load error", err)
	}
}

// Every request, including "OPTIONS *", must reach the handler.
func TestNoRequestBypassesTheHandler(t *testing.T) {
	site := load(t, spaConfig(writeTree(t, spaTree())))
	addr, _, _ := startServer(t, Config{ReadTimeout: time.Second, IdleTimeout: time.Second}, site)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("OPTIONS * HTTP/1.1\r\nHost: x\r\n\r\n"))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("OPTIONS *: %d %v, want 405 from the handler", resp.StatusCode, resp.Header)
	}
}

// Percent-encoded traversal, as a client would send it on the wire.
func TestEncodedTraversalOverTheWire(t *testing.T) {
	cfg := spaConfig(writeTree(t, spaTree()))
	cfg.Fallback = ""
	site := load(t, cfg)
	addr, _, _ := startServer(t, Config{ReadTimeout: time.Second, IdleTimeout: time.Second}, site)
	for _, target := range []string{
		"/%2e%2e/%2e%2e/%2e%2e/etc/passwd",
		"/assets/..%2fconfig.json",
		"/assets/%2e%2e/config.json",
		"/config.json%00",
		"/%2Fetc%2Fpasswd",
	} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conn.Write([]byte("GET " + target + " HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"))
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 404 or 400", target, resp.StatusCode)
		}
	}
}

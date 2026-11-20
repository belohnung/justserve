package main

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain runs the real main when the test binary is re-executed by runMain.
func TestMain(m *testing.M) {
	if os.Getenv("GO_JUSTSERVE_RUN_MAIN") == "1" {
		os.Args = append([]string{"justserve"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runMain starts justserve as its own process and returns its standard error, line by line.
func runMain(t *testing.T, args ...string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "GO_JUSTSERVE_RUN_MAIN=1")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd, bufio.NewScanner(stderr)
}

// readUntil returns the log lines up to and including the first one containing msg.
func readUntil(t *testing.T, lines *bufio.Scanner, msg string) []string {
	t.Helper()
	var seen []string
	for lines.Scan() {
		seen = append(seen, lines.Text())
		if strings.Contains(lines.Text(), msg) {
			return seen
		}
	}
	t.Fatalf("no %q record; got %q", msg, seen)
	return nil
}

func exitCode(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	case <-time.After(10 * time.Second):
		t.Fatal("process did not exit")
		return -1
	}
}

func TestMainLogsServesAndStopsOnSIGTERM(t *testing.T) {
	root := writeTree(t, map[string]string{"index.html": "hello"})
	cmd, lines := runMain(t, "-root", root, "-listen", "127.0.0.1:0")

	startup := strings.Join(readUntil(t, lines, "msg=listening"), "\n")
	for _, want := range []string{"level=INFO msg=config version=devel", "msg=loaded", "files=1"} {
		if !strings.Contains(startup, want) {
			t.Errorf("startup records lack %q:\n%s", want, startup)
		}
	}
	addr := regexp.MustCompile(`msg=listening addr=(\S+)`).FindStringSubmatch(startup)[1]
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello" {
		t.Errorf("body = %q", body)
	}

	cmd.Process.Signal(syscall.SIGTERM)
	readUntil(t, lines, `msg="shutting down"`)
	if code := exitCode(t, cmd); code != 0 {
		t.Errorf("exit status %d after SIGTERM, want 0", code)
	}
}

func TestMainExitStatus(t *testing.T) {
	root := writeTree(t, map[string]string{"app.html": "x"})
	for name, tc := range map[string]struct {
		args []string
		code int
		msg  string
	}{
		"invalid configuration": {[]string{"-root", root, "-cache-control", ""}, 2, `msg="invalid configuration" version=devel`},
		"unsafe content":        {[]string{"-root", root, "-fallback", "/index.html"}, 1, "msg=exiting"},
		"bind failure":          {[]string{"-root", root, "-listen", "256.0.0.1:1"}, 1, "msg=exiting"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd, lines := runMain(t, tc.args...)
			readUntil(t, lines, tc.msg)
			if code := exitCode(t, cmd); code != tc.code {
				t.Errorf("exit status %d, want %d", code, tc.code)
			}
		})
	}
}

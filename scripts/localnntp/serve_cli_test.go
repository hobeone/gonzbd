package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for run() to write while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestCLI_Serve_RepeatableDir(t *testing.T) {
	dirA := t.TempDir()
	if err := SaveManifest(filepath.Join(dirA, "manifest.json"), createTestRelease(t, dirA)); err != nil {
		t.Fatal(err)
	}
	dirB, mB, _ := writeSecondRelease(t, "-b")
	if err := SaveManifest(filepath.Join(dirB, "manifest.json"), mB); err != nil {
		t.Fatal(err)
	}

	// An already-cancelled context makes serve start, then exit 0 immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	args := []string{"serve", "--dir", dirA, "--dir", dirB, "--listen", "127.0.0.1:0", "--control", "127.0.0.1:0", "--max-connections", "3"}
	if code := run(ctx, args, &stdout, &stderr); code != 0 {
		t.Fatalf("serve with two distinct releases = %d, stderr: %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	dup := []string{"serve", "--dir", dirA, "--dir", dirA, "--listen", "127.0.0.1:0", "--control", "127.0.0.1:0"}
	if code := run(ctx, dup, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "duplicate message-id") {
		t.Fatalf("serve with duplicate release = %d, stderr %q; want non-zero with duplicate message-id", code, stderr.String())
	}
}

var controlAddrRE = regexp.MustCompile(`Control API listening on (\S+)`)

// serveInBackground runs `serve` with args plus a one-release --dir, and returns the control
// URL once the server is up and a stop function that cancels it and returns its exit code.
func serveInBackground(t *testing.T, extra ...string) (controlURL string, stop func() int) {
	t.Helper()
	dir := t.TempDir()
	if err := SaveManifest(filepath.Join(dir, "manifest.json"), createTestRelease(t, dir)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr syncBuffer
	args := append([]string{"serve", "--dir", dir, "--listen", "127.0.0.1:0", "--control", "127.0.0.1:0"}, extra...)
	done := make(chan int, 1)
	go func() { done <- run(ctx, args, &stdout, &stderr) }()
	stop = func() int {
		cancel()
		return <-done
	}
	t.Cleanup(func() { cancel() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := controlAddrRE.FindStringSubmatch(stdout.String()); m != nil {
			return "http://" + m[1], stop
		}
		select {
		case code := <-done:
			t.Fatalf("serve exited %d before listening, stderr: %s", code, stderr.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("serve did not report its control address, stdout: %s stderr: %s", stdout.String(), stderr.String())
	return "", nil
}

func TestCLI_Serve_ConnFlagsReachServer(t *testing.T) {
	controlURL, stop := serveInBackground(t, "--max-connections", "3", "--refuse", "--reject-greeting", "--auth-fail")
	resp, err := http.Get(controlURL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var st ServerStats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if want := (ConnSettings{MaxConnections: 3, Refuse: true, RejectGreeting: true, AuthFail: true}); st.Conn != want {
		t.Fatalf("/stats conn = %+v, want %+v", st.Conn, want)
	}
	if code := stop(); code != 0 {
		t.Fatalf("serve exit = %d, want 0", code)
	}

	controlURL, stop = serveInBackground(t)
	resp2, err := http.Get(controlURL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	var def ServerStats
	if err := json.NewDecoder(resp2.Body).Decode(&def); err != nil {
		t.Fatal(err)
	}
	if def.Conn != (ConnSettings{}) {
		t.Fatalf("default /stats conn = %+v, want all zero", def.Conn)
	}
	stop()
}

func TestCLI_Serve_RejectsNegativeMaxConnections(t *testing.T) {
	dir := t.TempDir()
	if err := SaveManifest(filepath.Join(dir, "manifest.json"), createTestRelease(t, dir)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	args := []string{"serve", "--dir", dir, "--listen", "127.0.0.1:0", "--control", "127.0.0.1:0", "--max-connections", "-1"}
	if code := run(ctx, args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "max_connections") {
		t.Fatalf("serve --max-connections -1 = %d, stderr %q; want non-zero naming max_connections", code, stderr.String())
	}
}

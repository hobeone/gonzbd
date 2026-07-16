package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
)

func TestFileExists(t *testing.T) {
	t.Run("existing file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "test.txt")
		if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !fileExists(f) {
			t.Errorf("fileExists(%q) = false, want true", f)
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		if fileExists("/nonexistent/path/to/file") {
			t.Error("fileExists(nonexistent) = true, want false")
		}
	})

	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		if fileExists(dir) {
			t.Errorf("fileExists(%q) = true for directory, want false", dir)
		}
	})
}

func TestHTTPSListener(t *testing.T) {
	// Generate self-signed cert for the test.
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := app.WriteSelfSigned(certFile, keyFile); err != nil {
		t.Fatalf("WriteSelfSigned: %v", err)
	}

	// Pick a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	defer srv.Close()

	// Give the server a moment to start.
	time.Sleep(50 * time.Millisecond)

	// Make a request with TLS verification disabled (self-signed cert).
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // test only
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		select {
		case srvErr := <-errCh:
			t.Fatalf("server error: %v; client error: %v", srvErr, err)
		default:
			t.Fatalf("GET https://%s: %v", addr, err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Error("expected TLS connection, got nil resp.TLS")
	}
}

func TestHTTPSAutoGenerateCert(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "auto-cert.pem")
	keyFile := filepath.Join(dir, "auto-key.pem")

	// Files should not exist yet.
	if fileExists(certFile) {
		t.Fatal("cert file exists before generation")
	}
	if fileExists(keyFile) {
		t.Fatal("key file exists before generation")
	}

	// Simulate the auto-generation logic from main.go.
	if !fileExists(certFile) || !fileExists(keyFile) {
		if err := app.WriteSelfSigned(certFile, keyFile); err != nil {
			t.Fatalf("WriteSelfSigned: %v", err)
		}
	}

	// Now both should exist.
	if !fileExists(certFile) {
		t.Error("cert file not created")
	}
	if !fileExists(keyFile) {
		t.Error("key file not created")
	}

	// Verify the cert is loadable as a TLS keypair.
	_, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
}

// syncBuffer is a mutex-guarded io.Writer so the HTTP listener goroutine
// (if one were incorrectly started) and the test can safely read/write the
// same log buffer concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Contains(s string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Contains(b.buf.String(), s)
}

// TestStartListeners_CertFailurePreventsHTTPStart is a regression guard: a
// self-signed-cert provisioning failure must be detected before the HTTP
// listener goroutine is started, so a bind/runtime HTTP failure never runs
// unsupervised with no shutdown handle returned to the caller.
func TestStartListeners_CertFailurePreventsHTTPStart(t *testing.T) {
	t.Parallel()

	addr := "127.0.0.1:0"

	// certFile/keyFile whose parent directory can never be created: a
	// path component ("blocker") is a regular file, not a directory, so
	// os.MkdirAll inside app.WriteSelfSigned's writeFileAtomic fails with
	// ENOTDIR — a deterministic, guaranteed cert-provisioning failure.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	certFile := filepath.Join(blocker, "sub", "cert.pem")
	keyFile := filepath.Join(blocker, "sub", "key.pem")

	httpSrv := &http.Server{Addr: addr, Handler: http.NewServeMux()}
	httpsSrv := &http.Server{Addr: addr, Handler: http.NewServeMux()}
	cfg := &config.Config{General: config.GeneralConfig{
		HTTPSPort: 8443,
		HTTPSCert: certFile,
		HTTPSKey:  keyFile,
	}}

	logBuf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logBuf, nil)).With("component", "startup-test")

	errCh, gotHTTPS, err := startListeners(httpSrv, httpsSrv, cfg, log)
	if err == nil {
		t.Fatal("startListeners: expected a cert-provisioning error, got nil")
	}
	if errCh != nil {
		t.Errorf("errCh = %v, want nil on cert-provisioning failure", errCh)
	}
	if gotHTTPS != nil {
		t.Errorf("httpsSrv = %v, want nil on cert-provisioning failure", gotHTTPS)
	}

	// Negative-observation window: "http listener starting" is logged
	// unconditionally as the first statement in the HTTP goroutine, before
	// ListenAndServe is even attempted. If startListeners regresses to
	// starting HTTP before checking the cert, that goroutine's scheduler
	// slice reliably arrives well within this window; its absence is the
	// regression proof. Documented per this repo's testing conventions as
	// an intentional timing window, not a flaky sleep-based assertion of
	// a positive event.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if logBuf.Contains("http listener starting") {
			t.Fatal("HTTP listener goroutine started despite cert-provisioning failure")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if logBuf.Contains("http listener starting") {
		t.Fatal("HTTP listener goroutine started despite cert-provisioning failure")
	}
}

// TestAwaitShutdownSignal_PropagatesListenerError is a regression guard: a
// listener-goroutine failure reported on errCh must surface as
// awaitShutdownSignal's return value, not be swallowed after logging, so
// serveMode can exit non-zero on a genuine bind/runtime failure instead of
// always returning nil.
func TestAwaitShutdownSignal_PropagatesListenerError(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler).With("component", "startup-test")
	wantErr := errors.New("listen tcp :8080: address already in use")

	errCh := make(chan error, 1)
	errCh <- wantErr

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	got := awaitShutdownSignal(ctx, errCh, log)
	if !errors.Is(got, wantErr) {
		t.Errorf("awaitShutdownSignal() = %v, want %v", got, wantErr)
	}
}

// TestAwaitShutdownSignal_NilOnNormalShutdown proves the companion path: a
// normal shutdown signal (context cancellation) returns nil, not an error.
func TestAwaitShutdownSignal_NilOnNormalShutdown(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler).With("component", "startup-test")
	errCh := make(chan error)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if got := awaitShutdownSignal(ctx, errCh, log); got != nil {
		t.Errorf("awaitShutdownSignal() = %v, want nil on normal shutdown", got)
	}
}

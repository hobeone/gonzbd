package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/app"
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

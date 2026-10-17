package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

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

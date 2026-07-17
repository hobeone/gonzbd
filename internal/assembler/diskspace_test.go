package assembler

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestWriteSpeedMBPerSec_WritesAndCleansUp(t *testing.T) {
	dir := t.TempDir()

	mbPerSec, err := WriteSpeedMBPerSec(context.Background(), dir, 4*1024*1024) // 4 MiB for a fast test
	if err != nil {
		t.Fatalf("WriteSpeedMBPerSec: %v", err)
	}
	// Sane order-of-magnitude bounds on the computed throughput, not just
	// "positive": a 4 MiB write completing in well under a second is
	// physically bounded between roughly 0.1 MB/s (pathologically slow) and
	// 100,000 MB/s (100 GB/s, far beyond any real disk) — wide enough to
	// never flake on real hardware, but tight enough that swapping the
	// mb/elapsed division for a multiplication (or vice versa) produces a
	// value far outside this range.
	const minPlausibleMBPerSec = 0.1
	const maxPlausibleMBPerSec = 100_000
	if mbPerSec < minPlausibleMBPerSec || mbPerSec > maxPlausibleMBPerSec {
		t.Errorf("mbPerSec = %f, want in [%v, %v]", mbPerSec, minPlausibleMBPerSec, maxPlausibleMBPerSec)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected temp file to be cleaned up, found %d entries in %s: %v", len(entries), dir, entries)
	}
}

func TestWriteSpeedMBPerSec_NonexistentDir(t *testing.T) {
	_, err := WriteSpeedMBPerSec(context.Background(), "/nonexistent/path/that/does/not/exist", 1024)
	if err == nil {
		t.Fatal("expected error for nonexistent directory, got nil")
	}
}

func TestWriteSpeedMBPerSec_ContextCanceled(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := WriteSpeedMBPerSec(ctx, dir, 4*1024*1024)
	if err == nil {
		t.Fatal("expected error for already-cancelled context, got nil")
	}
}

func TestFreeBytes_ContextValidation(t *testing.T) {
	_, err := FreeBytes(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("expected error when context.Background() is passed, got nil")
	}
	expected := "requires a cancellable context"
	if !strings.Contains(err.Error(), expected) {
		t.Errorf("expected error to contain %q, got: %v", expected, err)
	}
}

func TestFreeBytes_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := FreeBytes(ctx, t.TempDir())
	if err == nil {
		t.Fatal("expected error for already-cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled error, got: %v", err)
	}
}

func TestFreeBytes_InFlightCheck(t *testing.T) {
	dir := t.TempDir()
	inFlightProbes.Store(dir, struct{}{})
	defer inFlightProbes.Delete(dir)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	_, err := FreeBytes(ctx, dir)
	if err == nil {
		t.Fatal("expected error for in-flight check, got nil")
	}
	expected := "check already in flight"
	if !strings.Contains(err.Error(), expected) {
		t.Errorf("expected error to contain %q, got: %v", expected, err)
	}
}

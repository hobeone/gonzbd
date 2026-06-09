package unpack

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestContextCopy_FullCopy(t *testing.T) {
	data := strings.Repeat("hello world\n", 10000)
	src := strings.NewReader(data)
	var dst bytes.Buffer

	n, err := contextCopy(context.Background(), &dst, src)
	if err != nil {
		t.Fatalf("contextCopy error: %v", err)
	}
	if int(n) != len(data) {
		t.Errorf("contextCopy wrote %d bytes, want %d", n, len(data))
	}
	if dst.String() != data {
		t.Error("contextCopy: output data mismatch")
	}
}

func TestContextCopy_CancelledContext(t *testing.T) {
	// Create a large reader that will take many Read calls.
	// 1 MiB should be enough to trigger at least one ctx check.
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i % 256)
	}
	src := bytes.NewReader(data)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := contextCopy(ctx, io.Discard, src)
	if err == nil {
		t.Fatal("contextCopy: expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("contextCopy: expected context.Canceled, got %v", err)
	}
}

func TestContextCopy_EmptyReader(t *testing.T) {
	src := strings.NewReader("")
	var dst bytes.Buffer

	n, err := contextCopy(context.Background(), &dst, src)
	if err != nil {
		t.Fatalf("contextCopy error: %v", err)
	}
	if n != 0 {
		t.Errorf("contextCopy: expected 0 bytes, got %d", n)
	}
}

type failOnZeroWrite struct {
	io.Writer
	called bool
}

func (w *failOnZeroWrite) Write(p []byte) (int, error) {
	if len(p) == 0 {
		w.called = true
	}
	return w.Writer.Write(p)
}

type zeroReader struct {
	called bool
}

func (r *zeroReader) Read(p []byte) (int, error) {
	if !r.called {
		r.called = true
		return 0, nil
	}
	return copy(p, "a"), io.EOF
}

func TestContextCopy_ZeroRead(t *testing.T) {
	src := &zeroReader{}
	var dst bytes.Buffer
	w := &failOnZeroWrite{Writer: &dst}

	n, err := contextCopy(context.Background(), w, src)
	if err != nil {
		t.Fatalf("contextCopy error: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 byte, got %d", n)
	}
	if w.called {
		t.Error("dst.Write was called with 0 bytes when nr == 0")
	}
}

func TestContextCopy_PeriodicCancellationCheck(t *testing.T) {
	data := make([]byte, 512*1024)
	src := bytes.NewReader(data)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var dst bytes.Buffer
	n, err := contextCopy(ctx, &dst, src)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// Normal behavior: it should write exactly 256 KiB (8 blocks of 32 KiB)
	// before checking the context and aborting.
	// If it checked on every iteration (e.g., checkInterval is mutated),
	// it would write only 32 KiB.
	// If it never checked, it would write all 512 KiB.
	if n != 256*1024 {
		t.Errorf("expected to write exactly 262144 bytes, wrote %d", n)
	}
}

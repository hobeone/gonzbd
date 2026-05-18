package unpack

import (
	"bytes"
	"context"
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
	if err != context.Canceled {
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

package nntp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// --- Fuzz Targets ---

// FuzzReadDotStuffedBody fuzzes the readDotStuffedBody function with
// arbitrary input. Since readDotStuffedBody is unexported, we call it
// directly from within the nntp package (same-package test).
func FuzzReadDotStuffedBody(f *testing.F) {
	// Seed: minimal valid body (empty, just the terminator).
	f.Add([]byte(".\r\n"))

	// Seed: a simple one-line body.
	f.Add([]byte("hello world\r\n.\r\n"))

	// Seed: dot-stuffed line.
	f.Add([]byte("..this line starts with a dot\r\n.\r\n"))

	// Seed: bare LF (no CR).
	f.Add([]byte("hello\n.\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		br := bufio.NewReader(bytes.NewReader(data))
		_, _ = readDotStuffedBody(br)
	})
}

// --- M1: Idle timeout reader slow read ---

func TestIdleTimeoutReader_SlowRead(t *testing.T) {
	// Create a pipe so we can control when data arrives.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	itr := &idleTimeoutReader{nc: client, timeout: 100 * time.Millisecond}

	// Don't write anything on the server side. After 100ms the read
	// should fail with a deadline exceeded error.
	buf := make([]byte, 128)
	_, err := itr.Read(buf)
	if err == nil {
		t.Fatal("expected deadline error, got nil")
	}
	// The error should indicate a timeout (deadline exceeded).
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected timeout error, got: %v", err)
	}
}

// --- M2: Server disconnect mid-body ---

func TestReadDotStuffedBody_ServerDisconnectMidBody(t *testing.T) {
	// Input starts a body but ends abruptly without the ".\r\n"
	// terminator. This simulates a server disconnect mid-transfer.
	input := "partial body data\r\nmore data"
	br := bufio.NewReader(strings.NewReader(input))

	_, err := readDotStuffedBody(br)
	if err == nil {
		t.Fatal("expected error on server disconnect mid-body")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReadDotStuffedBody_ServerDisconnectMidLine(t *testing.T) {
	// EOF arrives mid-line (no trailing newline).
	input := "start of line without newline"
	br := bufio.NewReader(strings.NewReader(input))

	_, err := readDotStuffedBody(br)
	if err == nil {
		t.Fatal("expected error on mid-line EOF")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want io.ErrUnexpectedEOF", err)
	}
}

// --- M3: Long line exceeding buffer ---

func TestReadDotStuffedBody_LongLine(t *testing.T) {
	// Create a line longer than the default 256KB bufio buffer size.
	// readDotStuffedBody should return an error about the line
	// exceeding the buffer size.
	lineLen := 300 * 1024 // 300 KB
	var buf bytes.Buffer
	for range lineLen {
		buf.WriteByte('A')
	}
	buf.WriteString("\r\n.\r\n")

	br := bufio.NewReaderSize(bytes.NewReader(buf.Bytes()), 256*1024)
	_, err := readDotStuffedBody(br)
	if err == nil {
		t.Fatal("expected error for line exceeding buffer size")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unexpected error: %v (expected 'exceeds' message)", err)
	}
}

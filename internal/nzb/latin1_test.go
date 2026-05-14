package nzb

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// T9: Direct latin1Reader unit tests, especially the pending-byte edge case
// where a high byte (0x80-0xFF) expands to 2 UTF-8 bytes but the caller's
// buffer only has room for the first byte.

func TestLatin1Reader_ASCIIOnly(t *testing.T) {
	t.Parallel()
	src := strings.NewReader("Hello, world!")
	r := &latin1Reader{src: src}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "Hello, world!" {
		t.Errorf("got %q, want %q", got, "Hello, world!")
	}
}

func TestLatin1Reader_HighBytes(t *testing.T) {
	t.Parallel()
	// 0xE9 = é in Latin-1, which encodes to UTF-8 as 0xC3 0xA9.
	// 0xF1 = ñ in Latin-1, which encodes to UTF-8 as 0xC3 0xB1.
	src := bytes.NewReader([]byte{0xE9, 0xF1})
	r := &latin1Reader{src: src}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := "éñ"
	if string(got) != want {
		t.Errorf("got %q (% x), want %q (% x)", got, got, want, []byte(want))
	}
}

func TestLatin1Reader_MixedASCIIAndHigh(t *testing.T) {
	t.Parallel()
	// "caf\xE9" = "café" in Latin-1
	src := bytes.NewReader([]byte("caf\xE9"))
	r := &latin1Reader{src: src}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := "café"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLatin1Reader_PendingByteEdgeCase(t *testing.T) {
	t.Parallel()
	// This test exercises the hasPend path. When the caller provides a
	// very small buffer, a high byte (which expands to 2 UTF-8 bytes)
	// may only have room for the first byte on one Read call. The second
	// byte must be delivered on the next Read.
	//
	// We test this by reading one byte at a time.
	src := bytes.NewReader([]byte{0xE9}) // é → 0xC3, 0xA9
	r := &latin1Reader{src: src}

	buf := make([]byte, 1)

	// First read: should get 0xC3 (first byte of UTF-8 é).
	n, err := r.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("first read: %v", err)
	}
	if n != 1 || buf[0] != 0xC3 {
		t.Errorf("first read: n=%d buf=%x, want n=1 buf=C3", n, buf[:n])
	}

	// Second read: should get 0xA9 (second byte from pending).
	n, err = r.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("second read: %v", err)
	}
	if n != 1 || buf[0] != 0xA9 {
		t.Errorf("second read: n=%d buf=%x, want n=1 buf=A9", n, buf[:n])
	}
}

func TestLatin1Reader_EmptyInput(t *testing.T) {
	t.Parallel()
	src := strings.NewReader("")
	r := &latin1Reader{src: src}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestLatin1Reader_ZeroLengthBuffer(t *testing.T) {
	t.Parallel()
	src := strings.NewReader("data")
	r := &latin1Reader{src: src}

	buf := make([]byte, 0)
	n, err := r.Read(buf)
	if n != 0 || err != nil {
		t.Errorf("Read([]byte{}) = %d, %v; want 0, nil", n, err)
	}
}

func TestLatin1Reader_AllHighBytes(t *testing.T) {
	t.Parallel()
	// Test all bytes 0x80-0xFF to verify they are correctly encoded.
	var input []byte
	for b := 0x80; b <= 0xFF; b++ {
		input = append(input, byte(b))
	}
	r := &latin1Reader{src: bytes.NewReader(input)}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// Each input byte should produce exactly 2 UTF-8 bytes.
	if len(got) != len(input)*2 {
		t.Errorf("output length = %d, want %d", len(got), len(input)*2)
	}
	// Verify each character decoded back.
	runes := []rune(string(got))
	if len(runes) != len(input) {
		t.Fatalf("rune count = %d, want %d", len(runes), len(input))
	}
	for i, r := range runes {
		if r != rune(input[i]) {
			t.Errorf("rune %d = U+%04X, want U+%04X", i, r, input[i])
		}
	}
}

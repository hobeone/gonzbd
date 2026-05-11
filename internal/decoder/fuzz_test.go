package decoder

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"testing"
)

// --- Fuzz Targets ---

func FuzzDecodeArticle(f *testing.F) {
	// Seed with a minimal valid yEnc single-part article.
	// Raw bytes "hello" → each byte +42 mod 256.
	raw := []byte("hello")
	f.Add(yencEncode("test.bin", raw))

	// Seed with an empty-body article.
	f.Add([]byte("=ybegin line=128 size=0 name=empty.bin\r\n=yend size=0\r\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = DecodeArticle(data)
	})
}

func FuzzDecodeUU(f *testing.F) {
	f.Add([]byte("begin 644 test.bin\n#0V%T\n`\nend\n"))
	f.Add(uuEncode("fuzz.bin", []byte("fuzz me")))

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = DecodeUU(data)
	})
}

// --- M7: Garbage before =ybegin ---

func TestDecodeArticle_GarbageBeforeYbegin(t *testing.T) {
	// The current parseHeader searches for =ybegin anywhere in the body.
	// Prepending garbage should still find the header and decode correctly.
	raw := []byte("hello")
	checksum := crc32.ChecksumIEEE(raw)

	var buf bytes.Buffer
	buf.WriteString("garbage noise before header\r\n")
	buf.WriteString("more junk\r\n")
	fmt.Fprintf(&buf, "=ybegin line=128 size=%d name=test.bin\r\n", len(raw))
	for _, b := range raw {
		enc := byte((int(b) + 42) % 256)
		if enc == 0 || enc == '\n' || enc == '\r' || enc == '=' {
			buf.WriteByte('=')
			enc = byte((int(enc) + 64) % 256)
		}
		buf.WriteByte(enc)
	}
	buf.WriteString("\r\n")
	fmt.Fprintf(&buf, "=yend size=%d crc32=%08x\r\n", len(raw), checksum)

	art, err := DecodeArticle(buf.Bytes())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(art.Data, raw) {
		t.Errorf("decoded data = %q, want %q", art.Data, raw)
	}
	if art.Filename != "test.bin" {
		t.Errorf("Filename = %q, want test.bin", art.Filename)
	}
}

// --- M8: Truncated before trailer ---

func TestDecodeArticle_TruncatedBeforeTrailer(t *testing.T) {
	// Valid header, some encoded data, then EOF — no =yend line.
	body := []byte("=ybegin line=128 size=5 name=trunc.bin\r\nSOMEDATA\r\n")

	_, err := DecodeArticle(body)
	if !errors.Is(err, errMissingTrailer) {
		t.Errorf("got %v, want errMissingTrailer", err)
	}
}

// --- M9: UU truncated line ---

func TestDecodeUU_TruncatedLine(t *testing.T) {
	// UU line where rawLen claims more bytes than the line has encoded
	// characters. The decoder pads short lines with spaces, so this
	// should produce output without panic.
	body := []byte("begin 644 test.txt\n" +
		"-AB\n" + // '-' = 0x2d → rawLen = (0x2d-0x20)&0x3f = 13, but only 2 encoded chars
		"`\n" +
		"end\n")

	data, filename, err := DecodeUU(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filename != "test.txt" {
		t.Errorf("filename = %q, want test.txt", filename)
	}
	// Should produce some output (padded) without crashing.
	if len(data) != 13 {
		t.Errorf("len(data) = %d, want 13 (rawLen from length byte)", len(data))
	}
}

// --- L4: Control chars in filename ---

func TestDecodeArticle_ControlCharsInFilename(t *testing.T) {
	// The yEnc name= field may contain embedded control characters.
	// The decoder returns the filename as-is — it is the caller's
	// responsibility to sanitize filenames for the filesystem.
	//
	// Note: newline (\n) cannot be embedded because the name= field
	// runs to end-of-line per the yEnc spec. Null bytes and tabs are
	// valid test subjects for control-char passthrough.
	nameWithControls := "test\x00file\tname.bin"
	raw := []byte("hi")
	checksum := crc32.ChecksumIEEE(raw)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=ybegin line=128 size=%d name=%s\r\n", len(raw), nameWithControls)
	for _, b := range raw {
		enc := byte((int(b) + 42) % 256)
		if enc == 0 || enc == '\n' || enc == '\r' || enc == '=' {
			buf.WriteByte('=')
			enc = byte((int(enc) + 64) % 256)
		}
		buf.WriteByte(enc)
	}
	buf.WriteString("\r\n")
	fmt.Fprintf(&buf, "=yend size=%d crc32=%08x\r\n", len(raw), checksum)

	art, err := DecodeArticle(buf.Bytes())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(art.Data, raw) {
		t.Errorf("decoded data = %q, want %q", art.Data, raw)
	}
	// The decoder preserves control characters in the filename.
	// Callers (e.g., the assembler) are responsible for sanitization.
	if art.Filename != nameWithControls {
		t.Errorf("Filename = %q, want %q", art.Filename, nameWithControls)
	}
	t.Logf("Filename with control chars: %q (len=%d)", art.Filename, len(art.Filename))
}

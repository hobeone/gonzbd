package decoder

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"testing"
)

// yencEncode produces a well-formed single-part yEnc article from raw data.
// It exists only to generate test fixtures within this package.
func yencEncode(name string, raw []byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=ybegin line=128 size=%d name=%s\r\n", len(raw), name)

	lineLen := 128
	encoded := make([]byte, 0, len(raw)+len(raw)/32)
	for _, b := range raw {
		enc := byte((int(b) + 42) % 256)
		if enc == 0 || enc == '\n' || enc == '\r' || enc == '=' {
			encoded = append(encoded, '=')
			enc = byte((int(enc) + 64) % 256)
		}
		encoded = append(encoded, enc)
	}
	for i := 0; i < len(encoded); i += lineLen {
		end := i + lineLen
		if end > len(encoded) {
			end = len(encoded)
		}
		buf.Write(encoded[i:end])
		buf.WriteString("\r\n")
	}

	checksum := crc32.ChecksumIEEE(raw)
	fmt.Fprintf(&buf, "=yend size=%d crc32=%08x\r\n", len(raw), checksum)
	return buf.Bytes()
}

// yencEncodePart produces a well-formed multi-part yEnc article.
// beginOffset and endOffset are 1-based as per the yEnc spec.
func yencEncodePart(name string, partNum, totalParts int, raw []byte, fileSize, beginOffset, endOffset int64) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=ybegin part=%d total=%d line=128 size=%d name=%s\r\n",
		partNum, totalParts, fileSize, name)
	fmt.Fprintf(&buf, "=ypart begin=%d end=%d\r\n", beginOffset, endOffset)

	lineLen := 128
	encoded := make([]byte, 0, len(raw)+len(raw)/32)
	for _, b := range raw {
		enc := byte((int(b) + 42) % 256)
		if enc == 0 || enc == '\n' || enc == '\r' || enc == '=' {
			encoded = append(encoded, '=')
			enc = byte((int(enc) + 64) % 256)
		}
		encoded = append(encoded, enc)
	}
	for i := 0; i < len(encoded); i += lineLen {
		end := i + lineLen
		if end > len(encoded) {
			end = len(encoded)
		}
		buf.Write(encoded[i:end])
		buf.WriteString("\r\n")
	}

	checksum := crc32.ChecksumIEEE(raw)
	fmt.Fprintf(&buf, "=yend size=%d part=%d pcrc32=%08x\r\n", len(raw), partNum, checksum)
	return buf.Bytes()
}

// makeRaw returns a deterministic byte slice of the given length.
// Using a simple counter ensures every byte value (0–255) appears.
func makeRaw(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i % 256)
	}
	return b
}

func TestDecodeArticle_SinglePart(t *testing.T) {
	raw := makeRaw(1000)
	article := yencEncode("test.bin", raw)

	art, err := DecodeArticle(article)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(art.Data, raw) {
		t.Errorf("decoded data does not match original")
	}
	if art.Offset != 0 {
		t.Errorf("expected offset=0, got %d", art.Offset)
	}
	if int64(len(art.Data)) != int64(len(raw)) {
		t.Errorf("expected len(Data)=%d, got %d", len(raw), len(art.Data))
	}
	if art.TotalSize != int64(len(raw)) {
		t.Errorf("expected TotalSize=%d, got %d", len(raw), art.TotalSize)
	}
	if art.Filename != "test.bin" {
		t.Errorf("Filename = %q, want test.bin", art.Filename)
	}
	want := crc32.ChecksumIEEE(raw)
	if art.CRC != want {
		t.Errorf("CRC mismatch: got 0x%08x, want 0x%08x", art.CRC, want)
	}
}

func TestDecodeArticle_MultiPart(t *testing.T) {
	full := makeRaw(2000)
	part := full[:1000]
	fileSize := int64(len(full))
	article := yencEncodePart("test.bin", 1, 2, part, fileSize, 1, 1000)

	art, err := DecodeArticle(article)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(art.Data, part) {
		t.Errorf("decoded data does not match part")
	}
	if art.Offset != 0 {
		t.Errorf("expected offset=0 (begin=1 → 0-based), got %d", art.Offset)
	}
	if int64(len(art.Data)) != int64(len(part)) {
		t.Errorf("expected len(Data)=%d, got %d", len(part), len(art.Data))
	}
	if art.TotalSize != fileSize {
		t.Errorf("expected TotalSize=%d, got %d", fileSize, art.TotalSize)
	}
	want := crc32.ChecksumIEEE(part)
	if art.CRC != want {
		t.Errorf("CRC mismatch: got 0x%08x, want 0x%08x", art.CRC, want)
	}
}

func TestDecodeArticle_MultiPart_NonZeroOffset(t *testing.T) {
	full := makeRaw(2000)
	part := full[1000:]
	fileSize := int64(len(full))
	article := yencEncodePart("test.bin", 2, 2, part, fileSize, 1001, 2000)

	art, err := DecodeArticle(article)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(art.Data, part) {
		t.Errorf("decoded data does not match part")
	}
	if art.Offset != 1000 {
		t.Errorf("expected offset=1000, got %d", art.Offset)
	}
	if int64(len(art.Data)) != int64(len(part)) {
		t.Errorf("expected len(Data)=%d, got %d", len(part), len(art.Data))
	}
	if art.TotalSize != fileSize {
		t.Errorf("expected TotalSize=%d, got %d", fileSize, art.TotalSize)
	}
	want := crc32.ChecksumIEEE(part)
	if art.CRC != want {
		t.Errorf("CRC mismatch: got 0x%08x, want 0x%08x", art.CRC, want)
	}
}

// TestDecodeArticle_Byte0xD6 is the critical correctness test.
//
// Raw byte 0xd6 (214) encodes as (214+42) % 256 = 0, which must be escaped
// as '=' followed by (0+64) % 256 = '@'. When this escape sequence happens
// to fall at a line boundary — '=' at the end of one line, '@' at the start
// of the next — decoders that reset escape state on newlines produce corrupt
// output (they subtract 42 from '@' instead of 106). This test forces that
// exact scenario by placing 0xd6 such that '=' is the 128th encoded character
// and '@' is the first character of the next line.
func TestDecodeArticle_Byte0xD6(t *testing.T) {
	// We need exactly 127 single-encoding raw bytes before 0xd6 so that the
	// encoded output is: [127 safe bytes] [=] \r\n [@] \r\n.
	// A raw byte is single-encoding when (byte+42)%256 is not in {0,10,13,61}.
	// Unsafe raw bytes: {19, 214, 224, 227}. We collect the first 127 safe bytes.
	unsafe := map[byte]bool{19: true, 214: true, 224: true, 227: true}
	raw := make([]byte, 0, 128)
	for b := byte(0); len(raw) < 127; b++ {
		if !unsafe[b] {
			raw = append(raw, b)
		}
	}
	raw = append(raw, 0xd6)

	article := yencEncode("trap.bin", raw)

	// Verify the article actually encodes the escape across the line boundary.
	// The 128th encoded byte should be '=' and the 129th (after CRLF) '@'.
	if !containsEscapeAcrossLine(article) {
		t.Logf("article:\n%q", article)
		t.Fatal("test setup error: escape sequence is not split across line boundary")
	}

	art, err := DecodeArticle(article)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(art.Data, raw) {
		for i := range raw {
			if i < len(art.Data) && art.Data[i] != raw[i] {
				t.Errorf("first mismatch at byte %d: got 0x%02x, want 0x%02x", i, art.Data[i], raw[i])
				break
			}
		}
		t.Errorf("0xd6 cross-line escape decoded incorrectly (len got=%d want=%d)", len(art.Data), len(raw))
	}
}

// containsEscapeAcrossLine returns true if a '=' character is the last
// non-CRLF byte before a CRLF in the yEnc body.
func containsEscapeAcrossLine(article []byte) bool {
	lines := bytes.Split(article, []byte("\r\n"))
	for _, line := range lines {
		if len(line) > 0 && line[len(line)-1] == '=' {
			return true
		}
	}
	return false
}

// TestDecodeArticle_EscapeAcrossLineBoundary explicitly tests the boundary case
// with a hand-crafted article where '=' is the last character on a line and the
// escaped byte starts the next line.
func TestDecodeArticle_EscapeAcrossLineBoundary(t *testing.T) {
	// Craft an article manually: =ybegin, then a body line ending with '=',
	// then the continuation byte '@', then =yend.
	// '=' '@' decodes as: @ - 64 - 42 = 64 - 64 - 42 ... wait, let's be precise.
	// yEnc rule: escaped byte: output = escapedChar - 64 - 42 = '@' - 106 = 64 - 106 mod 256.
	// 64 - 106 = -42 mod 256 = 214 = 0xd6. Correct.
	raw := []byte{0xd6}
	checksum := crc32.ChecksumIEEE(raw)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=ybegin line=128 size=1 name=boundary.bin\r\n")
	// Body: '=' on its own line, '@' on the next.
	buf.WriteString("=\r\n")
	buf.WriteString("@\r\n")
	fmt.Fprintf(&buf, "=yend size=1 crc32=%08x\r\n", checksum)

	article := buf.Bytes()

	art, err := DecodeArticle(article)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(art.Data, raw) {
		t.Errorf("got %v, want %v", art.Data, raw)
	}
}

func TestDecodeArticle_AllByteValues(t *testing.T) {
	// Round-trip every byte value 0x00–0xff through encode/decode.
	raw := make([]byte, 256)
	for i := range raw {
		raw[i] = byte(i)
	}
	article := yencEncode("allbytes.bin", raw)

	art, err := DecodeArticle(article)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(art.Data, raw) {
		for i := range raw {
			if i < len(art.Data) && art.Data[i] != raw[i] {
				t.Errorf("first mismatch at byte %d (0x%02x): got 0x%02x", i, raw[i], art.Data[i])
				break
			}
		}
		t.Errorf("all-bytes round-trip failed (len got=%d want=%d)", len(art.Data), len(raw))
	}
}

func TestDecodeArticle_MalformedInputs(t *testing.T) {
	raw := makeRaw(100)
	good := yencEncode("good.bin", raw)
	checksum := crc32.ChecksumIEEE(raw)

	// Build a corrupt-CRC variant.
	corruptCRC := yencEncode("bad.bin", raw)
	// Replace the crc32 value in the =yend line with a wrong value.
	corruptCRC = bytes.Replace(corruptCRC,
		[]byte(fmt.Sprintf("crc32=%08x", checksum)),
		[]byte("crc32=deadbeef"),
		1)

	// Build a size-mismatch variant: declare size=99 but encode 100 bytes.
	sizeMismatch := bytes.Replace(good,
		[]byte(fmt.Sprintf("=ybegin line=128 size=%d", len(raw))),
		[]byte("=ybegin line=128 size=99"),
		1)
	sizeMismatch = bytes.Replace(sizeMismatch,
		[]byte(fmt.Sprintf("=yend size=%d", len(raw))),
		[]byte("=yend size=99"),
		1)

	cases := []struct {
		name    string
		input   []byte
		wantErr error
	}{
		{"empty", []byte{}, ErrNotYEnc},
		{"garbage", []byte("this is not yenc data"), ErrNotYEnc},
		{"truncated_header", []byte("=ybegin line=128 siz"), ErrMalformed},
		{"missing_yend", []byte("=ybegin line=128 size=3 name=x\r\nabc\r\n"), ErrMissingTrailer},
		{"corrupt_crc", corruptCRC, ErrCRCMismatch},
		{"size_mismatch", sizeMismatch, ErrSizeMismatch},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeArticle(tc.input)
			if !isErr(err, tc.wantErr) {
				t.Errorf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// isErr reports whether err wraps or equals target.
func isErr(err, target error) bool {
	return errors.Is(err, target)
}

func TestDecodeUU_RoundTrip(t *testing.T) {
	raw := []byte("Hello, UU world!")
	encoded := uuEncode("hello.txt", raw)

	data, filename, err := DecodeUU(encoded)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filename != "hello.txt" {
		t.Errorf("filename: got %q, want %q", filename, "hello.txt")
	}
	if !bytes.Equal(data, raw) {
		t.Errorf("decoded %q, want %q", data, raw)
	}
}

func TestDecodeUU_AllByteValues(t *testing.T) {
	raw := make([]byte, 256)
	for i := range raw {
		raw[i] = byte(i)
	}
	encoded := uuEncode("allbytes.bin", raw)
	data, _, err := DecodeUU(encoded)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(data, raw) {
		for i := range raw {
			if i < len(data) && data[i] != raw[i] {
				t.Errorf("first mismatch at byte %d: got 0x%02x, want 0x%02x", i, data[i], raw[i])
				break
			}
		}
		t.Errorf("round-trip failed (got %d bytes, want %d)", len(data), len(raw))
	}
}

func TestDecodeUU_MalformedInputs(t *testing.T) {
	cases := []struct {
		name    string
		input   []byte
		wantErr error
	}{
		{"empty", []byte{}, ErrNotUU},
		{"garbage", []byte("not uu encoded\n"), ErrNotUU},
		{"no_begin", []byte("hello world\nend\n"), ErrNotUU},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := DecodeUU(tc.input)
			if !isErr(err, tc.wantErr) {
				t.Errorf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// uuEncode is a test helper that produces valid UU-encoded output.
// Standard line length is 45 bytes (60 encoded characters).
func uuEncode(filename string, data []byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "begin 644 %s\n", filename)

	lineSize := 45
	for i := 0; i < len(data); i += lineSize {
		end := i + lineSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[i:end]
		// Length character.
		buf.WriteByte(byte(len(chunk)) + 0x20)
		// Encode groups of 3.
		for j := 0; j < len(chunk); j += 3 {
			var a, b, c byte
			a = chunk[j]
			if j+1 < len(chunk) {
				b = chunk[j+1]
			}
			if j+2 < len(chunk) {
				c = chunk[j+2]
			}
			buf.WriteByte(((a >> 2) & 0x3f) + 0x20)
			buf.WriteByte((((a << 4) | (b >> 4)) & 0x3f) + 0x20)
			buf.WriteByte((((b << 2) | (c >> 6)) & 0x3f) + 0x20)
			buf.WriteByte((c & 0x3f) + 0x20)
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("`\nend\n")
	return buf.Bytes()
}

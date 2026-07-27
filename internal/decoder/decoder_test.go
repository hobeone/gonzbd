package decoder

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"testing"
	"unsafe"
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
		end := min(i+lineLen, len(encoded))
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
		end := min(i+lineLen, len(encoded))
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

func TestDecodeArticle_StaticVector(t *testing.T) {
	// Static yEnc vector for payload "ABC" (ASCII 65, 66, 67 -> +42 -> 107 'k', 108 'l', 109 'm').
	// CRC32(IEEE) of "ABC" is 0xa3830348.
	input := []byte("=ybegin line=128 size=3 name=static.txt\r\nklm\r\n=yend size=3 crc32=a3830348\r\n")

	art, err := DecodeArticle(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(art.Data, []byte("ABC")) {
		t.Errorf("decoded data = %q, want ABC", art.Data)
	}
	if art.CRC != 0xa3830348 {
		t.Errorf("CRC mismatch: got 0x%08x, want 0xa3830348", art.CRC)
	}

	// Negative static check: corrupted header CRC must return ErrCRCMismatch.
	corruptInput := []byte("=ybegin line=128 size=3 name=static.txt\r\nklm\r\n=yend size=3 crc32=deadbeef\r\n")
	_, err = DecodeArticle(corruptInput)
	if !errors.Is(err, ErrCRCMismatch) {
		t.Errorf("expected ErrCRCMismatch for corrupt static CRC, got %v", err)
	}
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
	// Verify against statically computed CRC of makeRaw(1000) rather than tautologically
	// calling crc32.ChecksumIEEE(raw) which mirrors the encoder.
	const expectedRaw1000CRC uint32 = 0x74e3fb41
	if art.CRC != expectedRaw1000CRC {
		t.Errorf("CRC mismatch: got 0x%08x, want 0x%08x", art.CRC, expectedRaw1000CRC)
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
	const expectedRaw1000CRC uint32 = 0x74e3fb41
	if art.CRC != expectedRaw1000CRC {
		t.Errorf("CRC mismatch: got 0x%08x, want 0x%08x", art.CRC, expectedRaw1000CRC)
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
	lines := bytes.SplitSeq(article, []byte("\r\n"))
	for line := range lines {
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
		fmt.Appendf(nil, "crc32=%08x", checksum),
		[]byte("crc32=deadbeef"),
		1)

	// Build a size-mismatch variant: declare size=99 but encode 100 bytes.
	sizeMismatch := bytes.Replace(good,
		fmt.Appendf(nil, "=ybegin line=128 size=%d", len(raw)),
		[]byte("=ybegin line=128 size=99"),
		1)
	sizeMismatch = bytes.Replace(sizeMismatch,
		fmt.Appendf(nil, "=yend size=%d", len(raw)),
		[]byte("=yend size=99"),
		1)

	// Build a negative-size variant.
	negativeSize := bytes.Replace(good,
		fmt.Appendf(nil, "=ybegin line=128 size=%d", len(raw)),
		[]byte("=ybegin line=128 size=-100"),
		1)

	cases := []struct {
		name    string
		input   []byte
		wantErr error
	}{
		{"empty", []byte{}, ErrNotYEnc},
		{"garbage", []byte("this is not yenc data"), ErrNotYEnc},
		{"truncated_header", []byte("=ybegin line=128 siz"), errMalformed},
		{"missing_yend", []byte("=ybegin line=128 size=3 name=x\r\nabc\r\n"), errMissingTrailer},
		{"corrupt_crc", corruptCRC, ErrCRCMismatch},
		{"size_mismatch", sizeMismatch, errSizeMismatch},
		{"negative_size", negativeSize, nil},
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
		end := min(i+lineSize, len(data))
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

func TestDecodeUUShortLinePadding(t *testing.T) {
	body := []byte("begin 644 test.txt\n" +
		"!00\n" + // short line: 1 byte of payload ('A'), only 2 encoded chars
		"#04%!\n" + // another line: 3 bytes payload ("AAA"), 4 encoded chars
		"`\n" +
		"end\n")

	originalBody := make([]byte, len(body))
	copy(originalBody, body)

	data, filename, err := DecodeUU(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if filename != "test.txt" {
		t.Errorf("expected filename %q, got %q", "test.txt", filename)
	}

	expectedData := []byte{'A', 'A', 'A', 'A'}
	if !bytes.Equal(data, expectedData) {
		t.Errorf("expected data %q, got %q", expectedData, data)
	}

	if !bytes.Equal(body, originalBody) {
		t.Errorf("input buffer was corrupted!\noriginal: %q\ncurrent:  %q", originalBody, body)
	}
}

// TestDecodeArticle_HugeSizeHint verifies that a yEnc article with an
// inflated size= field in the header does not cause excessive memory
// allocation. The decoded output is bounded by len(body), not by the
// attacker-controlled size= value.
func TestDecodeArticle_HugeSizeHint(t *testing.T) {
	// Craft a small yEnc article with a wildly inflated size= header.
	raw := []byte("hello")
	checksum := crc32.ChecksumIEEE(raw)

	var buf bytes.Buffer
	// Declare size=999999999 but the actual body is tiny.
	fmt.Fprintf(&buf, "=ybegin line=128 size=999999999 name=huge.bin\r\n")
	for _, b := range raw {
		enc := byte((int(b) + 42) % 256)
		if enc == 0 || enc == '\n' || enc == '\r' || enc == '=' {
			buf.WriteByte('=')
			enc = byte((int(enc) + 64) % 256)
		}
		buf.WriteByte(enc)
	}
	buf.WriteString("\r\n")
	// The trailer declares the correct part size.
	fmt.Fprintf(&buf, "=yend size=%d crc32=%08x\r\n", len(raw), checksum)

	art, err := DecodeArticle(buf.Bytes())
	// We expect a size mismatch error since trailer size != header size,
	// but crucially, we should NOT have allocated 999999999 bytes.
	if err != nil && !errors.Is(err, errSizeMismatch) {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(art.Data, raw) {
		t.Errorf("decoded data does not match: got %q, want %q", art.Data, raw)
	}
}

// TestDecodeArticle_ExceedsMaxSize verifies that DecodeArticle rejects
// bodies larger than maxDecodeSize (10 MB).
func TestDecodeArticle_ExceedsMaxSize(t *testing.T) {
	// Create a body that exceeds maxDecodeSize. We don't need valid yEnc
	// content — the size check happens before parsing.
	body := make([]byte, maxDecodeSize+1)
	copy(body, []byte("=ybegin"))

	_, err := DecodeArticle(body)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("DecodeArticle(body of %d bytes) error = %v, want ErrBodyTooLarge", len(body), err)
	}
}

// TestDecodeUU_ExceedsMaxSize verifies that DecodeUU rejects bodies
// larger than maxDecodeSize (10 MB).
func TestDecodeUU_ExceedsMaxSize(t *testing.T) {
	body := make([]byte, maxDecodeSize+1)
	copy(body, []byte("begin 644 test.bin\n"))

	_, _, err := DecodeUU(body)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("DecodeUU(body of %d bytes) error = %v, want ErrBodyTooLarge", len(body), err)
	}
}

// TestDecodeArticle_AtMaxSize verifies that a body exactly at maxDecodeSize
// is accepted (boundary condition).
func TestDecodeArticle_AtMaxSize(t *testing.T) {
	body := make([]byte, maxDecodeSize)
	copy(body, []byte("=ybegin"))

	// The body is malformed yEnc, but should NOT trigger ErrBodyTooLarge.
	_, err := DecodeArticle(body)
	if errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("DecodeArticle(body of exactly maxDecodeSize) should not return ErrBodyTooLarge")
	}
}

func TestDecoderUnexportedHelpersDirect(t *testing.T) {
	t.Parallel()

	t.Run("sub42Span", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			dst  []byte
			src  []byte
			want []byte
		}{
			{
				name: "empty_src_nil_dst",
				dst:  nil, src: nil,
				want: nil,
			},
			{
				name: "empty_src_nonempty_dst",
				dst:  []byte{1, 2, 3}, src: nil,
				want: []byte{1, 2, 3},
			},
			{
				// n=3: only the scalar loop runs (n < 8).
				name: "scalar_only_n3",
				dst:  nil, src: []byte{50, 51, 52},
				want: []byte{8, 9, 10},
			},
			{
				// n=8: exactly one pass of the 8× unrolled loop; scalar loop gets 0 iterations.
				name: "unrolled_only_n8",
				dst:  nil, src: []byte{50, 51, 52, 53, 54, 55, 56, 57},
				want: []byte{8, 9, 10, 11, 12, 13, 14, 15},
			},
			{
				// src bytes < 42 wrap to high values inside the unrolled loop.
				// byte(0)-42 = 214, same arithmetic as the hot-path 0xd6 correctness rule.
				name: "unrolled_underflow_wrap_n8",
				dst:  nil, src: []byte{0, 0, 0, 0, 0, 0, 0, 0},
				want: []byte{214, 214, 214, 214, 214, 214, 214, 214},
			},
			{
				// n=9: one 8× pass then one scalar iteration.
				name: "unrolled_plus_scalar_n9",
				dst:  nil, src: []byte{50, 51, 52, 53, 54, 55, 56, 57, 58},
				want: []byte{8, 9, 10, 11, 12, 13, 14, 15, 16},
			},
			{
				// n=16: two full 8× passes; scalar loop gets 0 iterations.
				name: "two_unrolled_passes_n16",
				dst:  nil,
				src:  []byte{42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53, 54, 55, 56, 57},
				want: []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			},
			{
				name: "nonempty_dst_preserves_prefix",
				dst:  []byte{99, 100}, src: []byte{42, 43, 44},
				want: []byte{99, 100, 0, 1, 2},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := sub42Span(tc.dst, tc.src)
				if !bytes.Equal(got, tc.want) {
					t.Errorf("sub42Span(%v, %v) = %v, want %v", tc.dst, tc.src, got, tc.want)
				}
			})
		}

		// Verify the no-grow path: when dst already has sufficient spare capacity,
		// sub42Span extends it in-place and produces the same decoded output.
		t.Run("preallocated_dst_no_grow", func(t *testing.T) {
			src := []byte{50, 51, 52, 53, 54, 55, 56, 57}
			dst := make([]byte, 2, 20)
			dst[0], dst[1] = 99, 100
			got := sub42Span(dst, src)
			want := []byte{99, 100, 8, 9, 10, 11, 12, 13, 14, 15}
			if !bytes.Equal(got, want) {
				t.Errorf("pre-allocated: got %v, want %v", got, want)
			}
		})

		// Verify exact capacity no-alloc behavior to kill boundary mutant
		t.Run("sub42Span_exact_capacity_no_alloc", func(t *testing.T) {
			dst := make([]byte, 2, 5)
			dst[0], dst[1] = 99, 100
			src := []byte{42, 43, 44} // n=3, matches spare capacity exactly
			got := sub42Span(dst, src)
			if &got[0] != &dst[0] {
				t.Error("sub42Span allocated a new backing array when spare capacity was exactly sufficient")
			}
		})
	})

	t.Run("indexSpecial", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name  string
			input []byte
			want  int
		}{
			{"no_specials", []byte("abcdef"), -1},
			{"equals_start", []byte("=abcdef"), 0},
			{"equals_mid", []byte("abc=def"), 3},
			{"newline_mid", []byte("abc\ndef"), 3},
			{"cr_mid", []byte("abc\rdef"), 3},
			{"multiple_first_wins", []byte("abc\rde=f"), 3},
			{"empty", []byte(""), -1},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := indexSpecial(tc.input)
				if got != tc.want {
					t.Errorf("indexSpecial(%q) = %d, want %d", tc.input, got, tc.want)
				}
			})
		}
	})

	t.Run("decodeUULine", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name   string
			enc    []byte
			rawLen int
			want   []byte
		}{
			{
				name: "rawLen_0", enc: nil, rawLen: 0,
				want: []byte{},
			},
			{
				// 'A'=65. Encoded group for one byte: (65>>2+32)='0', (65<<4&63+32)='0', ' ', ' '.
				name: "rawLen_1_A", enc: []byte("00  "), rawLen: 1,
				want: []byte{'A'},
			},
			{
				// "He" encodes as "2&4 " (4-char group, 2nd byte stops the loop).
				name: "rawLen_2_He", enc: []byte("2&4 "), rawLen: 2,
				want: []byte("He"),
			},
			{
				// Full 3-byte group, no padding required.
				name: "rawLen_3_Cat", enc: []byte("0V%T"), rawLen: 3,
				want: []byte("Cat"),
			},
			{
				// enc has only 3 chars for a 3-byte decode; the 4th is padded with ' ' (→ d=0).
				// Padding changes byte3: (c<<6)|0 = 5<<6 as uint8 = 64 = '@', not 't'.
				name: "short_enc_pads_with_space", enc: []byte("0V%"), rawLen: 3,
				want: []byte("Ca@"),
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := decodeUULine(tc.enc, tc.rawLen)
				if !bytes.Equal(got, tc.want) {
					t.Errorf("decodeUULine(%q, %d) = %q, want %q", tc.enc, tc.rawLen, got, tc.want)
				}
				if len(got) != tc.rawLen {
					t.Errorf("decodeUULine returned %d bytes, want exactly %d", len(got), tc.rawLen)
				}
			})
		}

		t.Run("decodeUULine_capacity", func(t *testing.T) {
			got1 := decodeUULine([]byte("00  "), 1)
			if cap(got1) != 1 {
				t.Errorf("expected cap 1, got %d", cap(got1))
			}
			got2 := decodeUULine([]byte("2&4 "), 2)
			if cap(got2) != 2 {
				t.Errorf("expected cap 2, got %d", cap(got2))
			}
		})
	})
}

func TestDecoderMetadataParsingDirect(t *testing.T) {
	t.Parallel()

	t.Run("parseHeader", func(t *testing.T) {
		t.Run("missing ybegin", func(t *testing.T) {
			body := []byte("hello world")
			_, _, err := parseHeader(body)
			if !errors.Is(err, ErrNotYEnc) {
				t.Errorf("expected ErrNotYEnc, got %v", err)
			}
		})

		t.Run("malformed line end", func(t *testing.T) {
			body := []byte("=ybegin size=100") // no newline
			_, _, err := parseHeader(body)
			if !errors.Is(err, errMalformed) {
				t.Errorf("expected errMalformed, got %v", err)
			}
		})

		t.Run("single part ybegin", func(t *testing.T) {
			body := []byte("=ybegin size=1234 name=test_file.bin\nbody_data")
			hdr, bodyStart, err := parseHeader(body)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if hdr.size != 1234 {
				t.Errorf("expected size 1234, got %d", hdr.size)
			}
			if hdr.name != "test_file.bin" {
				t.Errorf("expected name 'test_file.bin', got %q", hdr.name)
			}
			if hdr.isPart {
				t.Error("expected isPart false")
			}
			if bodyStart != len("=ybegin size=1234 name=test_file.bin\n") {
				t.Errorf("unexpected bodyStart: %d", bodyStart)
			}
		})

		t.Run("multipart ybegin and ypart", func(t *testing.T) {
			body := []byte("=ybegin size=100000 part=12 name=multi.bin\n=ypart begin=1001 end=2000\nbody_data")
			hdr, bodyStart, err := parseHeader(body)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if hdr.size != 100000 {
				t.Errorf("expected size 100000, got %d", hdr.size)
			}
			if hdr.name != "multi.bin" {
				t.Errorf("expected name 'multi.bin', got %q", hdr.name)
			}
			if !hdr.isPart {
				t.Error("expected isPart true")
			}
			if hdr.offset != 1000 { // 1001-1 (0-based)
				t.Errorf("expected offset 1000, got %d", hdr.offset)
			}
			expectedStart := len("=ybegin size=100000 part=12 name=multi.bin\n=ypart begin=1001 end=2000\n")
			if bodyStart != expectedStart {
				t.Errorf("expected bodyStart %d, got %d", expectedStart, bodyStart)
			}
		})

		t.Run("ypart malformed", func(t *testing.T) {
			body := []byte("=ybegin size=100000 part=12 name=multi.bin\n=ypart begin=1001") // no newline on ypart
			_, _, err := parseHeader(body)
			if !errors.Is(err, errMalformed) {
				t.Errorf("expected errMalformed, got %v", err)
			}
		})
	})

	t.Run("parseTrailer", func(t *testing.T) {
		t.Run("missing yend prefix", func(t *testing.T) {
			_, err := parseTrailer([]byte("=not_yend size=123"), false)
			if !errors.Is(err, errMissingTrailer) {
				t.Errorf("expected errMissingTrailer, got %v", err)
			}
		})

		t.Run("single part trailer", func(t *testing.T) {
			line := []byte("=yend size=1234 crc32=abcdef12\n")
			tr, err := parseTrailer(line, false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tr.size != 1234 {
				t.Errorf("expected size 1234, got %d", tr.size)
			}
			if tr.crc != 0xabcdef12 {
				t.Errorf("expected crc 0xabcdef12, got 0x%x", tr.crc)
			}
			if !tr.valid {
				t.Error("expected valid true")
			}
		})

		t.Run("multipart trailer", func(t *testing.T) {
			line := []byte("=yend size=500 part=2 pcrc32=12345678\n")
			tr, err := parseTrailer(line, true)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tr.size != 500 {
				t.Errorf("expected size 500, got %d", tr.size)
			}
			if tr.crc != 0x12345678 {
				t.Errorf("expected crc 0x12345678, got 0x%x", tr.crc)
			}
			if !tr.valid {
				t.Error("expected valid true")
			}
		})
	})

	t.Run("parseKeyValues callback", func(t *testing.T) {
		line := []byte("=ybegin size=500 name=some name with spaces.bin\r\n")
		var keys, values []string
		parseKeyValues(line, func(k, v string) {
			keys = append(keys, k)
			values = append(values, v)
		})
		if len(keys) != 2 {
			t.Fatalf("expected 2 keys, got %d", len(keys))
		}
		if keys[0] != "size" || values[0] != "500" {
			t.Errorf("expected size=500, got %s=%s", keys[0], values[0])
		}
		if keys[1] != "name" || values[1] != "some name with spaces.bin" {
			t.Errorf("expected name='some name with spaces.bin', got %s=%s", keys[1], values[1])
		}
	})

	t.Run("parseKeyValues name field with trailing whitespace and null", func(t *testing.T) {
		cases := []struct {
			name     string
			line     []byte
			wantName string
		}{
			{
				name:     "trailing space",
				line:     []byte("=ybegin size=100 name=file.bin \r\n"),
				wantName: "file.bin",
			},
			{
				name:     "trailing tab",
				line:     []byte("=ybegin size=100 name=file.bin\t\r\n"),
				wantName: "file.bin",
			},
			{
				name:     "trailing null byte",
				line:     []byte("=ybegin size=100 name=file.bin\x00\r\n"),
				wantName: "file.bin",
			},
			{
				name:     "trailing space and tab",
				line:     []byte("=ybegin size=100 name=file.bin \t\r\n"),
				wantName: "file.bin",
			},
			{
				name:     "trailing space tab and null",
				line:     []byte("=ybegin size=100 name=file.bin \t\x00\r\n"),
				wantName: "file.bin",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var gotName string
				parseKeyValues(tc.line, func(k, v string) {
					if k == "name" {
						gotName = v
					}
				})
				if gotName != tc.wantName {
					t.Errorf("expected name=%q, got %q", tc.wantName, gotName)
				}
			})
		}
	})

	t.Run("ypart with trailing spaces", func(t *testing.T) {
		body := []byte("=ybegin size=100000 part=12 name=multi.bin\n=ypart begin=1001 end=2000    \nbody_data")
		hdr, _, err := parseHeader(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hdr.offset != 1000 {
			t.Errorf("expected offset 1000, got %d", hdr.offset)
		}
	})

	t.Run("trailer with trailing spaces", func(t *testing.T) {
		line := []byte("=yend size=1234 crc32=abcdef12    \n")
		tr, err := parseTrailer(line, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tr.size != 1234 || tr.crc != 0xabcdef12 {
			t.Errorf("incorrect parse: %+v", tr)
		}
	})
}

func TestDecodeArticle_ExtraLinesAfterYend(t *testing.T) {
	raw := []byte("hello")
	checksum := crc32.ChecksumIEEE(raw)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=ybegin line=128 size=%d name=test.bin\r\n", len(raw))
	for _, b := range raw {
		buf.WriteByte(b + 42)
	}
	buf.WriteString("\r\n")
	fmt.Fprintf(&buf, "=yend size=%d crc32=%08x\r\n", len(raw), checksum)
	buf.WriteString("some random signature or newsreader info\r\n")
	buf.WriteString("size=999999 crc32=deadbeef\r\n")

	art, err := DecodeArticle(buf.Bytes())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(art.Data, raw) {
		t.Errorf("decoded data mismatch: got %q, want %q", art.Data, raw)
	}
}

func TestDecodeBody_Preallocation(t *testing.T) {
	encoded := []byte("12345") // 5 bytes

	// Case 1: sizeHint <= 0 (e.g., 0) -> should preallocate to at least maxCap = len(encoded) = 5
	out, _ := decodeBody(encoded, 0, nil)
	if cap(out) < 5 {
		t.Errorf("For sizeHint=0, expected capacity >= 5, got %d", cap(out))
	}

	// Case 2: sizeHint > maxCap (e.g., 100) -> when passing exact scratch, capacity should be capped to 5
	scratch := make([]byte, 0, 5)
	out2, _ := decodeBody(encoded, 100, scratch)
	if cap(out2) != 5 {
		t.Errorf("For sizeHint=100 with scratch cap 5, expected capacity 5, got %d", cap(out2))
	}

	// Case 3: sizeHint is valid (e.g., 5)
	out3, _ := decodeBody(encoded, 5, scratch)
	if cap(out3) != 5 {
		t.Errorf("For sizeHint=5, expected capacity 5, got %d", cap(out3))
	}

	// Case 4: sizeHint > maxCap (negation test, e.g. 6) -> should cap to maxCap = 5 when using exact scratch
	out4, _ := decodeBody(encoded, 6, scratch)
	if cap(out4) != 5 {
		t.Errorf("For sizeHint=6, expected capacity 5, got %d", cap(out4))
	}

	// Case 5: sizeHint is negative (e.g., -5) -> should preallocate to at least maxCap = 5
	out5, _ := decodeBody(encoded, -5, nil)
	if cap(out5) < 5 {
		t.Errorf("For sizeHint=-5, expected capacity >= 5, got %d", cap(out5))
	}
}

func TestDecodeArticle_TrailerImmediatelyAfterNewline(t *testing.T) {
	body := []byte("=ybegin line=128 size=0 name=empty.bin\n\n=yend size=0\n")
	art, err := DecodeArticle(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(art.Data) != 0 {
		t.Errorf("expected empty data, got %q", art.Data)
	}
}

func TestDecodeArticle_Multipart_BeginZero(t *testing.T) {
	full := makeRaw(2000)
	part := full[:1000]
	fileSize := int64(len(full))
	article := yencEncodePart("test.bin", 1, 2, part, fileSize, 0, 1000)

	art, err := DecodeArticle(article)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if art.Offset < 0 {
		t.Errorf("expected non-negative offset, got %d", art.Offset)
	}
}

func TestDecodeArticle_Multipart_CorruptPCRC(t *testing.T) {
	full := makeRaw(2000)
	part := full[:1000]
	fileSize := int64(len(full))
	article := yencEncodePart("test.bin", 1, 2, part, fileSize, 1, 1000)

	checksum := crc32.ChecksumIEEE(part)
	article = bytes.Replace(article,
		fmt.Appendf(nil, "pcrc32=%08x", checksum),
		[]byte("pcrc32=deadbeef"),
		1)

	_, err := DecodeArticle(article)
	if !errors.Is(err, ErrCRCMismatch) {
		t.Errorf("expected ErrCRCMismatch for corrupt pcrc32, got %v", err)
	}
}

func TestLatin1ToUTF8_Boundary128(t *testing.T) {
	input := []byte{128}
	expected := string([]byte{0xC2, 0x80})
	got := latin1ToUTF8(input)
	if got != expected {
		t.Errorf("latin1ToUTF8([128]) = %q (len %d), want %q (len %d)", got, len(got), expected, len(expected))
	}
}

func TestDecodeUU_AtMaxSize(t *testing.T) {
	body := make([]byte, maxDecodeSize)
	copy(body, []byte("begin 644 test.bin\n"))

	_, _, err := DecodeUU(body)
	if errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("DecodeUU(body of exactly maxDecodeSize) should not return ErrBodyTooLarge")
	}
}

func TestDecodeArticleBuf_ScratchReuse(t *testing.T) {
	raw := makeRaw(1000)
	article := yencEncode("test.bin", raw)

	scratch := make([]byte, 0, 2000)
	art, err := DecodeArticleBuf(article, scratch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(art.Data, raw) {
		t.Errorf("decoded data does not match original")
	}
	if unsafe.SliceData(art.Data) != unsafe.SliceData(scratch) {
		t.Errorf("expected art.Data to reuse scratch buffer backing array")
	}
}

func TestDecodeUUBuf_ScratchReuse(t *testing.T) {
	body := []byte("begin 644 test.txt\n#0V%T\n`\nend\n")
	scratch := make([]byte, 0, 100)
	data, filename, err := DecodeUUBuf(body, scratch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filename != "test.txt" {
		t.Errorf("filename = %q, want test.txt", filename)
	}
	if string(data) != "Cat" {
		t.Errorf("data = %q, want Cat", string(data))
	}
	if len(data) > 0 && unsafe.SliceData(data) != unsafe.SliceData(scratch) {
		t.Errorf("expected data to reuse scratch buffer backing array")
	}
}

func TestDecodePool_GetPut(t *testing.T) {
	buf := GetBuffer(1024)
	if cap(buf) < 1024 {
		t.Fatalf("expected cap >= 1024, got %d", cap(buf))
	}
	if len(buf) != 0 {
		t.Fatalf("expected len == 0, got %d", len(buf))
	}
	buf = append(buf, []byte("hello")...)
	PutBuffer(buf)

	buf2 := GetBuffer(1024)
	if len(buf2) != 0 {
		t.Fatalf("expected len == 0 after pool recycling, got %d", len(buf2))
	}
	PutBuffer(buf2)

	zeroCap := make([]byte, 0)
	PutBuffer(zeroCap)

	hugeBuf := make([]byte, 0, maxDecodeSize+1)
	PutBuffer(hugeBuf)
}

func TestDecodeArticleBuf_ParseTrailerError_PoolHandling(t *testing.T) {
	raw := []byte("hello world")
	article := yencEncode("test.bin", raw)
	article = bytes.Replace(article, []byte("=yend size=11"), []byte("=yend size=abc"), 1)

	// Clean the pool first
	for range 20 {
		_ = GetBuffer(100)
	}

	// Case 1: scratch is nil. It should return errMalformed and return the buffer to the pool.
	_, err := DecodeArticleBuf(article, nil)
	if !errors.Is(err, errMalformed) {
		t.Errorf("Case 1: expected errMalformed, got %v", err)
	}
	buf1 := GetBuffer(768 * 1024)
	if cap(buf1) < 768*1024 {
		t.Errorf("Case 1: expected pooled buffer to be returned, but got smaller buffer")
	}

	// Clean the pool again
	for range 20 {
		_ = GetBuffer(100)
	}

	// Case 2: scratch is non-nil and large enough. It should NOT return scratch to the pool.
	scratch := make([]byte, 0, 100)
	_, err = DecodeArticleBuf(article, scratch)
	if !errors.Is(err, errMalformed) {
		t.Errorf("Case 2: expected errMalformed, got %v", err)
	}
	for range 20 {
		bg := GetBuffer(100)
		if unsafe.SliceData(bg) == unsafe.SliceData(scratch) {
			t.Errorf("Case 2: scratch buffer was incorrectly put into the pool")
		}
	}

	// Clean the pool again
	for range 20 {
		_ = GetBuffer(100)
	}

	// Case 3: scratch is non-nil but too small. It should allocate a pooled buffer and return it on error.
	smallScratch := make([]byte, 0, 2)
	_, err = DecodeArticleBuf(article, smallScratch)
	if !errors.Is(err, errMalformed) {
		t.Errorf("Case 3: expected errMalformed, got %v", err)
	}
	buf3 := GetBuffer(768 * 1024)
	if cap(buf3) < 768*1024 {
		t.Errorf("Case 3: expected allocated pooled buffer to be returned to pool, got smaller buffer")
	}
}

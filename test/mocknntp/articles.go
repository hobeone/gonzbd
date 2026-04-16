package mocknntp

import (
	"bytes"
	"fmt"
	"hash/crc32"
)

// EncodeYEnc produces a yEnc-encoded article body for the given payload.
// filename appears in the =ybegin line; the result is a single-part article
// (no =ypart) suitable for use with Server.AddArticle.
//
// The encoding follows the yEnc spec: each input byte b is mapped to
// (b+42) mod 256, with special-case escaping for NUL, LF, CR, and '='.
// Lines are wrapped at 128 output characters; each line is CRLF-terminated.
func EncodeYEnc(filename string, payload []byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=ybegin line=128 size=%d name=%s\r\n", len(payload), filename)

	encoded := encodeYEncBytes(payload)
	writeWrappedLines(&buf, encoded, 128)

	checksum := crc32.ChecksumIEEE(payload)
	fmt.Fprintf(&buf, "=yend size=%d crc32=%08x\r\n", len(payload), checksum)
	return buf.Bytes()
}

// EncodeYEncPart produces a multi-part yEnc article. partNum is 1-based.
// totalParts and totalSize describe the full assembled file; offset is the
// 0-based byte offset of this part within the assembled file.
//
// The =ypart line uses 1-based begin/end values as required by the spec.
// The trailer uses pcrc32 (per-part CRC) as expected by internal/decoder's
// parseTrailer when isPart is true.
func EncodeYEncPart(filename string, partNum, totalParts int, totalSize, offset int64, partPayload []byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=ybegin part=%d total=%d line=128 size=%d name=%s\r\n",
		partNum, totalParts, totalSize, filename)

	// =ypart begin and end are 1-based; offset is 0-based so begin = offset+1.
	begin := offset + 1
	end := offset + int64(len(partPayload))
	fmt.Fprintf(&buf, "=ypart begin=%d end=%d\r\n", begin, end)

	encoded := encodeYEncBytes(partPayload)
	writeWrappedLines(&buf, encoded, 128)

	checksum := crc32.ChecksumIEEE(partPayload)
	fmt.Fprintf(&buf, "=yend size=%d part=%d pcrc32=%08x\r\n", len(partPayload), partNum, checksum)
	return buf.Bytes()
}

// encodeYEncBytes encodes raw payload bytes using the yEnc transformation:
// each byte b → (b+42) mod 256, with escaping for the four critical values
// that would otherwise corrupt framing:
//   - NUL (0x00)
//   - LF  (0x0a) — NNTP line delimiter
//   - CR  (0x0d) — NNTP line delimiter
//   - '=' (0x3d) — yEnc escape character
//
// An escaped byte is represented as '=' followed by (encoded+64) mod 256.
// The returned slice is not yet line-wrapped.
func encodeYEncBytes(raw []byte) []byte {
	out := make([]byte, 0, len(raw)+len(raw)/32)
	for _, b := range raw {
		enc := byte((int(b) + 42) % 256)
		switch enc {
		case 0, '\n', '\r', '=':
			// Escape: prepend '=' and offset the byte by 64.
			out = append(out, '=', byte((int(enc)+64)%256))
		default:
			out = append(out, enc)
		}
	}
	return out
}

// writeWrappedLines writes encoded yEnc bytes to buf in lines of at most
// lineWidth output characters, each terminated with CRLF. The lineWidth
// is measured in encoded bytes (after escaping), not raw bytes.
func writeWrappedLines(buf *bytes.Buffer, encoded []byte, lineWidth int) {
	for i := 0; i < len(encoded); i += lineWidth {
		end := min(i+lineWidth, len(encoded))
		buf.Write(encoded[i:end])
		buf.WriteString("\r\n")
	}
}

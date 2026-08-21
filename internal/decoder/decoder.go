// Package decoder implements yEnc and UU decoding for NNTP article bodies.
// It is called after the NNTP layer has removed dot-stuffing from the raw
// response, so the body passed in is clean encoded payload only.
//
// yEnc design notes
//
// The hot loop uses bytes.IndexByte to skip over runs of ordinary
// (non-escape, non-CR/LF) bytes in a single call, achieving >500 MB/s on
// realistic article data. The escape flag is never reset on CR or LF because
// the yEnc spec allows the '=' escape character to fall immediately before a
// CRLF line ending, which shifts the escaped byte to the start of the next
// line. Resetting escape state on newlines is the root cause of the 0xd6 bug
// present in several popular Go yEnc libraries.
package decoder

import (
	"bytes"
	"errors"
	"hash/crc32"
	"strconv"
	"sync"
	"unsafe"
)

// Sentinel errors returned by DecodeArticle.
var (
	// ErrNotYEnc is returned when the body does not begin with a =ybegin line.
	ErrNotYEnc = errors.New("decoder: not a yEnc article")

	// errMalformed is returned when the header or trailer is structurally invalid.
	errMalformed = errors.New("decoder: malformed yEnc article")

	// ErrCRCMismatch is returned when the decoded data's CRC32 disagrees with
	// the value declared in the =yend trailer. This error is retryable: the
	// downloader should try fetching from an alternate server before giving up.
	ErrCRCMismatch = errors.New("decoder: CRC mismatch")

	// errMissingTrailer is returned when no =yend line is found.
	errMissingTrailer = errors.New("decoder: missing =yend trailer")

	// errSizeMismatch is returned when the declared size in the trailer does
	// not equal the number of bytes decoded.
	errSizeMismatch = errors.New("decoder: declared size does not match decoded length")

	// ErrBodyTooLarge is returned when the input body exceeds maxDecodeSize.
	ErrBodyTooLarge = errors.New("decoder: body exceeds maximum decode size")
)

// maxDecodeSize is the maximum allowed input body size for decoding.
// Matches the NNTP layer's maxBodySize (10 MB). Direct callers that
// bypass the NNTP layer are still protected from OOM via this cap.
const maxDecodeSize = 10 * 1024 * 1024 // 10 MB

// maxPartOffset caps the `=ypart begin=` value a server may claim. The header
// is attacker-controlled and parsed as an unbounded int64, so without a cap a
// single hostile article can drive the assembler's WriteAt to an arbitrary
// offset. maxDecodeSize bounds the *payload*; this bounds the *offset*.
//
// 1 << 48 (256 TiB) is far above any legitimate Usenet file — a 50 GB posting
// has offsets around 5e10, six orders of magnitude below this — so it rejects
// MaxInt64-shaped garbage without second-guessing real file sizes. The
// authoritative bound is the assembler's per-file ExpectedSize check; this
// exists so malformed input fails at parse with a clear error instead of deep
// in the write path.
const maxPartOffset = 1 << 48

// yencHeader holds the fields parsed from a =ybegin / =ypart line pair.
type yencHeader struct {
	size   int64 // total file size (from =ybegin)
	offset int64 // byte offset of this part in the assembled file (0-based, from =ypart begin-1)
	name   string
	isPart bool
	part   int // part ordinal from =ybegin part=, 0 when absent
}

// yencTrailer holds the fields parsed from a =yend line.
type yencTrailer struct {
	size  int64
	crc   uint32
	valid bool // crc field was present
}

// Article is the result of decoding one yEnc-encoded NNTP article body.
//
// For single-part articles Offset is 0. For multi-part articles (=ypart
// header present) Offset describes the part's position; callers compose the
// full file by pwriting Data at Offset.
//
// TotalSize is the poster's DECLARED figure and is not an invariant — see its
// field comment. In particular it is not "len(Data) for a single-part
// article": that holds for a well-behaved poster and the parser has never
// checked it (#347).
type Article struct {
	// Filename is the yEnc name= field declared in =ybegin. May be empty
	// only on malformed articles, which DecodeArticle would reject earlier.
	Filename string

	// Offset is the byte position of this part within the assembled file.
	// Derived from the =ypart begin-1 field (yEnc uses 1-based indexing).
	Offset int64

	// TotalSize is the assembled file's size in bytes as DECLARED by the
	// poster in =ybegin size=. The same value is declared on every part of a
	// multi-part upload.
	//
	// It is unvalidated sender data. parseHeader keeps whatever
	// strconv.ParseInt accepts and drops the value silently otherwise, so a
	// single-part article can declare size=999999 for 46 bytes of payload, or
	// size=-5, or math.MaxInt64, and still decode with a nil error. The two
	// checks that DO run — the trailer's declared size against the decoded
	// length, and the CRC — constrain this PART, never the total.
	//
	// The one place this package uses the figure, decodeBody's pre-allocation
	// hint, caps it at min(len(encoded), maxDecodeSize) precisely because it
	// is attacker-controlled. Callers get no such treatment for free: do not
	// use it as a length — not to size a buffer, seek, or truncate a file.
	// Nothing outside this package reads it today. A caller that starts to
	// must bound it itself (#347).
	TotalSize int64

	// Data is the decoded part body. len(Data) is the size of this part.
	Data []byte

	// CRC is the CRC32 computed over Data. If the trailer's pcrc32/crc32
	// field was present, DecodeArticle has already verified it matches.
	CRC uint32

	// PartNumber is the 1-based ordinal the server declared in =ybegin part=,
	// or 0 when the article carries none — a single-part post, or a
	// non-numeric value.
	//
	// Nothing in this package acts on it. It exists so the dispatcher can
	// compare it against the NZB segment number that was requested and COUNT
	// the disagreements (#379). That comparison is novel: SABnzbd's decoder
	// reads part_begin and part_size without ever checking the served part
	// against the segment number, so there is no prior evidence about how
	// often servers and indexers agree. Counting first is what makes it safe
	// to consider acting later.
	PartNumber int
}

// DecodeArticle decodes a yEnc-encoded NNTP article body. body is the raw
// response body with dot-stuffing already removed by the NNTP layer.
//
// Returns ErrCRCMismatch if the trailer declares a CRC that disagrees with
// the decoded data, errSizeMismatch if the declared part size does not match
// the decoded length, and errMissingTrailer / errMalformed / ErrNotYEnc on
// structural problems.

var decodePool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 768*1024)
		return &b
	},
}

// GetBuffer retrieves a byte slice from the decoding pool with at least capacity capReq,
// up to maxDecodeSize. The returned slice has length 0.
func GetBuffer(capReq int64) []byte {
	maxCap := min(capReq, maxDecodeSize)
	if maxCap <= 0 {
		maxCap = 768 * 1024
	}
	bp, ok := decodePool.Get().(*[]byte)
	if !ok || bp == nil {
		return make([]byte, 0, maxCap)
	}
	buf := *bp
	if int64(cap(buf)) < maxCap {
		buf = make([]byte, 0, maxCap)
	} else {
		buf = buf[:0]
	}
	return buf
}

// PutBuffer returns a byte slice to the decoding pool.
// Slices with capacity 0 or greater than maxDecodeSize are discarded to prevent pool pollution.
func PutBuffer(buf []byte) {
	if cap(buf) == 0 || cap(buf) > maxDecodeSize {
		return
	}
	buf = buf[:0]
	decodePool.Put(&buf)
}

// DecodeArticle decodes a yEnc-encoded NNTP article body using DecodeArticleBuf with a nil scratch buffer.
func DecodeArticle(body []byte) (Article, error) {
	return DecodeArticleBuf(body, nil)
}

// DecodeArticleBuf decodes a yEnc-encoded NNTP article body using an optional
// scratch buffer for the decoded output to reduce heap allocations. If scratch
// is nil or has insufficient capacity, a buffer from the decoding pool is used.
func DecodeArticleBuf(body, scratch []byte) (Article, error) {
	// Guard against huge inputs from direct callers that bypass the
	// NNTP layer's maxBodySize limit.
	if len(body) > maxDecodeSize {
		return Article{}, ErrBodyTooLarge
	}

	hdr, bodyStart, err := parseHeader(body)
	if err != nil {
		return Article{}, err
	}

	// Locate the =yend trailer before decoding so we know exactly where the
	// encoded body ends. The yEnc spec requires =yend at the start of a line,
	// so search for "\n=yend" to avoid false matches from encoded body data
	// that happens to contain the byte sequence "=yend".
	trailerIdx := bytes.Index(body[bodyStart:], []byte("\n=yend"))
	//nolint:gocritic // ifElseChain: retained because trailerIdx is mutated across branches
	if trailerIdx >= 0 {
		trailerIdx += bodyStart + 1 // skip the \n, point at '='
	} else if bytes.HasPrefix(body[bodyStart:], []byte("=yend")) {
		// Edge case: empty encoded body — trailer starts immediately.
		trailerIdx = bodyStart
	} else {
		return Article{}, errMissingTrailer
	}

	encoded := body[bodyStart:trailerIdx]

	decoded, computedCRC := decodeBody(encoded, hdr.size, scratch)

	// Slice to just the =yend line — content after it (signatures,
	// empty lines, dot-stuffing leftovers) would corrupt parseKeyValues.
	trailerLine := body[trailerIdx:]
	if nl := bytes.IndexByte(trailerLine, '\n'); nl >= 0 {
		trailerLine = trailerLine[:nl]
	}
	// Also strip trailing \r if present.
	trailerLine = bytes.TrimRight(trailerLine, "\r")

	trailer, err := parseTrailer(trailerLine, hdr.isPart)
	if err != nil {
		//nolint:gosec // unsafe.SliceData is required to compare zero-length slice backing array pointers safely
		if scratch == nil || unsafe.SliceData(decoded) != unsafe.SliceData(scratch) {
			PutBuffer(decoded)
		}
		return Article{}, err
	}

	// Built before validation so the failure paths below can return the
	// article alongside their error.
	//
	// This used to say callers "especially PAR2 repair" can still use the
	// decoded data when it is corrupt. No caller does: decodePayload in
	// internal/downloader is the only caller of DecodeArticle, and it releases
	// the buffer to the pool on every error branch. Reading the old comment as
	// a contract — that invalid data survives for a repair step to consume —
	// would have been reading a use-after-free as a feature.
	//
	// Note also that the CRC check below is gated on trailer.valid, which is
	// false whenever the poster omitted pcrc32. Reaching the assembler
	// therefore does not imply a checksum was verified, only that none
	// disagreed.
	art := Article{
		Filename:   hdr.name,
		Offset:     hdr.offset,
		TotalSize:  hdr.size,
		Data:       decoded,
		CRC:        computedCRC,
		PartNumber: hdr.part,
	}

	if trailer.size != int64(len(decoded)) {
		return art, errSizeMismatch
	}

	if trailer.valid && computedCRC != trailer.crc {
		return art, ErrCRCMismatch
	}

	return art, nil
}

// decodeBody decodes the raw yEnc-encoded body bytes into their original form.
// sizeHint is used to pre-allocate the output buffer; 0 is safe.
//
// The fast path uses indexSpecial to find the next special byte ('=', '\r',
// '\n') and copies the intervening span in bulk via sub42Span, which fuses
// the copy and subtract-42 into a single pass using SWAR (SIMD Within A
// Register) uint64 arithmetic. Special bytes are handled individually.
// The escape flag persists across CR/LF so that a '=' split across a line
// boundary is handled correctly (the 0xd6 correctness rule).
func decodeBody(encoded []byte, sizeHint int64, scratch []byte) (out []byte, checksum uint32) {
	capacity := sizeHint
	// Cap the pre-allocation: the decoded output can never exceed
	// len(encoded), and never exceed maxDecodeSize. This prevents
	// attacker-controlled =ybegin size= fields from triggering OOM.
	maxCap := min(int64(len(encoded)), maxDecodeSize)
	if capacity <= 0 || capacity > maxCap {
		capacity = maxCap
	}
	if scratch != nil && int64(cap(scratch)) >= capacity {
		out = scratch[:0]
	} else {
		out = GetBuffer(capacity)
	}

	escaped := false
	remaining := encoded

	for len(remaining) > 0 {
		// Fast path: advance past any byte that is neither CR, LF, nor '='.
		if !escaped {
			next := indexSpecial(remaining)
			if next < 0 {
				// No special bytes remain; decode the entire tail at once.
				out = sub42Span(out, remaining)
				break
			}
			if next > 0 {
				// Bulk-decode the safe prefix.
				out = sub42Span(out, remaining[:next])
				remaining = remaining[next:]
			}
		}

		c := remaining[0]
		remaining = remaining[1:]

		switch {
		case c == '\r' || c == '\n':
			// Do NOT reset escaped: the '=' character is permitted to appear
			// immediately before CRLF, causing the escaped byte to be the
			// first character of the next line. Resetting here produces
			// silently corrupt output for raw bytes whose yEnc encoding maps
			// to 0x00 (byte value 0xd6 = 214, +42 mod 256 = 0, escaped as "= @").
		case escaped:
			// Subtract 106 = 64 (escape offset) + 42 (yEnc shift).
			out = append(out, c-106)
			escaped = false
		case c == '=':
			escaped = true
		default:
			out = append(out, c-42)
		}
	}

	checksum = crc32.ChecksumIEEE(out)
	return out, checksum
}

// sub42Span appends src to dst with each byte decremented by 42 (the yEnc
// shift). It fuses the copy and subtract into a single pass, avoiding the
// double memory traversal of append-then-loop where memmove copies the bytes
// and a second loop subtracts 42. Reading from src and writing the decoded
// value directly into dst keeps the working set in L1 cache.
func sub42Span(dst, src []byte) []byte {
	n := len(src)
	base := len(dst)

	// Ensure capacity for n more bytes, then extend the length.
	if cap(dst)-base < n {
		dst = append(dst[:base], make([]byte, n)...)[:base]
	}
	dst = dst[:base+n]
	out := dst[base:]

	// Fused copy+subtract: read from src, write decoded to out.
	// Unrolled 8x to help the compiler schedule loads and stores.
	i := 0
	for ; i+8 <= n; i += 8 {
		out[i] = src[i] - 42
		out[i+1] = src[i+1] - 42
		out[i+2] = src[i+2] - 42
		out[i+3] = src[i+3] - 42 //nolint:gosec // bounds guaranteed by length pre-check at function top
		out[i+4] = src[i+4] - 42 //nolint:gosec // bounds guaranteed by length pre-check at function top
		out[i+5] = src[i+5] - 42 //nolint:gosec // bounds guaranteed by length pre-check at function top
		out[i+6] = src[i+6] - 42 //nolint:gosec // bounds guaranteed by length pre-check at function top
		out[i+7] = src[i+7] - 42 //nolint:gosec // bounds guaranteed by length pre-check at function top
	}
	for ; i < n; i++ {
		out[i] = src[i] - 42
	}

	return dst
}

// specialLUT is a 256-byte lookup table: specialLUT[b] is true for bytes
// that require special handling in the yEnc decode loop ('\r', '\n', '=').
// Using a fixed table avoids the per-call asciiSet construction cost of
// bytes.IndexAny while maintaining single-pass O(n) scanning.
var specialLUT = [256]bool{
	'\r': true,
	'\n': true,
	'=':  true,
}

// indexSpecial returns the index of the first byte in b that is '\r', '\n',
// or '=', or -1 if none is found.
//
// Uses a single-pass scan with a pre-computed 256-byte lookup table.
// This avoids the two problems of multi-pass bytes.IndexByte:
//   - Scanning for an absent byte wastes a full-buffer SIMD pass.
//   - Any fixed scan order regresses on data patterns where the first
//     byte to search for is rare or missing.
//
// The lookup table is allocated once at init time (vs IndexAny which
// builds its asciiSet on every call), so per-byte cost is a single
// array bounds check + branch — approximately 1-2 cycles per byte on
// modern x86. For typical yEnc data where special bytes appear every
// ~80-200 bytes, this gives ~100-400 ns per call.
func indexSpecial(b []byte) int {
	for i, c := range b {
		if specialLUT[c] {
			return i
		}
	}
	return -1
}

// parseHeader parses the =ybegin line and the optional =ypart line.
// It returns the parsed header and the byte offset where the encoded body begins.
func parseHeader(body []byte) (yencHeader, int, error) {
	start := bytes.Index(body, []byte("=ybegin"))
	if start < 0 {
		return yencHeader{}, 0, ErrNotYEnc
	}
	body = body[start:]

	ybeginEnd := bytes.IndexByte(body, '\n')
	if ybeginEnd < 0 {
		return yencHeader{}, 0, errMalformed
	}

	hdr := yencHeader{}

	ybeginLine := body[:ybeginEnd]
	parseKeyValues(ybeginLine, func(k, v string) {
		switch k {
		case "size":
			n, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				hdr.size = n
			}
		case "name":
			hdr.name = v
		case "part":
			if v != "" {
				hdr.isPart = true
				// The VALUE, not just its presence. isPart drives which
				// checksum field parseTrailer treats as authoritative and is
				// unchanged; the ordinal is carried so the dispatcher can
				// compare what the server served against the segment number
				// the NZB asked for (#379). A non-numeric part= leaves it at
				// zero, which disables that comparison rather than failing the
				// article — nothing has ever validated this field, and a
				// decoder that starts rejecting on it would fail articles that
				// download correctly today.
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					hdr.part = n
				}
			}
		}
	})

	bodyStart := start + ybeginEnd + 1

	// Parse optional =ypart line.
	if bytes.HasPrefix(body[ybeginEnd+1:], []byte("=ypart")) {
		ypartEnd := bytes.IndexByte(body[ybeginEnd+1:], '\n')
		if ypartEnd < 0 {
			return yencHeader{}, 0, errMalformed
		}
		ypartLine := body[ybeginEnd+1 : ybeginEnd+1+ypartEnd]
		var beginVal int64
		var beginBad bool
		parseKeyValues(ypartLine, func(k, v string) {
			if k == "begin" {
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					// Non-numeric, or outside int64. Flag it rather than
					// leaving beginVal at zero, which would silently coerce
					// garbage to offset 0 and slip past the bounds check.
					beginBad = true
					return
				}
				beginVal = n
			}
		})
		// Reject implausible offsets here rather than letting them reach the
		// assembler's WriteAt (see maxPartOffset).
		//
		// begin=0 is deliberately tolerated. yEnc begin is 1-based, so a
		// poster emitting 0 most plausibly means "offset 0" — already the
		// result below. Rejecting it would buy spec-purity at the cost of
		// failing real articles.
		if beginBad || beginVal < 0 || beginVal > maxPartOffset {
			return yencHeader{}, 0, errMalformed
		}
		// =ypart begin= is 1-based; convert to 0-based offset.
		if beginVal > 0 {
			hdr.offset = beginVal - 1
		}
		hdr.isPart = true
		bodyStart += ypartEnd + 1
	}

	return hdr, bodyStart, nil
}

// parseTrailer parses the =yend line. isPart controls whether pcrc32 or
// crc32 is used as the authoritative checksum field.
func parseTrailer(line []byte, isPart bool) (yencTrailer, error) {
	if !bytes.HasPrefix(line, []byte("=yend")) {
		return yencTrailer{}, errMissingTrailer
	}

	trailer := yencTrailer{}
	hasSize := false
	var parseErr error
	parseKeyValues(line, func(k, v string) {
		switch k {
		case "size":
			n, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				trailer.size = n
				hasSize = true
			} else {
				parseErr = errMalformed
			}
		case "crc32":
			if !isPart {
				n, err := strconv.ParseUint(v, 16, 32)
				if err == nil {
					trailer.crc = uint32(n)
					trailer.valid = true
				} else {
					parseErr = errMalformed
				}
			}
		case "pcrc32":
			if isPart {
				n, err := strconv.ParseUint(v, 16, 32)
				if err == nil {
					trailer.crc = uint32(n)
					trailer.valid = true
				} else {
					parseErr = errMalformed
				}
			}
		}
	})

	if parseErr != nil {
		return yencTrailer{}, parseErr
	}
	if !hasSize {
		return yencTrailer{}, errMalformed
	}

	return trailer, nil
}

// parseKeyValues calls fn for each key=value token in a yEnc header or
// trailer line. Tokens are space-separated; the "name" field may contain
// spaces and is treated as extending to the end of the line.
func parseKeyValues(line []byte, fn func(k, v string)) {
	// Strip the leading directive (=ybegin, =ypart, =yend).
	_, after, ok := bytes.Cut(line, []byte{' '})
	if !ok {
		return
	}
	rest := after

	for len(rest) > 0 {
		// Consume leading spaces.
		for len(rest) > 0 && rest[0] == ' ' {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			break
		}

		// Find '='.
		eq := bytes.IndexByte(rest, '=')
		if eq < 0 {
			break
		}
		key := string(bytes.TrimRight(rest[:eq], " "))
		rest = rest[eq+1:]

		// The "name" field runs to end-of-line (may contain spaces).
		if key == "name" {
			// Strip trailing whitespace and null bytes per sabctools commit bc24612.
			val := bytes.TrimRight(rest, " \t\r\n\x00")
			fn(key, latin1ToUTF8(val))
			break
		}

		// Other fields run to the next space.
		nextSp := bytes.IndexByte(rest, ' ')
		var val []byte
		if nextSp < 0 {
			val = bytes.TrimRight(rest, "\r\n")
			rest = nil
		} else {
			val = rest[:nextSp]
			rest = rest[nextSp+1:]
		}
		fn(key, string(val))
	}
}

// latin1ToUTF8 converts a Latin1 (ISO-8859-1) byte slice to a UTF-8 string.
// It pre-checks for pure ASCII to avoid allocation when no conversion is needed.
func latin1ToUTF8(b []byte) string {
	isASCII := true
	for _, c := range b {
		if c >= 128 {
			isASCII = false
			break
		}
	}
	if isASCII {
		return string(b)
	}

	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		if c < 128 {
			out = append(out, c)
		} else {
			out = append(out, 0xC0|(c>>6), 0x80|(c&0x3F))
		}
	}
	return string(out)
}

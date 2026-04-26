package decoder

import (
	"bytes"
	"errors"
	"strings"
)

// Sentinel errors returned by DecodeUU.
var (
	// ErrNotUU is returned when the body does not start with a valid "begin" line.
	ErrNotUU = errors.New("decoder: not a UU-encoded article")

	// ErrUUMalformed is returned when a UU body line cannot be decoded.
	ErrUUMalformed = errors.New("decoder: malformed UU article")
)

// DecodeUU decodes a UU-encoded article body.
//
// UU encoding is a legacy format occasionally seen on older Usenet servers.
// Each encoded line begins with a length character (raw value + 0x20); the
// body characters each encode 6 bits of payload with an offset of 0x20.
// Three payload bytes are encoded as four UU characters. A line of length
// zero (encoded as '`' or ' ') signals the end of the payload.
//
// body must begin with "begin <mode> <filename>\n" and end with an "end" line.
// DecodeUU does not wire into DecodeArticle; the caller decides which format
// to try (typically by checking whether DecodeArticle returns ErrNotYEnc).
func DecodeUU(body []byte) (data []byte, filename string, err error) {
	line, rest, ok := cutLine(body)
	if !ok {
		return nil, "", ErrNotUU
	}

	// Parse "begin <mode> <filename>"
	fields := strings.Fields(string(line))
	if len(fields) < 3 || fields[0] != "begin" {
		return nil, "", ErrNotUU
	}
	filename = fields[2]

	out := make([]byte, 0, len(body)/4*3)

	for {
		line, rest, ok = cutLine(rest)
		if !ok {
			// Ran out of input without an "end" line; return what we have.
			break
		}

		// Trim trailing CR if present.
		line = bytes.TrimRight(line, "\r")

		if bytes.Equal(line, []byte("end")) {
			break
		}

		if len(line) == 0 {
			continue
		}

		// The first character encodes the number of payload bytes on this line.
		// '`' (0x60) is a common zero-length sentinel; ' ' (0x20) is the spec value.
		rawLen := int((line[0] - 0x20) & 0x3f)
		if rawLen == 0 {
			break
		}
		encoded := line[1:]

		out = append(out, decodeUULine(encoded, rawLen)...)
	}

	return out, filename, nil
}

// decodeUULine decodes one line of UU-encoded data, returning exactly rawLen bytes.
// Each group of 4 characters encodes 3 bytes; the offset is 0x20.
func decodeUULine(enc []byte, rawLen int) []byte {
	out := make([]byte, 0, rawLen)

	needed := ((rawLen + 2) / 3) * 4
	if len(enc) < needed {
		padded := make([]byte, needed)
		copy(padded, enc)
		for i := len(enc); i < needed; i++ {
			padded[i] = ' '
		}
		enc = padded
	}

	for i := 0; len(out) < rawLen; i += 4 {
		a := (enc[i] - 0x20) & 0x3f
		b := (enc[i+1] - 0x20) & 0x3f
		c := (enc[i+2] - 0x20) & 0x3f
		d := (enc[i+3] - 0x20) & 0x3f

		out = append(out, (a<<2)|(b>>4))
		if len(out) < rawLen {
			out = append(out, (b<<4)|(c>>2))
		}
		if len(out) < rawLen {
			out = append(out, (c<<6)|d)
		}
	}

	return out[:rawLen]
}

// cutLine returns the content before the first '\n', the remainder after it,
// and true. If no '\n' is found it returns the full slice, nil, false.
func cutLine(b []byte) (line, rest []byte, ok bool) {
	idx := bytes.IndexByte(b, '\n')
	if idx < 0 {
		return b, nil, false
	}
	return b[:idx], b[idx+1:], true
}

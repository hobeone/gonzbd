package nntp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// readResponseLine reads exactly one CRLF-terminated status line from
// br. Returns the line with CRLF (or bare LF) stripped. An unexpected
// EOF mid-line is returned as io.ErrUnexpectedEOF to make short-read
// diagnostics clearer.
func readResponseLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line == "" {
			return "", io.EOF
		}
		if errors.Is(err, io.EOF) {
			return "", io.ErrUnexpectedEOF
		}
		return "", err
	}
	line = trimCRLF(line)
	return line, nil
}

// parseStatus splits "NNN text..." into the numeric code and remaining
// text. The NNTP grammar mandates a single space between code and
// text, but some servers emit just the code with no trailing text on
// short responses (e.g. "200\r\n" — seen in the wild); handle that
// case too.
func parseStatus(line string) (code int, text string, err error) {
	if len(line) < 3 {
		return 0, "", fmt.Errorf("nntp: short response %q", line)
	}
	code, err = strconv.Atoi(line[:3])
	if err != nil {
		return 0, "", fmt.Errorf("nntp: non-numeric status %q", line)
	}
	if len(line) == 3 {
		return code, "", nil
	}
	if line[3] != ' ' {
		return 0, "", fmt.Errorf("nntp: malformed response %q", line)
	}
	return code, line[4:], nil
}

// MaxBodySize is the maximum size of an un-dotstuffed NNTP body we
// are willing to buffer in memory (10 MB). Usenet articles are
// typically under 800 KB but this leaves generous headroom.
const MaxBodySize = 10 * 1024 * 1024

// readDotStuffedBody reads a multi-line response body from br per RFC
// 3977 §3.1.1. The body ends at a line containing only ".". Leading
// "." characters on other lines are dot-stuffed and must be removed
// (first byte dropped). CRLF on the wire is normalised to LF in the
// output.
//
// Implementation: ReadSlice borrows the bufio reader's internal buffer
// rather than allocating a fresh slice per line, so the only heap
// traffic per call is the growing output buffer (~log2(body size)
// allocations from bytes.Buffer, not one per line).
func readDotStuffedBody(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				return nil, fmt.Errorf("nntp: body line exceeds %d bytes", br.Size())
			}
			return nil, err
		}
		// Strip trailing LF (always present) and optional CR.
		end := len(line) - 1
		if end > 0 && line[end-1] == '\r' {
			end--
		}
		body := line[:end]

		// Terminator is a line whose content is exactly ".".
		if end == 1 && body[0] == '.' {
			return buf.Bytes(), nil
		}
		// Un-dotstuff: RFC 3977 §3.1.1 requires any line starting
		// with "." to be prefixed with an extra "." for transport;
		// strip that extra dot on receipt.
		if end > 0 && body[0] == '.' {
			body = body[1:]
		}
		if buf.Len()+len(body)+1 > MaxBodySize {
			return nil, fmt.Errorf("nntp: body exceeds %d bytes", MaxBodySize)
		}
		buf.Write(body)
		buf.WriteByte('\n')
	}
}

func trimCRLF(s string) string {
	n := len(s)
	if n >= 2 && s[n-2] == '\r' && s[n-1] == '\n' {
		return s[:n-2]
	}
	if n >= 1 && s[n-1] == '\n' {
		return s[:n-1]
	}
	return s
}

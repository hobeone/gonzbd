package nntp

import (
	"bufio"
	"bytes"
	"fmt"
	"testing"
)

// BenchmarkParseStatus measures the pure-function cost of splitting an NNTP
// status line into numeric code and text.
func BenchmarkParseStatus(b *testing.B) {
	line := "222 0 <abc@host> body follows"
	b.SetBytes(int64(len(line)))
	for b.Loop() {
		code, text, err := parseStatus(line)
		if err != nil {
			b.Fatal(err)
		}
		if code != 222 || text == "" {
			b.Fatalf("unexpected result: code=%d text=%q", code, text)
		}
	}
}

// buildStatusLines constructs n CRLF-terminated status lines concatenated
// into a single byte slice.
func buildStatusLines(n int) []byte {
	var buf bytes.Buffer
	for i := range n {
		fmt.Fprintf(&buf, "200 Welcome to news server %d\r\n", i)
	}
	return buf.Bytes()
}

const linesPerInput = 500

// BenchmarkReadResponseLine benchmarks reading one CRLF-terminated status line
// at a time from a bufio.Reader. The underlying byte slice is re-wrapped into a
// new reader each time the input is exhausted.
func BenchmarkReadResponseLine(b *testing.B) {
	bulk := buildStatusLines(linesPerInput)
	b.SetBytes(int64(len(bulk) / linesPerInput)) // bytes per line

	br := bufio.NewReader(bytes.NewReader(bulk))
	linesRead := 0
	for b.Loop() {
		line, err := readResponseLine(br)
		if err != nil {
			// Exhausted the buffer — re-wrap and continue.
			br = bufio.NewReader(bytes.NewReader(bulk))
			linesRead = 0
			line, err = readResponseLine(br)
			if err != nil {
				b.Fatal(err)
			}
		}
		linesRead++
		if line == "" {
			b.Fatal("empty line")
		}
	}
}

// buildDotStuffedBody returns a dot-stuffed body of approximately targetSize
// bytes (after un-stuffing). Each line is 76 bytes of content followed by
// CRLF. Lines that would start with '.' have an extra '.' prepended.
func buildDotStuffedBody(targetSize int) []byte {
	const lineContent = 76 // bytes of content per line
	var buf bytes.Buffer
	written := 0
	lineNum := 0
	for written < targetSize {
		remaining := targetSize - written
		n := min(lineContent, remaining)
		// Every 7th line starts with '.' to exercise dot-unstuffing.
		if lineNum%7 == 0 {
			buf.WriteByte('.') // extra dot (stuffing)
			buf.WriteByte('.') // actual content dot
			for k := 2; k < n; k++ {
				buf.WriteByte(byte('a' + k%26))
			}
		} else {
			for k := range n {
				buf.WriteByte(byte('a' + k%26))
			}
		}
		buf.WriteString("\r\n")
		written += n
		lineNum++
	}
	// Terminator line.
	buf.WriteString(".\r\n")
	return buf.Bytes()
}

// BenchmarkReadDotStuffedBody_Small benchmarks parsing a 4 KB dot-stuffed body
// — the smallest realistic Usenet article.
func BenchmarkReadDotStuffedBody_Small(b *testing.B) {
	const bodySize = 4 * 1024
	input := buildDotStuffedBody(bodySize)
	b.SetBytes(int64(bodySize))
	for b.Loop() {
		br := bufio.NewReader(bytes.NewReader(input))
		data, err := readDotStuffedBody(br)
		if err != nil {
			b.Fatal(err)
		}
		if len(data) == 0 {
			b.Fatal("empty result")
		}
	}
}

// BenchmarkReadDotStuffedBody_Large benchmarks parsing an 800 KB dot-stuffed
// body — the upper end of a typical Usenet article.
func BenchmarkReadDotStuffedBody_Large(b *testing.B) {
	const bodySize = 800 * 1024
	input := buildDotStuffedBody(bodySize)
	b.SetBytes(int64(bodySize))
	for b.Loop() {
		br := bufio.NewReader(bytes.NewReader(input))
		data, err := readDotStuffedBody(br)
		if err != nil {
			b.Fatal(err)
		}
		if len(data) == 0 {
			b.Fatal("empty result")
		}
	}
}

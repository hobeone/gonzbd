// Package cmdutil provides helpers for running external commands with
// real-time output streaming.
package cmdutil

import (
	"bytes"
	"strings"
	"sync"
)

// LineStreamer is an [io.Writer] that buffers incoming bytes and calls OnLine
// for each complete line (delimited by \n, with \r stripped). It also
// accumulates the full output so callers can parse it after the process exits.
//
// Thread-safe: concurrent writes from stdout and stderr are serialized.
type LineStreamer struct {
	mu     sync.Mutex
	buf    bytes.Buffer // partial line accumulator
	full   bytes.Buffer // complete output (for post-exit String())
	onLine func(string) // called for each complete line; may be nil
	cap    int          // max total bytes in full (0 = unlimited)
	capped bool         // true once cap is reached
}

// NewLineStreamer constructs a LineStreamer that calls onLine for each complete
// line of output. If onLine is nil, the streamer silently accumulates output.
func NewLineStreamer(onLine func(string)) *LineStreamer {
	return &LineStreamer{onLine: onLine}
}

// NewLineStreamerCapped constructs a LineStreamer with a maximum total byte
// limit. Once exceeded, further writes are silently dropped (but onLine
// callbacks continue as long as the partial-line buffer has room).
func NewLineStreamerCapped(onLine func(string), maxBytes int) *LineStreamer {
	return &LineStreamer{onLine: onLine, cap: maxBytes}
}

// Write implements [io.Writer]. It splits p on newlines, calling onLine for
// each complete line, and buffers any trailing partial line.
func (s *LineStreamer) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n = len(p)

	// Accumulate full output (for post-exit parsing).
	if s.cap > 0 && !s.capped {
		remaining := s.cap - s.full.Len()
		switch {
		case remaining <= 0:
			s.capped = true
		case len(p) > remaining:
			s.full.Write(p[:remaining])
			s.capped = true
		default:
			s.full.Write(p)
		}
	} else if s.cap == 0 {
		s.full.Write(p)
	}

	// Split on newlines and dispatch complete lines.
	s.buf.Write(p)
	for {
		line, err := s.buf.ReadString('\n')
		if err != nil {
			// No newline found — put the partial line back.
			s.buf.WriteString(line)
			break
		}
		// Strip \r\n → clean line.
		line = strings.TrimRight(line, "\r\n")
		if s.onLine != nil {
			s.onLine(line)
		}
	}

	return n, nil
}

// Flush emits any remaining partial line (no trailing newline). Call this
// after the subprocess exits to capture the last line if it wasn't terminated
// with a newline.
func (s *LineStreamer) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() > 0 {
		line := strings.TrimRight(s.buf.String(), "\r\n")
		s.buf.Reset()
		if line != "" && s.onLine != nil {
			s.onLine(line)
		}
	}
}

// String returns the full accumulated output (for post-exit parsing).
func (s *LineStreamer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.full.String()
}

// Bytes returns the full accumulated output as a byte slice.
func (s *LineStreamer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.full.Bytes()
}

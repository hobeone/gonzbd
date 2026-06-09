package nntp

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// ---------- parseStatus ----------

func TestParseStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		wantCode int
		wantText string
		wantErr  bool
	}{
		{name: "standard 200 OK", input: "200 OK", wantCode: 200, wantText: "OK"},
		{name: "code only", input: "200", wantCode: 200, wantText: ""},
		{name: "code with longer text", input: "222 0 <abc@host>", wantCode: 222, wantText: "0 <abc@host>"},
		{name: "430 no article", input: "430 no such article", wantCode: 430, wantText: "no such article"},
		{name: "481 auth rejected", input: "481 bad credentials", wantCode: 481, wantText: "bad credentials"},
		{name: "empty text after space", input: "200 ", wantCode: 200, wantText: ""},
		{name: "too short - 2 chars", input: "20", wantErr: true},
		{name: "empty string", input: "", wantErr: true},
		{name: "non-numeric", input: "2X0 OK", wantErr: true},
		{name: "no space separator", input: "200OK", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, text, err := parseStatus(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseStatus(%q) expected error, got code=%d text=%q", tt.input, code, text)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseStatus(%q) unexpected error: %v", tt.input, err)
			}
			if code != tt.wantCode {
				t.Errorf("code = %d, want %d", code, tt.wantCode)
			}
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
		})
	}
}

// ---------- trimCRLF ----------

func TestTrimCRLF(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"foo\r\n", "foo"},
		{"foo\n", "foo"},
		{"foo", "foo"},
		{"", ""},
		{"\r\n", ""},
		{"\n", ""},
		{"foo\r", "foo\r"},                 // bare CR is not stripped
		{"multi\r\nline", "multi\r\nline"}, // only terminal CRLF is stripped
	}
	for _, tt := range tests {
		got := trimCRLF(tt.input)
		if got != tt.want {
			t.Errorf("trimCRLF(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---------- readResponseLine ----------

func TestReadResponseLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		input       string
		want        string
		wantErr     bool
		wantErrType error
	}{
		{name: "CRLF terminated", input: "200 OK\r\n", want: "200 OK"},
		{name: "LF terminated", input: "200 OK\n", want: "200 OK"},
		{name: "bare LF with body", input: "222 0 <id@host> body\n", want: "222 0 <id@host> body"},
		{name: "empty input", input: "", wantErr: true, wantErrType: io.EOF},
		{name: "unexpected EOF", input: "200 OK", wantErr: true, wantErrType: io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			br := bufio.NewReader(strings.NewReader(tt.input))
			got, err := readResponseLine(br)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				if tt.wantErrType != nil && !errors.Is(err, tt.wantErrType) {
					t.Errorf("got error %v, want %v", err, tt.wantErrType)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------- idleTimeoutReader ----------

func TestIdleTimeoutReader_ResetsDeadline(t *testing.T) {
	t.Parallel()

	// Use a pipe to control data flow. The idleTimeoutReader should
	// reset the read deadline before each Read, so reads that arrive
	// within the timeout window succeed even if the total time exceeds
	// the timeout.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	timeout := 200 * time.Millisecond
	itr := &idleTimeoutReader{nc: client, timeout: timeout}

	// Write data from the "server" side.
	go func() {
		// First chunk immediately.
		server.Write([]byte("hello"))
		// Second chunk after a short delay (within timeout).
		time.Sleep(50 * time.Millisecond)
		server.Write([]byte("world"))
		// Let the connection idle past the timeout.
		time.Sleep(300 * time.Millisecond)
		server.Write([]byte("late")) // should be unreachable — deadline fired
	}()

	buf := make([]byte, 5)

	// First read: should succeed.
	n, err := itr.Read(buf)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Errorf("first read: got %q, want %q", buf[:n], "hello")
	}

	// Second read: should succeed (within timeout window).
	n, err = itr.Read(buf)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if string(buf[:n]) != "world" {
		t.Errorf("second read: got %q, want %q", buf[:n], "world")
	}

	// Third read: should fail with timeout (idle too long).
	_, err = itr.Read(buf)
	if err == nil {
		t.Error("expected timeout error on third read, got nil")
	}
}

func TestReadResponseLine_Boundaries(t *testing.T) {
	t.Parallel()

	// maxResponseLineLen is 2048
	t.Run("exact max len", func(t *testing.T) {
		input := strings.Repeat("A", 2047) + "\n"
		br := bufio.NewReader(strings.NewReader(input))
		got, err := readResponseLine(br)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2047 {
			t.Errorf("got line len %d, want 2047", len(got))
		}
	})

	t.Run("exceeds max len", func(t *testing.T) {
		input := strings.Repeat("A", 2048) + "\n"
		br := bufio.NewReader(strings.NewReader(input))
		_, err := readResponseLine(br)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("expected exceeds error, got: %v", err)
		}
	})

	t.Run("exact max len split buffer", func(t *testing.T) {
		input := strings.Repeat("A", 2047) + "\n"
		br := bufio.NewReaderSize(strings.NewReader(input), 16)
		got, err := readResponseLine(br)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2047 {
			t.Errorf("got line len %d, want 2047", len(got))
		}
	})

	t.Run("exceeds max len split buffer", func(t *testing.T) {
		input := strings.Repeat("A", 2048) + "\n"
		br := bufio.NewReaderSize(strings.NewReader(input), 16)
		_, err := readResponseLine(br)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestReadResponseLine_OversizedDiscard(t *testing.T) {
	t.Parallel()
	line1 := strings.Repeat("A", 2049) + "\n"
	line2 := "OK\n"
	br := bufio.NewReader(strings.NewReader(line1 + line2))

	_, err := readResponseLine(br)
	if err == nil {
		t.Fatal("expected error for oversized line")
	}

	got, err := readResponseLine(br)
	if err != nil {
		t.Fatalf("unexpected error reading next line: %v", err)
	}
	if got != "OK" {
		t.Errorf("got %q, want %q", got, "OK")
	}
}

func TestReadDotStuffedBody_GrowAndEmptyLine(t *testing.T) {
	t.Parallel()

	// Grow pre-size check
	input := "line1\n\nline2\n.\n"
	br := bufio.NewReader(strings.NewReader(input))
	res, err := readDotStuffedBody(br)
	if err != nil {
		t.Fatalf("readDotStuffedBody: %v", err)
	}
	if cap(res) < 768*1024 {
		t.Errorf("expected buffer to grow to at least 768KB, got capacity %d", cap(res))
	}

	// Empty line and dots checks
	expected := "line1\n\nline2\n"
	if string(res) != expected {
		t.Errorf("got %q, want %q", string(res), expected)
	}
}

func TestReadDotStuffedBody_MaxSize(t *testing.T) {
	t.Parallel()

	// maxBodySize is 10MB = 10 * 1024 * 1024 = 10485760 bytes
	t.Run("exact max body size", func(t *testing.T) {
		var sb strings.Builder
		chunk := strings.Repeat("A", 999) + "\n"
		for i := 0; i < 10485; i++ {
			sb.WriteString(chunk)
		}
		sb.WriteString(strings.Repeat("A", 759) + "\n")
		sb.WriteString(".\n")
		br := bufio.NewReader(strings.NewReader(sb.String()))
		res, err := readDotStuffedBody(br)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(res) != 10*1024*1024 {
			t.Errorf("got len %d, want %d", len(res), 10*1024*1024)
		}
	})

	t.Run("exceeds max body size", func(t *testing.T) {
		var sb strings.Builder
		chunk := strings.Repeat("A", 999) + "\n"
		for i := 0; i < 10485; i++ {
			sb.WriteString(chunk)
		}
		sb.WriteString(strings.Repeat("A", 760) + "\n")
		sb.WriteString(".\n")
		br := bufio.NewReader(strings.NewReader(sb.String()))
		_, err := readDotStuffedBody(br)
		if err == nil {
			t.Fatal("expected error for exceeding maxBodySize, got nil")
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("expected exceeds error, got: %v", err)
		}
	})
}

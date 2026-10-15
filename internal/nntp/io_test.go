package nntp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
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
		{name: "signed negative status code", input: "-01 OK", wantErr: true},
		{name: "signed positive status code", input: "+01 OK", wantErr: true},
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

type failingDeadlineConn struct {
	net.Conn
}

func (f *failingDeadlineConn) SetReadDeadline(_ time.Time) error {
	return errors.New("synthetic SetReadDeadline failure")
}

func TestIdleTimeoutReader_GatedOnInFlight(t *testing.T) {
	t.Parallel()

	// Nil receiver calls are safe no-ops.
	var nilITR *idleTimeoutReader
	nilITR.beginCmd()
	nilITR.endCmd()

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	const timeout = 60 * time.Millisecond
	itr := &idleTimeoutReader{nc: client, timeout: timeout, log: logger}

	// 0. Before endHandshake (gateOnInFlight == false), calling beginCmd +
	// endCmd while a Read is parked must NOT disarm the handshake deadline.
	buf := make([]byte, 2)
	hsDone := make(chan error, 1)
	go func() {
		_, rerr := itr.Read(buf)
		hsDone <- rerr
	}()
	time.Sleep(15 * time.Millisecond)
	itr.beginCmd()
	itr.endCmd()
	select {
	case rerr := <-hsDone:
		if !errors.Is(rerr, os.ErrDeadlineExceeded) {
			t.Fatalf("handshake Read after endCmd err = %v, want os.ErrDeadlineExceeded", rerr)
		}
	case <-time.After(time.Second):
		t.Fatal("handshake Read did not time out: endCmd disarmed deadline before endHandshake")
	}

	itr.endHandshake()

	// 1. With inFlight == 0 after endHandshake, Read disarms the deadline and
	// survives a pause longer than 2x timeout before bytes arrive.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = server.Write([]byte("ok"))
	}()
	n, err := itr.Read(buf)
	if err != nil {
		t.Fatalf("idle Read after endHandshake failed: %v", err)
	}
	if string(buf[:n]) != "ok" {
		t.Fatalf("idle Read got %q, want %q", buf[:n], "ok")
	}

	// 2. Calling endCmd with inFlight == 0 records an underflow (log + counter)
	// and does not let inFlight go negative, so a subsequent beginCmd still
	// transitions 0 -> 1 and arms the read deadline on a parked Read.
	itr.endCmd()
	if got := itr.underflows.Load(); got != 1 {
		t.Fatalf("underflows = %d, want 1", got)
	}
	if !strings.Contains(logBuf.String(), "endCmd called with no commands in flight") {
		t.Fatalf("expected underflow error log, got %q", logBuf.String())
	}

	done := make(chan error, 1)
	go func() {
		_, rerr := itr.Read(buf)
		done <- rerr
	}()
	time.Sleep(20 * time.Millisecond)
	itr.beginCmd()
	select {
	case rerr := <-done:
		if !errors.Is(rerr, os.ErrDeadlineExceeded) {
			t.Fatalf("parked Read after beginCmd err = %v, want os.ErrDeadlineExceeded", rerr)
		}
	case <-time.After(time.Second):
		t.Fatal("parked Read did not time out after beginCmd armed the deadline")
	}

	// 3. While a Read is already parked with an armed deadline (inFlight == 1),
	// calling endCmd disarms the socket deadline on that parked Read so it stays
	// blocked past 2x timeout and succeeds when bytes arrive later.
	parkedAfterBegin := make(chan error, 1)
	go func() {
		_, rerr := itr.Read(buf)
		parkedAfterBegin <- rerr
	}()
	time.Sleep(15 * time.Millisecond)
	itr.endCmd()
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = server.Write([]byte("hi"))
	}()
	select {
	case rerr := <-parkedAfterBegin:
		if rerr != nil {
			t.Fatalf("parked Read after endCmd failed (endCmd did not disarm deadline): %v", rerr)
		}
		if string(buf) != "hi" {
			t.Fatalf("parked Read after endCmd got %q, want %q", buf, "hi")
		}
	case <-time.After(time.Second):
		t.Fatal("parked Read after endCmd did not complete")
	}

	// 4. Non-ErrClosed SetReadDeadline errors are logged at Warn by logDeadlineErr.
	logBuf.Reset()
	errITR := &idleTimeoutReader{
		nc:      &failingDeadlineConn{Conn: client},
		timeout: timeout,
		log:     logger,
	}
	dlErr := errITR.setReadDeadline(time.Now())
	if dlErr == nil {
		t.Fatal("expected setReadDeadline error from failingDeadlineConn")
	}
	errITR.logDeadlineErr(time.Now(), dlErr)
	if !strings.Contains(logBuf.String(), "nntp: failed to set socket read deadline") {
		t.Fatalf("expected Warn log on SetReadDeadline error, got %q", logBuf.String())
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
		for range 10485 {
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
		for range 10485 {
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

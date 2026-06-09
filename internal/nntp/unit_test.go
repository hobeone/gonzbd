package nntp

import (
	"bufio"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

// ---------- classifyStatus ----------

func TestClassifyStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code int
		want error
	}{
		{430, ErrNoArticle},
		{423, ErrNoArticle},
		{480, ErrAuthRequired},
		{481, ErrAuthRejected},
		{482, ErrAuthRejected},
		{502, ErrServerUnavailable},
		{503, ErrServerUnavailable},
		{400, ErrTransient},
		{450, ErrTransient},
		{500, ErrTransient},
		{599, ErrTransient},
		{600, nil},
		{200, nil},
		{222, nil},
		{100, nil},
	}
	for _, tt := range tests {
		got := classifyStatus(tt.code)
		if !errors.Is(got, tt.want) {
			t.Errorf("classifyStatus(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
}

// ---------- ServerError ----------

func TestServerError_Error(t *testing.T) {
	t.Parallel()
	se := &ServerError{Code: 430, Text: "No such article"}
	got := se.Error()
	want := "nntp: server responded 430 No such article"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestServerError_Unwrap(t *testing.T) {
	t.Parallel()
	se := &ServerError{Code: 480, Text: "Auth required"}
	if !errors.Is(se, ErrAuthRequired) {
		t.Error("errors.Is(ServerError{480}, ErrAuthRequired) = false")
	}
	se2 := &ServerError{Code: 502, Text: "Service unavailable"}
	if !errors.Is(se2, ErrServerUnavailable) {
		t.Error("errors.Is(ServerError{502}, ErrServerUnavailable) = false")
	}
}

// ---------- parseCapabilities ----------

func TestParseCapabilities_Reader(t *testing.T) {
	t.Parallel()
	caps := parseCapabilities("VERSION 2\nREADER\nPOST\n")
	if !caps.HasBody {
		t.Error("HasBody should be true (READER implies BODY)")
	}
	if !caps.HasStat {
		t.Error("HasStat should be true (READER implies STAT)")
	}
	if caps.HasCompress {
		t.Error("HasCompress should be false")
	}
	if !caps.HasPost {
		t.Error("HasPost should be true")
	}
	if caps.Version != 2 {
		t.Errorf("Version = %d, want 2", caps.Version)
	}
	if len(caps.Raw) != 3 {
		t.Errorf("Raw has %d lines, want 3", len(caps.Raw))
	}
}

func TestParseCapabilities_ExplicitVerbs(t *testing.T) {
	t.Parallel()
	caps := parseCapabilities("BODY\nSTAT\nXFEATURE-COMPRESS\n")
	if !caps.HasBody {
		t.Error("HasBody should be true")
	}
	if !caps.HasStat {
		t.Error("HasStat should be true")
	}
	if !caps.HasCompress {
		t.Error("HasCompress should be true")
	}
}

func TestParseCapabilities_Empty(t *testing.T) {
	t.Parallel()
	caps := parseCapabilities("")
	// Empty CAPABILITIES response: server responded but advertised nothing.
	// HasBody and HasStat should be false — the server genuinely doesn't
	// support them via explicit advertisement.
	if caps.HasBody {
		t.Error("HasBody should be false for empty response")
	}
	if caps.HasStat {
		t.Error("HasStat should be false for empty response")
	}
}

func TestParseCapabilities_OverHDR(t *testing.T) {
	t.Parallel()
	caps := parseCapabilities("VERSION 2\nREADER\nOVER\nHDR\n")
	if !caps.HasOver {
		t.Error("HasOver should be true")
	}
	if !caps.HasHDR {
		t.Error("HasHDR should be true")
	}
	if !caps.HasBody {
		t.Error("HasBody should be true (via READER)")
	}
}

func TestParseCapabilities_IHave(t *testing.T) {
	t.Parallel()
	caps := parseCapabilities("VERSION 2\nIHAVE\n")
	if !caps.HasIHave {
		t.Error("HasIHave should be true")
	}
	if caps.HasBody {
		t.Error("HasBody should be false (no READER or BODY)")
	}
}

func TestParseCapabilities_RealisticServer(t *testing.T) {
	t.Parallel()
	// Realistic Usenet server response.
	body := "VERSION 2\r\nREADER\r\nOVER\r\nHDR\r\nPOST\r\nLIST ACTIVE NEWSGROUPS OVERVIEW.FMT\r\nXFEATURE-COMPRESS GZIP TERMINATE\r\n"
	caps := parseCapabilities(body)
	if !caps.HasBody || !caps.HasStat {
		t.Error("READER should imply HasBody+HasStat")
	}
	if !caps.HasOver {
		t.Error("HasOver should be true")
	}
	if !caps.HasHDR {
		t.Error("HasHDR should be true")
	}
	if !caps.HasPost {
		t.Error("HasPost should be true")
	}
	if !caps.HasCompress {
		t.Error("HasCompress should be true")
	}
	if caps.Version != 2 {
		t.Errorf("Version = %d, want 2", caps.Version)
	}
}

func TestParseCapabilities_VersionMalformed(t *testing.T) {
	t.Parallel()
	caps := parseCapabilities("VERSION abc\nREADER\n")
	if caps.Version != 0 {
		t.Errorf("Version = %d, want 0 for malformed", caps.Version)
	}
	if !caps.HasBody {
		t.Error("HasBody should still be true via READER")
	}
}

func TestParseCapabilities_VersionNoArgs(t *testing.T) {
	t.Parallel()
	caps := parseCapabilities("VERSION\nREADER\n")
	if caps.Version != 0 {
		t.Errorf("Version = %d, want 0 for no args", caps.Version)
	}
}

func TestProbeCapabilities_Errors(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)

	t.Run("write error", func(t *testing.T) {
		fw := &failingWriter{}
		bw := bufio.NewWriter(fw)
		bw.Write(make([]byte, 10000))
		bw.Flush()

		br := bufio.NewReader(strings.NewReader(""))
		caps := probeCapabilities(logger, bw, br)
		if !caps.HasBody || !caps.HasStat {
			t.Error("expected default capabilities on write failure")
		}
	})

	t.Run("flush error", func(t *testing.T) {
		fw := &failingWriter{}
		bw := bufio.NewWriter(fw)
		br := bufio.NewReader(strings.NewReader(""))
		caps := probeCapabilities(logger, bw, br)
		if !caps.HasBody || !caps.HasStat {
			t.Error("expected default capabilities on flush failure")
		}
	})

	t.Run("read greeting error", func(t *testing.T) {
		var buf strings.Builder
		bw := bufio.NewWriter(&buf)
		br := bufio.NewReader(strings.NewReader(""))
		caps := probeCapabilities(logger, bw, br)
		if !caps.HasBody || !caps.HasStat {
			t.Error("expected default capabilities on greeting read failure")
		}
	})

	t.Run("rejected status", func(t *testing.T) {
		var buf strings.Builder
		bw := bufio.NewWriter(&buf)
		br := bufio.NewReader(strings.NewReader("500 Command not recognized\r\n"))
		caps := probeCapabilities(logger, bw, br)
		if !caps.HasBody || !caps.HasStat {
			t.Error("expected default capabilities on status rejection")
		}
	})

	t.Run("read body error", func(t *testing.T) {
		var buf strings.Builder
		bw := bufio.NewWriter(&buf)
		br := bufio.NewReader(strings.NewReader("101 capabilities follow\r\nVERSION 2\r\nREADER\r\n"))
		caps := probeCapabilities(logger, bw, br)
		if !caps.HasBody || !caps.HasStat {
			t.Error("expected default capabilities on body read failure")
		}
	})
}

type failingWriter struct{}

func (f *failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestDefaultCapabilities(t *testing.T) {
	t.Parallel()
	caps := defaultCapabilities()
	if !caps.HasBody || !caps.HasStat {
		t.Error("defaults should have HasBody=true, HasStat=true")
	}
}

// ---------- newDialOptions ----------

func TestNewDialOptions_DefaultPorts(t *testing.T) {
	t.Parallel()
	// Plain (no SSL).
	opts, err := newDialOptions(config.ServerConfig{Host: "news.example.com"})
	if err != nil {
		t.Fatalf("newDialOptions: %v", err)
	}
	if opts.port != 119 {
		t.Errorf("plain port = %d, want 119", opts.port)
	}
	if opts.readBuf != 256*1024 {
		t.Errorf("readBuf = %d, want 262144", opts.readBuf)
	}

	// SSL.
	opts, err = newDialOptions(config.ServerConfig{Host: "news.example.com", SSL: true})
	if err != nil {
		t.Fatalf("newDialOptions: %v", err)
	}
	if opts.port != 563 {
		t.Errorf("SSL port = %d, want 563", opts.port)
	}
}

func TestNewDialOptions_TimeoutNonZero(t *testing.T) {
	t.Parallel()
	opts, err := newDialOptions(config.ServerConfig{Host: "x", Timeout: 10})
	if err != nil {
		t.Fatalf("newDialOptions: %v", err)
	}
	if opts.dialer.Timeout != 10*time.Second {
		t.Errorf("timeout = %v, want 10s", opts.dialer.Timeout)
	}
}

func TestNewDialOptions_ExplicitPort(t *testing.T) {
	t.Parallel()
	opts, err := newDialOptions(config.ServerConfig{Host: "news.example.com", Port: 8080})
	if err != nil {
		t.Fatalf("newDialOptions: %v", err)
	}
	if opts.port != 8080 {
		t.Errorf("port = %d, want 8080", opts.port)
	}
}

func TestNewDialOptions_PipeliningMin(t *testing.T) {
	t.Parallel()
	opts, err := newDialOptions(config.ServerConfig{Host: "x", PipeliningRequests: 0})
	if err != nil {
		t.Fatalf("newDialOptions: %v", err)
	}
	if opts.pipelining != 1 {
		t.Errorf("pipelining = %d, want 1 (minimum)", opts.pipelining)
	}
}

func TestNewDialOptions_TimeoutDefault(t *testing.T) {
	t.Parallel()
	opts, err := newDialOptions(config.ServerConfig{Host: "x", Timeout: 0})
	if err != nil {
		t.Fatalf("newDialOptions: %v", err)
	}
	if opts.dialer.Timeout.Seconds() != 60 {
		t.Errorf("timeout = %v, want 60s", opts.dialer.Timeout)
	}
}

// ---------- State transitions ----------

func TestCanTransition_Valid(t *testing.T) {
	t.Parallel()
	valid := [][2]State{
		{StateDisconnected, StateConnected},
		{StateDisconnected, StateClosed},
		{StateConnected, StateAuthenticated},
		{StateConnected, StateReady},
		{StateConnected, StateClosed},
		{StateAuthenticated, StateReady},
		{StateAuthenticated, StateClosed},
		{StateReady, StateConnected},
		{StateReady, StateClosed},
	}
	for _, tt := range valid {
		if !tt[0].canTransition(tt[1]) {
			t.Errorf("%v → %v should be valid", tt[0], tt[1])
		}
	}
}

func TestCanTransition_Invalid(t *testing.T) {
	t.Parallel()
	invalid := [][2]State{
		{StateClosed, StateReady},
		{StateClosed, StateConnected},
		{StateDisconnected, StateReady},
		{StateDisconnected, StateAuthenticated},
		{StateReady, StateAuthenticated},
	}
	for _, tt := range invalid {
		if tt[0].canTransition(tt[1]) {
			t.Errorf("%v → %v should be invalid", tt[0], tt[1])
		}
	}
}

// ---------- errInvalidTransition ----------

func TestErrInvalidTransition_Error(t *testing.T) {
	t.Parallel()
	e := errInvalidTransition{from: StateReady, to: StateAuthenticated}
	got := e.Error()
	if got == "" {
		t.Error("Error() should not be empty")
	}
}

func TestErrInvalidTransition_Is(t *testing.T) {
	t.Parallel()
	e := errInvalidTransition{from: StateReady, to: StateAuthenticated}
	if !errors.Is(e, ErrInvalidState) {
		t.Error("errors.Is(errInvalidTransition, ErrInvalidState) = false")
	}
}

// ---------- State.String ----------

func TestState_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state State
		want  string
	}{
		{StateDisconnected, "disconnected"},
		{StateConnected, "connected"},
		{StateAuthenticated, "authenticated"},
		{StateReady, "ready"},
		{StateClosed, "closed"},
		{State(99), "state(99)"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

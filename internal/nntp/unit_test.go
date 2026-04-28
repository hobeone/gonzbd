package nntp

import (
	"crypto/tls"
	"errors"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/config"
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

// ---------- tlsVersionString ----------

func TestTlsVersionString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version uint16
		want    string
	}{
		{tls.VersionTLS13, "TLSv1.3"},
		{tls.VersionTLS12, "TLSv1.2"},
		{tls.VersionTLS11, "TLSv1.1"},
		{tls.VersionTLS10, "TLSv1.0"},
		{0x0300, "TLS(0x0300)"},
	}
	for _, tt := range tests {
		got := tlsVersionString(tt.version)
		if got != tt.want {
			t.Errorf("tlsVersionString(0x%04x) = %q, want %q", tt.version, got, tt.want)
		}
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
	// Defaults: HasBody and HasStat are forced true even when not listed.
	if !caps.HasBody {
		t.Error("HasBody should default to true")
	}
	if !caps.HasStat {
		t.Error("HasStat should default to true")
	}
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

	// SSL.
	opts, err = newDialOptions(config.ServerConfig{Host: "news.example.com", SSL: true})
	if err != nil {
		t.Fatalf("newDialOptions: %v", err)
	}
	if opts.port != 563 {
		t.Errorf("SSL port = %d, want 563", opts.port)
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

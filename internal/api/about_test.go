package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type mockTransport struct {
	roundTrip func(*http.Request) (*http.Response, error)
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.roundTrip(req)
}

func TestModeAbout(t *testing.T) {
	oldTransport := http.DefaultClient.Transport
	defer func() { http.DefaultClient.Transport = oldTransport }()

	http.DefaultClient.Transport = &mockTransport{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.String(), "api.ipify.org") {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("192.0.2.1")),
				}, nil
			}
			if strings.Contains(req.URL.String(), "api6.ipify.org") {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("2001:db8::1")),
				}, nil
			}
			return nil, fmt.Errorf("unexpected request URL: %s", req.URL)
		},
	}

	s := testServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=about", nil)
	s.modeAbout(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}

	about, ok := m["about"].(map[string]any)
	if !ok {
		t.Fatalf("expected map in 'about', got: %T", m["about"])
	}

	if about["public_ipv4"] != "192.0.2.1" {
		t.Errorf("public_ipv4 = %v; want '192.0.2.1'", about["public_ipv4"])
	}
	if about["public_ipv6"] != "2001:db8::1" {
		t.Errorf("public_ipv6 = %v; want '2001:db8::1'", about["public_ipv6"])
	}
}

func TestLocalIPv4(t *testing.T) {
	t.Parallel()
	_ = localIPv4()
}

func TestPublicIP_Errors(t *testing.T) {
	oldTransport := http.DefaultClient.Transport
	defer func() { http.DefaultClient.Transport = oldTransport }()

	// Case 1: client.Do returns error
	http.DefaultClient.Transport = &mockTransport{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("network error")
		},
	}
	if got := publicIP(context.Background(), "http://localhost"); got != "" {
		t.Errorf("publicIP returned %q, want empty on network error", got)
	}

	// Case 2: status code not 200
	http.DefaultClient.Transport = &mockTransport{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader("internal error")),
			}, nil
		},
	}
	if got := publicIP(context.Background(), "http://localhost"); got != "" {
		t.Errorf("publicIP returned %q, want empty on 500 status", got)
	}

	// Case 3: body read failure or truncation
	http.DefaultClient.Transport = &mockTransport{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(strings.Repeat("a", 100))),
			}, nil
		},
	}
	got := publicIP(context.Background(), "http://localhost")
	if len(got) != 64 {
		t.Errorf("publicIP returned body length %d, want 64", len(got))
	}
}

func TestResolveBinary(t *testing.T) {
	t.Parallel()
	// Case 1: cfgPath is set and fails to resolve
	if got := resolveBinary("/nonexistent/binary"); got != "" {
		t.Errorf("resolveBinary(/nonexistent/binary) = %q, want empty", got)
	}

	// Case 2: cfgPath is empty, fallbacks fail
	if got := resolveBinary("", "nonexistent-fallback-12345"); got != "" {
		t.Errorf("resolveBinary('', nonexistent) = %q, want empty", got)
	}

	// Case 3: cfgPath is empty, fallbacks succeed
	if got := resolveBinary("", "sh"); got == "" {
		t.Error("resolveBinary('', 'sh') returned empty, want path to sh")
	}
}

func TestModeAbout_ContextCancellation(t *testing.T) {
	oldTransport := http.DefaultClient.Transport
	defer func() { http.DefaultClient.Transport = oldTransport }()

	var called atomic.Int32
	http.DefaultClient.Transport = &mockTransport{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			called.Add(1)
			if req.Context().Err() != context.Canceled {
				t.Errorf("roundTrip request context Err() = %v; want context.Canceled", req.Context().Err())
			}
			return nil, req.Context().Err()
		},
	}

	s := testServer()
	rr := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=about", nil).WithContext(ctx)
	s.modeAbout(rr, req)

	if called.Load() != 2 {
		t.Errorf("expected 2 roundTrip calls, got %d", called.Load())
	}
}

func TestPublicIP_ContextCancellation(t *testing.T) {
	oldTransport := http.DefaultClient.Transport
	defer func() { http.DefaultClient.Transport = oldTransport }()

	var called atomic.Int32
	http.DefaultClient.Transport = &mockTransport{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			called.Add(1)
			if req.Context().Err() != context.Canceled {
				t.Errorf("roundTrip request context Err() = %v; want context.Canceled", req.Context().Err())
			}
			return nil, req.Context().Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := publicIP(ctx, "http://localhost"); got != "" {
		t.Errorf("publicIP returned %q; want empty string on canceled context", got)
	}
	if called.Load() != 1 {
		t.Errorf("expected 1 roundTrip call, got %d", called.Load())
	}
}

func TestPublicIP_Deadline(t *testing.T) {
	oldTransport := http.DefaultClient.Transport
	defer func() { http.DefaultClient.Transport = oldTransport }()

	var checked atomic.Bool
	http.DefaultClient.Transport = &mockTransport{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			deadline, ok := req.Context().Deadline()
			if !ok {
				t.Errorf("request context has no deadline")
			}
			diff := time.Until(deadline)
			if diff < 2*time.Second || diff > 4*time.Second {
				t.Errorf("request context deadline diff %v; want ~3s", diff)
			}
			checked.Store(true)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("1.2.3.4")),
			}, nil
		},
	}

	if got := publicIP(context.Background(), "http://localhost"); got != "1.2.3.4" {
		t.Errorf("publicIP() = %q; want 1.2.3.4", got)
	}
	if !checked.Load() {
		t.Error("roundTrip was not called")
	}
}

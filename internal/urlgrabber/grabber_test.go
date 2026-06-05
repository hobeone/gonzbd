package urlgrabber

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/types"
)

// MockHandler captures NZBs and errors for testing.
type MockHandler struct {
	mu      sync.Mutex
	nzbs    map[string][]byte
	lastErr error
	count   int
}

func (m *MockHandler) HandleNZB(ctx context.Context, filename string, data []byte, opts types.FetchOptions) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nzbs == nil {
		m.nzbs = make(map[string][]byte)
	}
	m.count++
	m.nzbs[fmt.Sprintf("%s_%d", filename, m.count)] = data
	return fmt.Sprintf("mock_nzo_%d", m.count), m.lastErr
}

func (m *MockHandler) NZBs() map[string][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string][]byte)
	maps.Copy(result, m.nzbs)
	return result
}

func TestFetchPlainNZB(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
</nzb>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Write(nzbData)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), server.URL+"/test.nzb", types.FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 NZB, got %d", len(ids))
	}

	nzbs := handler.NZBs()
	if len(nzbs) != 1 {
		t.Fatalf("expected 1 NZB in handler, got %d", len(nzbs))
	}

	var found bool
	for filename, data := range nzbs {
		if strings.HasPrefix(filename, "test.nzb") && bytes.Equal(data, nzbData) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected NZB data not found")
	}
}

func TestFetchFilenameFromContentDisposition(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Header().Set("Content-Disposition", `attachment; filename="cool.nzb"`)
		w.Write(nzbData)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), server.URL+"/file", types.FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 NZB, got %d", len(ids))
	}

	nzbs := handler.NZBs()
	var found bool
	for filename := range nzbs {
		if strings.HasPrefix(filename, "cool.nzb") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected filename 'cool.nzb', got keys: %v", nzbs)
	}
}

func TestContentDispositionPathTraversal(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Header().Set("Content-Disposition", `attachment; filename="../../etc/passwd.nzb"`)
		w.Write(nzbData)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), server.URL+"/file", types.FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 NZB, got %d", len(ids))
	}

	nzbs := handler.NZBs()
	var found bool
	for filename := range nzbs {
		if strings.HasPrefix(filename, "passwd.nzb") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected filename starting with 'passwd.nzb', got keys: %v", nzbs)
	}
}

func TestFetchFilenameFallbackToURLPath(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Write(nzbData)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), server.URL+"/path/myfile.nzb", types.FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 NZB, got %d", len(ids))
	}

	nzbs := handler.NZBs()
	var found bool
	for filename := range nzbs {
		if strings.HasPrefix(filename, "myfile.nzb") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected filename 'myfile.nzb', got keys: %v", nzbs)
	}
}

func TestFetchHTMLRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html>Login Page</html>"))
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), server.URL+"/test.nzb", types.FetchOptions{})
	if err == nil {
		t.Fatalf("expected error for HTML content, got none")
	}

	if len(ids) != 0 {
		t.Fatalf("expected 0 NZBs for HTML error, got %d", len(ids))
	}

	if !strings.Contains(err.Error(), "HTML") {
		t.Fatalf("expected error to mention HTML, got: %v", err)
	}
}

func TestFetchSizeCapExceeded(t *testing.T) {
	nzbData := make([]byte, 200)
	for i := range nzbData {
		nzbData[i] = 'x'
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Write(nzbData)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{MaxBytes: 100, AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), server.URL+"/test.nzb", types.FetchOptions{})
	if err == nil {
		t.Fatalf("expected error for size cap exceeded, got none")
	}

	if len(ids) != 0 {
		t.Fatalf("expected 0 NZBs for size error, got %d", len(ids))
	}

	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected error to mention size limit, got: %v", err)
	}

	if len(handler.NZBs()) != 0 {
		t.Fatalf("handler should not be called on size error")
	}
}

func TestFetchHTTPBasicAuthViaConfig(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "user" || password != "pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Write(nzbData)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{Username: "user", Password: "pass", AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), server.URL+"/test.nzb", types.FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 NZB, got %d", len(ids))
	}
}

func TestFetchHTTPBasicAuthViaURL(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "user" || password != "pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Write(nzbData)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	urlWithAuth := strings.Replace(server.URL, "http://", "http://user:pass@", 1)
	ids, err := grabber.Fetch(t.Context(), urlWithAuth+"/test.nzb", types.FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 NZB, got %d", len(ids))
	}
}

func TestFetchHTTPBasicAuthConfigOverridesURL(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "config_user" || password != "config_pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Write(nzbData)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{Username: "config_user", Password: "config_pass", AllowPrivateIPs: true}, handler)

	urlWithAuth := strings.Replace(server.URL, "http://", "http://url_user:url_pass@", 1)
	ids, err := grabber.Fetch(t.Context(), urlWithAuth+"/test.nzb", types.FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 NZB, got %d", len(ids))
	}
}

func TestFetchRedirect(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`)

	// Create the final server.
	finalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Write(nzbData)
	}))
	defer finalServer.Close()

	// Create the redirect server.
	redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, finalServer.URL+"/final.nzb", http.StatusFound)
	}))
	defer redirectServer.Close()

	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), redirectServer.URL+"/redirect.nzb", types.FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 NZB, got %d", len(ids))
	}
}

func TestFetchServer404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), server.URL+"/notfound.nzb", types.FetchOptions{})
	if err == nil {
		t.Fatalf("expected error for 404, got none")
	}

	if len(ids) != 0 {
		t.Fatalf("expected 0 NZBs for 404 error, got %d", len(ids))
	}

	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected error to mention 404, got: %v", err)
	}
}

func TestFetchContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`))
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{Timeout: 5 * time.Second, AllowPrivateIPs: true}, handler)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	ids, err := grabber.Fetch(ctx, server.URL+"/test.nzb", types.FetchOptions{})
	if err == nil {
		t.Fatalf("expected error for cancelled context, got none")
	}

	if len(ids) != 0 {
		t.Fatalf("expected 0 NZBs for cancelled context, got %d", len(ids))
	}
}

func TestFetchTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`))
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{Timeout: 50 * time.Millisecond, AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), server.URL+"/test.nzb", types.FetchOptions{})
	if err == nil {
		t.Fatalf("expected timeout error, got none")
	}

	if len(ids) != 0 {
		t.Fatalf("expected 0 NZBs for timeout, got %d", len(ids))
	}
}

func TestFetchGzipNZB(t *testing.T) {
	plainNZB := []byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`)
	gzipBuf := bytes.NewBuffer(nil)
	gzipWriter := gzip.NewWriter(gzipBuf)
	gzipWriter.Write(plainNZB)
	gzipWriter.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(gzipBuf.Bytes())
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), server.URL+"/test.nzb.gz", types.FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 NZB, got %d", len(ids))
	}

	nzbs := handler.NZBs()
	if len(nzbs) != 1 {
		t.Fatalf("expected 1 NZB in handler, got %d", len(nzbs))
	}

	var filename string
	var data []byte
	for f, d := range nzbs {
		filename = f
		data = d
	}

	if !bytes.Equal(data, plainNZB) {
		t.Fatalf("decompressed NZB data mismatch")
	}

	if !strings.HasPrefix(filename, "test.nzb.gz") {
		t.Fatalf("expected filename with 'test.nzb.gz' prefix, got: %s", filename)
	}
}

func TestFetchEmptyURL(t *testing.T) {
	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), "", types.FetchOptions{})
	if err == nil {
		t.Fatalf("expected error for empty URL, got none")
	}

	if len(ids) != 0 {
		t.Fatalf("expected 0 NZBs for empty URL, got %d", len(ids))
	}
}

func TestFetchHandlerError(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Write(nzbData)
	}))
	defer server.Close()

	handler := &MockHandler{lastErr: fmt.Errorf("handler error")}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), server.URL+"/test.nzb", types.FetchOptions{})
	if err != nil {
		t.Fatalf("unexpected fetch error: %v", err)
	}

	// Handler error is logged, not returned. count=0 because the single
	// NZB failed to be handled.
	if len(ids) != 0 {
		t.Fatalf("expected 0 NZBs on handler error, got %d", len(ids))
	}
}

func TestExtractFilenameWithNoExtension(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Write(nzbData)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), server.URL+"/myfile", types.FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 NZB, got %d", len(ids))
	}

	nzbs := handler.NZBs()
	var found bool
	for filename := range nzbs {
		if strings.HasPrefix(filename, "myfile.nzb") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected filename 'myfile.nzb', got keys: %v", nzbs)
	}
}

func TestFetchRaceCondition(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Write(nzbData)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, err := grabber.Fetch(t.Context(), server.URL+"/test.nzb", types.FetchOptions{})
			if err != nil {
				t.Errorf("concurrent Fetch failed: %v", err)
			}
		})
	}
	wg.Wait()

	if len(handler.NZBs()) != 10 {
		t.Fatalf("expected 10 NZBs from concurrent fetches, got %d", len(handler.NZBs()))
	}
}

func TestExtractFromContentDisposition(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`attachment; filename="test.nzb"`, "test.nzb"},
		{`attachment; filename=test.nzb`, "test.nzb"},
		{`inline; filename="file with spaces.nzb"`, "file with spaces.nzb"},
		{`attachment; filename=""; other=value`, ""},
		{`attachment`, ""},
		{`filename="only.nzb"`, ""}, // malformed: no media type prefix
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := extractFromContentDisposition(tt.input)
			if result != tt.expected {
				t.Fatalf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// --- SSRF protection tests ---

func TestFetch_RejectsFileScheme(t *testing.T) {
	handler := &MockHandler{}
	grabber := New(Config{}, handler)

	ids, err := grabber.Fetch(t.Context(), "file:///etc/passwd", types.FetchOptions{})
	if err == nil {
		t.Fatalf("expected error for file:// scheme, got none")
	}
	if !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("expected 'unsupported scheme' in error, got: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected 0 NZBs for file:// scheme, got %d", len(ids))
	}
}

func TestFetch_RejectsPrivateIP(t *testing.T) {
	handler := &MockHandler{}
	// AllowPrivateIPs is false (default), so 127.0.0.1 should be blocked.
	grabber := New(Config{}, handler)

	ids, err := grabber.Fetch(t.Context(), "http://127.0.0.1/foo.nzb", types.FetchOptions{})
	if err == nil {
		t.Fatalf("expected error for private IP, got none")
	}
	if !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("expected 'private' or 'loopback' in error, got: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected 0 NZBs for private IP, got %d", len(ids))
	}
}

func TestFetch_RejectsLocalhostURL(t *testing.T) {
	handler := &MockHandler{}
	grabber := New(Config{}, handler)

	ids, err := grabber.Fetch(t.Context(), "http://localhost/foo.nzb", types.FetchOptions{})
	if err == nil {
		t.Fatalf("expected error for localhost URL, got none")
	}
	if !strings.Contains(err.Error(), "localhost") {
		t.Fatalf("expected 'localhost' in error, got: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected 0 NZBs for localhost, got %d", len(ids))
	}
}

func TestFetch_RejectsRedirectToPrivateIP(t *testing.T) {
	// Set up an httptest server that redirects to 127.0.0.1.
	// The initial request targets the httptest server (also 127.0.0.1), so
	// we must AllowPrivateIPs for the initial URL but rely on the CheckRedirect
	// to block the redirect target. To test this properly, we configure
	// AllowPrivateIPs=false and redirect to a literal private IP.
	//
	// Since AllowPrivateIPs=false blocks even the initial request to httptest,
	// we set AllowPrivateIPs=true and verify that the redirect to "localhost"
	// is caught (localhost is always blocked regardless of AllowPrivateIPs).
	redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost/evil.nzb", http.StatusFound)
	}))
	defer redirectServer.Close()

	handler := &MockHandler{}
	grabber := New(Config{AllowPrivateIPs: true}, handler)

	ids, err := grabber.Fetch(t.Context(), redirectServer.URL+"/redirect.nzb", types.FetchOptions{})
	if err == nil {
		t.Fatalf("expected error for redirect to localhost, got none")
	}
	if !strings.Contains(err.Error(), "localhost") {
		t.Fatalf("expected 'localhost' in error, got: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected 0 NZBs for redirect to private IP, got %d", len(ids))
	}
}

func TestExtractFilename_Direct(t *testing.T) {
	parsedURL, _ := url.Parse("http://example.com/downloads/file")

	// 1. Content-Disposition takes precedence
	resp1 := &http.Response{Header: make(http.Header)}
	resp1.Header.Set("Content-Disposition", `attachment; filename="header.nzb"`)
	if got := extractFilename(resp1, parsedURL); got != "header.nzb" {
		t.Errorf("extractFilename: got %q, want %q", got, "header.nzb")
	}

	// 2. Fallback to URL path with extension appended
	resp2 := &http.Response{Header: make(http.Header)}
	if got := extractFilename(resp2, parsedURL); got != "file.nzb" {
		t.Errorf("extractFilename: got %q, want %q", got, "file.nzb")
	}

	// 3. Root path "/": filepath.Base("/") returns "/", which must fall back
	//    to the default name. Reverting the `|| filename == "/"` guard in
	//    extractFilename makes this case return "/.nzb" and fail.
	rootURL, _ := url.Parse("http://example.com/")
	if got := extractFilename(resp2, rootURL); got != "download.nzb" {
		t.Errorf("extractFilename root path: got %q, want %q", got, "download.nzb")
	}

	// 4. Genuinely empty path: filepath.Base("") returns ".", the other
	//    fallback branch. url.Parse always yields Path "/" for a bare host,
	//    so construct the empty-path URL directly.
	emptyURL := &url.URL{Scheme: "http", Host: "example.com"}
	if got := extractFilename(resp2, emptyURL); got != "download.nzb" {
		t.Errorf("extractFilename empty path: got %q, want %q", got, "download.nzb")
	}
}

func TestIsPrivateIP_Direct(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.1", true},
		{"192.168.1.50", true},
		{"172.16.31.254", true},
		{"169.254.0.1", true}, // link-local
		{"8.8.8.8", false},
		{"2606:4700:4700::1111", false},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("failed to parse IP %q", tt.ip)
			}
			if got := isPrivateIP(ip); got != tt.want {
				t.Errorf("isPrivateIP(%s) = %v; want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestValidateURL_Direct(t *testing.T) {
	tests := []struct {
		name         string
		urlStr       string
		allowPrivate bool
		wantErr      bool
	}{
		{"valid HTTP public", "http://8.8.8.8/test.nzb", false, false},
		{"valid HTTPS public", "https://1.1.1.1/test.nzb", false, false},
		{"unsupported scheme", "ftp://google.com/test.nzb", false, true},
		{"localhost blocked", "http://localhost/test.nzb", false, true},
		{"localhost blocked even if allowed", "http://localhost/test.nzb", true, true},
		{"private IP blocked by default", "http://127.0.0.1/test.nzb", false, true},
		{"private IP allowed by flag", "http://127.0.0.1/test.nzb", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.urlStr)
			if err != nil {
				t.Fatalf("failed to parse URL %q: %v", tt.urlStr, err)
			}
			err = validateURL(u, tt.allowPrivate)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateURL(%q, %v) error = %v; wantErr = %v", tt.urlStr, tt.allowPrivate, err, tt.wantErr)
			}
		})
	}
}

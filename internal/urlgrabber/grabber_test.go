package urlgrabber

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// MockHandler captures NZBs and errors for testing.
type MockHandler struct {
	mu      sync.Mutex
	nzbs    map[string][]byte
	lastErr error
	count   int
}

func (m *MockHandler) HandleNZB(ctx context.Context, filename string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nzbs == nil {
		m.nzbs = make(map[string][]byte)
	}
	m.count++
	m.nzbs[fmt.Sprintf("%s_%d", filename, m.count)] = data
	return m.lastErr
}

func (m *MockHandler) NZBs() map[string][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string][]byte)
	for k, v := range m.nzbs {
		result[k] = v
	}
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
	grabber := New(Config{}, handler)

	count, err := grabber.Fetch(context.Background(), server.URL+"/test.nzb")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 NZB, got %d", count)
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
	grabber := New(Config{}, handler)

	count, err := grabber.Fetch(context.Background(), server.URL+"/file")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 NZB, got %d", count)
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

func TestFetchFilenameFallbackToURLPath(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Write(nzbData)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{}, handler)

	count, err := grabber.Fetch(context.Background(), server.URL+"/path/myfile.nzb")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 NZB, got %d", count)
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
	grabber := New(Config{}, handler)

	count, err := grabber.Fetch(context.Background(), server.URL+"/test.nzb")
	if err == nil {
		t.Fatalf("expected error for HTML content, got none")
	}

	if count != 0 {
		t.Fatalf("expected 0 NZBs for HTML error, got %d", count)
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
	grabber := New(Config{MaxBytes: 100}, handler)

	count, err := grabber.Fetch(context.Background(), server.URL+"/test.nzb")
	if err == nil {
		t.Fatalf("expected error for size cap exceeded, got none")
	}

	if count != 0 {
		t.Fatalf("expected 0 NZBs for size error, got %d", count)
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
	grabber := New(Config{Username: "user", Password: "pass"}, handler)

	count, err := grabber.Fetch(context.Background(), server.URL+"/test.nzb")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 NZB, got %d", count)
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
	grabber := New(Config{}, handler)

	urlWithAuth := strings.Replace(server.URL, "http://", "http://user:pass@", 1)
	count, err := grabber.Fetch(context.Background(), urlWithAuth+"/test.nzb")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 NZB, got %d", count)
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
	grabber := New(Config{Username: "config_user", Password: "config_pass"}, handler)

	urlWithAuth := strings.Replace(server.URL, "http://", "http://url_user:url_pass@", 1)
	count, err := grabber.Fetch(context.Background(), urlWithAuth+"/test.nzb")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 NZB, got %d", count)
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
	grabber := New(Config{}, handler)

	count, err := grabber.Fetch(context.Background(), redirectServer.URL+"/redirect.nzb")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 NZB, got %d", count)
	}
}

func TestFetchServer404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{}, handler)

	count, err := grabber.Fetch(context.Background(), server.URL+"/notfound.nzb")
	if err == nil {
		t.Fatalf("expected error for 404, got none")
	}

	if count != 0 {
		t.Fatalf("expected 0 NZBs for 404 error, got %d", count)
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
	grabber := New(Config{Timeout: 5 * time.Second}, handler)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	count, err := grabber.Fetch(ctx, server.URL+"/test.nzb")
	if err == nil {
		t.Fatalf("expected error for cancelled context, got none")
	}

	if count != 0 {
		t.Fatalf("expected 0 NZBs for cancelled context, got %d", count)
	}
}

func TestFetchTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><nzb/>`))
	}))
	defer server.Close()

	handler := &MockHandler{}
	grabber := New(Config{Timeout: 50 * time.Millisecond}, handler)

	count, err := grabber.Fetch(context.Background(), server.URL+"/test.nzb")
	if err == nil {
		t.Fatalf("expected timeout error, got none")
	}

	if count != 0 {
		t.Fatalf("expected 0 NZBs for timeout, got %d", count)
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
	grabber := New(Config{}, handler)

	count, err := grabber.Fetch(context.Background(), server.URL+"/test.nzb.gz")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 NZB, got %d", count)
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
	grabber := New(Config{}, handler)

	count, err := grabber.Fetch(context.Background(), "")
	if err == nil {
		t.Fatalf("expected error for empty URL, got none")
	}

	if count != 0 {
		t.Fatalf("expected 0 NZBs for empty URL, got %d", count)
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
	grabber := New(Config{}, handler)

	count, err := grabber.Fetch(context.Background(), server.URL+"/test.nzb")
	if err == nil {
		t.Fatalf("expected error from handler, got none")
	}

	if count != 0 {
		t.Fatalf("expected 0 NZBs on handler error, got %d", count)
	}

	if !strings.Contains(err.Error(), "handler error") {
		t.Fatalf("expected error to contain 'handler error', got: %v", err)
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
	grabber := New(Config{}, handler)

	count, err := grabber.Fetch(context.Background(), server.URL+"/myfile")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 NZB, got %d", count)
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
	grabber := New(Config{}, handler)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := grabber.Fetch(context.Background(), server.URL+"/test.nzb")
			if err != nil {
				t.Errorf("concurrent Fetch failed: %v", err)
			}
		}()
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
		{`filename="only.nzb"`, "only.nzb"},
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

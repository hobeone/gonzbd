package web

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestHandler(t *testing.T) {
	// Handler() should not panic if ui/dist is correctly populated.
	// In a real build environment, this is guaranteed by the build script.
	handler, err := Handler(func() string { return "test-api-key" }, alwaysTrusted)
	if err != nil {
		t.Fatalf("Handler() returned error: %v", err)
	}
	if handler == nil {
		t.Fatal("Handler() returned nil")
	}

	// Basic check: root route should return 200 OK.
	// We don't check the body content here as it depends on the Vite build output.
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("GET / status code = %v, want %v", rr.Code, http.StatusOK)
	}
}

// failingSubFS is an fs.SubFS whose Sub always fails, the one way to make
// fs.Sub return an error for the valid path "dist".
type failingSubFS struct{ fstest.MapFS }

var errSub = errors.New("injected Sub failure")

func (failingSubFS) Sub(string) (fs.FS, error) { return nil, errSub }

func TestHandlerFrom_ReturnsSubError(t *testing.T) {
	t.Parallel()
	handler, err := handlerFrom(failingSubFS{}, func() string { return "k" }, alwaysTrusted)
	if !errors.Is(err, errSub) {
		t.Fatalf("handlerFrom() error = %v, want wrapping %v", err, errSub)
	}
	if handler != nil {
		t.Errorf("handlerFrom() handler = %v, want nil on error", handler)
	}
}

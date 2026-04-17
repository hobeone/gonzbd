package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandler(t *testing.T) {
	// Handler() should not panic if ui/dist is correctly populated.
	// In a real build environment, this is guaranteed by the build script.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Handler() panicked: %v", r)
		}
	}()

	handler := Handler()
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

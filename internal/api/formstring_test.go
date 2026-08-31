package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/api/apitest"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
)

// M14: Verify formValue reads values correctly without triggering
// implicit unbounded parsing (safe because middleware caps body size).

// TestFormValue_QueryParam verifies formValue reads from query parameters.
func TestFormValue_QueryParam(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "/api?name=delete&value=SABnzbd_nzo_abc", nil)
	if got := formValue(req, "name"); got != "delete" {
		t.Errorf("formValue(name) = %q, want %q", got, "delete")
	}
	if got := formValue(req, "value"); got != "SABnzbd_nzo_abc" {
		t.Errorf("formValue(value) = %q, want %q", got, "SABnzbd_nzo_abc")
	}
}

// TestFormValue_PostFormBody verifies formValue reads from URL-encoded form body.
func TestFormValue_PostFormBody(t *testing.T) {
	t.Parallel()
	body := strings.NewReader("name=delete&value=SABnzbd_nzo_xyz")
	req := httptest.NewRequest("POST", "/api", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if got := formValue(req, "name"); got != "delete" {
		t.Errorf("formValue(name) from body = %q, want %q", got, "delete")
	}
}

// TestFormValue_BodyTakesPrecedence verifies that form body takes
// precedence over query parameter for the same key, per Go's r.FormValue
// semantics: "POST and PUT body parameters take precedence over URL query
// string values."
func TestFormValue_BodyTakesPrecedence(t *testing.T) {
	t.Parallel()
	body := strings.NewReader("name=body_value")
	req := httptest.NewRequest("POST", "/api?name=query_value", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if got := formValue(req, "name"); got != "body_value" {
		t.Errorf("formValue(name) = %q, want %q (body should take precedence per FormValue)", got, "body_value")
	}
}

// TestFormValue_MissingKey returns empty string for absent keys.
func TestFormValue_MissingKey(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "/api?mode=queue", nil)
	if got := formValue(req, "nonexistent"); got != "" {
		t.Errorf("formValue(nonexistent) = %q, want empty string", got)
	}
}

// M13: Verify configTestServer handles private IP addresses gracefully.
// Private IPs are NOT rejected — they just fail to connect like any
// unreachable host. This is correct for a Usenet client where users
// may run local NNTP servers on RFC 1918 addresses.

type privateIPTestApp struct {
	apitest.NopApp
	calledHost string
}

func (p *privateIPTestApp) TestNNTPServer(_ context.Context, cfg config.ServerConfig) (app.NNTPTestResult, error) {
	p.calledHost = cfg.Host
	return app.NNTPTestResult{}, errors.New("dial tcp " + cfg.Host + ":119: connect: network is unreachable")
}

func TestConfigTestServer_PrivateIPNotRejected(t *testing.T) {
	t.Parallel()
	s := testServer()
	mockApp := &privateIPTestApp{}
	s.setAppServices(mockApp)

	// 10.255.255.1 is a private (RFC 1918) address. The handler should
	// accept it and return a connection-failed result (not a 400 reject).
	rr := apiGet(t, s.Handler(), "/api?mode=config&name=test_server&host=10.255.255.1&port=119&apikey="+testAPIKey)
	if rr.Code != 200 {
		t.Fatalf("status = %d; want 200 (handler should not reject private IPs)", rr.Code)
	}
	if mockApp.calledHost != "10.255.255.1" {
		t.Errorf("calledHost = %q; want 10.255.255.1", mockApp.calledHost)
	}
	m := decodeJSON(t, rr)
	result, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing or not a map: %v", m["result"])
	}
	// Connection to a private IP should fail, not panic or reject.
	if result["passed"] != false {
		t.Errorf("passed = %v; want false (private IP is unreachable)", result["passed"])
	}
}

// errorReader simulates a read error during form parsing.
type errorReader struct{}

func (e *errorReader) Read(_ []byte) (int, error) {
	return 0, os.ErrPermission
}

func TestFormValue_MultipartSuccess(t *testing.T) {
	t.Parallel()
	body := strings.NewReader("--myboundary\r\nContent-Disposition: form-data; name=\"name\"\r\n\r\nupload_name\r\n--myboundary--\r\n")
	req := httptest.NewRequest("POST", "/api?name=query_fallback", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=myboundary")

	if got := formValue(req, "name"); got != "upload_name" {
		t.Errorf("formValue(name) from multipart = %q, want %q", got, "upload_name")
	}
}

func TestFormValue_MultipartEmptyValueFallback(t *testing.T) {
	t.Parallel()
	body := strings.NewReader("--myboundary\r\nContent-Disposition: form-data; name=\"name\"\r\n\r\n\r\n--myboundary--\r\n")
	req := httptest.NewRequest("POST", "/api?name=query_fallback", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=myboundary")

	if got := formValue(req, "name"); got != "query_fallback" {
		t.Errorf("formValue(name) fallback from empty multipart = %q, want %q", got, "query_fallback")
	}
}

func TestFormValue_MultipartParseErrorFallback(t *testing.T) {
	t.Parallel()
	body := strings.NewReader("broken-multipart-body-without-boundary-markers")
	req := httptest.NewRequest("POST", "/api?name=query_fallback", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=myboundary")

	if got := formValue(req, "name"); got != "query_fallback" {
		t.Errorf("formValue(name) fallback from broken multipart = %q, want %q", got, "query_fallback")
	}
}

func TestFormValue_PutAndPatchMethods(t *testing.T) {
	t.Parallel()
	bodyPut := strings.NewReader("name=put_body")
	reqPut := httptest.NewRequest("PUT", "/api?name=query_val", bodyPut)
	reqPut.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := formValue(reqPut, "name"); got != "put_body" {
		t.Errorf("formValue(name) PUT = %q, want %q", got, "put_body")
	}

	bodyPatch := strings.NewReader("name=patch_body")
	reqPatch := httptest.NewRequest("PATCH", "/api?name=query_val", bodyPatch)
	reqPatch.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := formValue(reqPatch, "name"); got != "patch_body" {
		t.Errorf("formValue(name) PATCH = %q, want %q", got, "patch_body")
	}
}

func TestFormValue_ParseFormErrorFallback(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("POST", "/api?name=query_fallback", &errorReader{})
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if got := formValue(req, "name"); got != "query_fallback" {
		t.Errorf("formValue(name) fallback from ParseForm error = %q, want %q", got, "query_fallback")
	}
}

func TestFormValue_PostFormEmptyValueFallback(t *testing.T) {
	t.Parallel()
	body := strings.NewReader("name=&other=val")
	req := httptest.NewRequest("POST", "/api?name=query_fallback", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if got := formValue(req, "name"); got != "query_fallback" {
		t.Errorf("formValue(name) fallback from empty PostForm value = %q, want %q", got, "query_fallback")
	}
}

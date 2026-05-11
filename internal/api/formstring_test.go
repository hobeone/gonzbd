package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// M14: Verify formString reads values correctly without triggering
// implicit unbounded parsing (safe because middleware caps body size).

// TestFormString_QueryParam verifies formString reads from query parameters.
func TestFormString_QueryParam(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "/api?name=delete&value=SABnzbd_nzo_abc", nil)
	if got := formString(req, "name"); got != "delete" {
		t.Errorf("formString(name) = %q, want %q", got, "delete")
	}
	if got := formString(req, "value"); got != "SABnzbd_nzo_abc" {
		t.Errorf("formString(value) = %q, want %q", got, "SABnzbd_nzo_abc")
	}
}

// TestFormString_PostFormBody verifies formString reads from URL-encoded form body.
func TestFormString_PostFormBody(t *testing.T) {
	t.Parallel()
	body := strings.NewReader("name=delete&value=SABnzbd_nzo_xyz")
	req := httptest.NewRequest("POST", "/api", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if got := formString(req, "name"); got != "delete" {
		t.Errorf("formString(name) from body = %q, want %q", got, "delete")
	}
}

// TestFormString_BodyTakesPrecedence verifies that form body takes
// precedence over query parameter for the same key, per Go's r.FormValue
// semantics: "POST and PUT body parameters take precedence over URL query
// string values."
func TestFormString_BodyTakesPrecedence(t *testing.T) {
	t.Parallel()
	body := strings.NewReader("name=body_value")
	req := httptest.NewRequest("POST", "/api?name=query_value", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if got := formString(req, "name"); got != "body_value" {
		t.Errorf("formString(name) = %q, want %q (body should take precedence per FormValue)", got, "body_value")
	}
}

// TestFormString_MissingKey returns empty string for absent keys.
func TestFormString_MissingKey(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "/api?mode=queue", nil)
	if got := formString(req, "nonexistent"); got != "" {
		t.Errorf("formString(nonexistent) = %q, want empty string", got)
	}
}

// M13: Verify configTestServer handles private IP addresses gracefully.
// Private IPs are NOT rejected — they just fail to connect like any
// unreachable host. This is correct for a Usenet client where users
// may run local NNTP servers on RFC 1918 addresses.

func TestConfigTestServer_PrivateIPNotRejected(t *testing.T) {
	t.Parallel()
	s := testServer()

	// 10.255.255.1 is a private (RFC 1918) address. The handler should
	// accept it and return a connection-failed result (not a 400 reject).
	rr := apiGet(t, s.Handler(), "/api?mode=config&name=test_server&host=10.255.255.1&port=119&apikey="+testAPIKey)
	if rr.Code != 200 {
		t.Fatalf("status = %d; want 200 (handler should not reject private IPs)", rr.Code)
	}
	m := decodeJSON(t, rr)
	result, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing or not a map: %v", m["result"])
	}
	// Connection to a private IP should fail (timeout), not panic.
	if result["passed"] != false {
		t.Errorf("passed = %v; want false (private IP is unreachable in CI)", result["passed"])
	}
}

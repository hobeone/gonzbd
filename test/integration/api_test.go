//go:build integration

// Package integration contains end-to-end integration tests for the gonzbd daemon.
// Tests in this package require external dependencies (mock NNTP server, real HTTP,
// optional par2/unrar binaries) and are gated behind the "integration" build tag.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/api"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/urlgrabber"
)

// nopNZBHandler is a no-op urlgrabber.Handler used in tests.
type nopNZBHandler struct{}

func (nopNZBHandler) HandleNZB(context.Context, string, []byte, types.FetchOptions) (string, error) {
	return "", nil
}

const (
	integrationAPIKey = "aabbccddeeff0011"
	integrationNZBKey = "1100ffeeddccbbaa"
)

// buildAPIServer constructs a minimal, fully-wired api.Server backed by a fresh
// history.Repository and in-memory queue. It returns the Server, an httptest.Server
// wrapping it, and the temp directory used for the history DB.
//
// The returned directory is both the database home and the temp directory backing the server's history DB; it is also configured as
// General.DownloadDir so tests exercising path-scoped handlers (e.g.
// addlocalfile, browse) have a valid configured root to operate within.
func buildAPIServer(t *testing.T) (srv *api.Server, ts *httptest.Server, dir string) {
	t.Helper()

	dir = t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) //nolint:errcheck // test cleanup

	repo := history.NewRepository(db)

	cfg := &config.Config{}
	cfg.General.APIKey = integrationAPIKey
	cfg.General.NZBKey = integrationNZBKey
	cfg.General.DownloadDir = dir
	cfg.General.CompleteDir = filepath.Join(dir, "complete")
	cfg.General.AdminDir = filepath.Join(dir, "admin")

	application, err := app.New(cfg, repo)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	grabber := urlgrabber.New(urlgrabber.Config{}, nopNZBHandler{})

	srv = api.New(api.Options{
		Version:    "integration-test",
		Dispatcher: application.Dispatcher(),
		History:    repo,
		Config:     cfg,
		Grabber:    grabber,
		App:        application,
	})

	ts = httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, dir
}

// apiDo sends a GET to the httptest server's /api endpoint with the given
// query params appended.
func apiDo(t *testing.T, ts *httptest.Server, query string) *http.Response {
	t.Helper()
	url := ts.URL + "/api?" + query
	resp, err := http.Get(url) //nolint:gosec // G107: test URL from ephemeral test server
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// decodeAPIJSON reads and JSON-decodes the response body into map[string]any.
func decodeAPIJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // test cleanup
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode JSON (status %d): %v", resp.StatusCode, err)
	}
	return m
}

// TestAPI_OpenModes exercises the modes that require no API key.
func TestAPI_OpenModes(t *testing.T) {
	t.Parallel()
	_, ts, _ := buildAPIServer(t)

	tests := []struct {
		name       string
		query      string
		wantStatus int
		checkKey   string
		checkVal   any
	}{
		{
			name:       "version no key",
			query:      "mode=version",
			wantStatus: http.StatusOK,
			checkKey:   "version",
			checkVal:   "integration-test",
		},
		{
			name:       "auth no key",
			query:      "mode=auth",
			wantStatus: http.StatusOK,
			checkKey:   "auth",
			checkVal:   "apikey",
		},
		{
			name:       "auth valid key",
			query:      "mode=auth&apikey=" + integrationAPIKey,
			wantStatus: http.StatusOK,
			checkKey:   "auth",
			checkVal:   "apikey",
		},
		{
			name:       "auth nzb key",
			query:      "mode=auth&apikey=" + integrationNZBKey,
			wantStatus: http.StatusOK,
			checkKey:   "auth",
			checkVal:   "nzbkey",
		},
		{
			name:       "auth bad key",
			query:      "mode=auth&apikey=badkey",
			wantStatus: http.StatusOK,
			checkKey:   "auth",
			checkVal:   "badkey",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := apiDo(t, ts, tt.query) //nolint:bodyclose // closed inside decodeAPIJSON
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d; want %d", resp.StatusCode, tt.wantStatus)
			}
			m := decodeAPIJSON(t, resp)
			if tt.checkKey != "" && m[tt.checkKey] != tt.checkVal {
				t.Errorf("%s = %v; want %v", tt.checkKey, m[tt.checkKey], tt.checkVal)
			}
		})
	}
}

// TestAPI_AuthEnforcement verifies that protected and admin modes reject
// requests without credentials or with insufficient credentials.
func TestAPI_AuthEnforcement(t *testing.T) {
	t.Parallel()
	_, ts, _ := buildAPIServer(t)

	tests := []struct {
		name       string
		query      string
		wantStatus int
	}{
		// Protected mode without key → 401.
		{"queue no key", "mode=queue", http.StatusUnauthorized},
		// Protected mode with NZB key → 200 (NZB key is sufficient for protected).
		{"queue nzb key", "mode=queue&apikey=" + integrationNZBKey, http.StatusOK},
		// Protected mode with API key → 200.
		{"queue api key", "mode=queue&apikey=" + integrationAPIKey, http.StatusOK},
		// Admin mode without key → 401.
		{"pause no key", "mode=pause", http.StatusUnauthorized},
		// Admin mode with NZB key → 403 (NZB key insufficient for admin).
		{"pause nzb key", "mode=pause&apikey=" + integrationNZBKey, http.StatusForbidden},
		// Admin mode with API key → 200.
		{"pause api key", "mode=pause&apikey=" + integrationAPIKey, http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := apiDo(t, ts, tt.query)
			defer func() { _ = resp.Body.Close() }() //nolint:errcheck // test cleanup
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d; want %d (query: %s)", resp.StatusCode, tt.wantStatus, tt.query)
			}
		})
	}
}

// TestAPI_ProtectedModes walks the protected-level modes with a valid API key.
func TestAPI_ProtectedModes(t *testing.T) {
	t.Parallel()
	_, ts, _ := buildAPIServer(t)

	// Each entry is mode=X plus any extra params needed for the mode to
	// return a non-400 response.
	modeQueries := []string{
		"mode=queue",
		"mode=history",
		"mode=status",
		"mode=fullstatus",
		"mode=warnings",
		"mode=server_stats",
		"mode=get_cats",
		"mode=get_scripts",
		"mode=watched_now",
	}

	for _, query := range modeQueries {
		t.Run(query, func(t *testing.T) {
			resp := apiDo(t, ts, query+"&apikey="+integrationAPIKey)
			defer func() { _ = resp.Body.Close() }() //nolint:errcheck // test cleanup
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				t.Errorf("query=%s: unexpected auth failure (status %d)", query, resp.StatusCode)
			}
			// Accept 200, 501 (unimplemented), 500 (missing config).
			if resp.StatusCode != http.StatusOK &&
				resp.StatusCode != http.StatusNotImplemented &&
				resp.StatusCode != http.StatusInternalServerError {
				t.Errorf("query=%s: unexpected status %d", query, resp.StatusCode)
			}
		})
	}
}

// TestAPI_AdminModes walks admin-level modes with a valid API key.
func TestAPI_AdminModes(t *testing.T) {
	t.Parallel()
	_, ts, _ := buildAPIServer(t)

	modes := []string{
		"config", "get_config", "set_config",
		"pause", "resume", "pause_pp", "resume_pp",
		"shutdown", "restart", "disconnect",
	}

	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			resp := apiDo(t, ts, "mode="+mode+"&apikey="+integrationAPIKey)
			defer func() { _ = resp.Body.Close() }() //nolint:errcheck // test cleanup
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				t.Errorf("mode=%s: unexpected auth failure (status %d)", mode, resp.StatusCode)
			}
		})
	}
}

// TestAPI_AddFile tests multipart POST with a canned NZB.
func TestAPI_AddFile(t *testing.T) {
	t.Parallel()
	_, ts, _ := buildAPIServer(t)

	payload := []byte("hello nzb file payload")
	files := []TestFile{{Name: "test.bin", Payload: payload}}
	rawNZB := BuildNZB(files)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("nzbfile", "test.nzb")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(rawNZB); err != nil {
		t.Fatalf("write nzb to form: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	url := ts.URL + "/api?mode=addfile&apikey=" + integrationAPIKey
	req, err := http.NewRequest(http.MethodPost, url, &buf) //nolint:gosec // G107: test URL
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST addfile: %v", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // test cleanup

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("addfile: status %d, body: %s", resp.StatusCode, body)
	}
}

// TestAPI_AddLocalFile tests mode=addlocalfile with a temp NZB file.
func TestAPI_AddLocalFile(t *testing.T) {
	t.Parallel()
	_, ts, dir := buildAPIServer(t)

	payload := []byte("local file nzb test")
	files := []TestFile{{Name: "local.bin", Payload: payload}}
	rawNZB := BuildNZB(files)

	nzbPath := filepath.Join(dir, "local.nzb")
	if err := os.WriteFile(nzbPath, rawNZB, 0o600); err != nil {
		t.Fatalf("write nzb file: %v", err)
	}

	resp := apiDo(t, ts,
		fmt.Sprintf("mode=addlocalfile&name=%s&apikey=%s", nzbPath, integrationAPIKey))
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // test cleanup

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("addlocalfile: status %d, body: %s", resp.StatusCode, body)
	}
}

// TestAPI_AddURL tests mode=addurl with an httptest server serving a canned NZB.
func TestAPI_AddURL(t *testing.T) {
	t.Parallel()
	_, ts, _ := buildAPIServer(t)

	payload := []byte("url nzb test content")
	files := []TestFile{{Name: "url.bin", Payload: payload}}
	rawNZB := BuildNZB(files)

	// Serve the NZB from a local httptest server.
	nzbServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		w.Header().Set("Content-Disposition", `attachment; filename="url.nzb"`)
		_, _ = w.Write(rawNZB) //nolint:errcheck // test handler
	}))
	t.Cleanup(nzbServer.Close)

	nzbURL := nzbServer.URL + "/url.nzb"
	resp := apiDo(t, ts,
		fmt.Sprintf("mode=addurl&name=%s&apikey=%s", nzbURL, integrationAPIKey))
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // test cleanup

	// addurl may return 200 (accepted) or 501 (grabber not wired).
	// Our server wires a grabber but it uses a no-op handler, so the URL
	// will be fetched but the job not added to a real queue. We only care
	// that the auth and dispatch layers work.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Errorf("addurl: unexpected auth failure (status %d)", resp.StatusCode)
	}
}

// TestAPI_MissingMode verifies the error response when mode= is absent.
func TestAPI_MissingMode(t *testing.T) {
	t.Parallel()
	_, ts, _ := buildAPIServer(t)
	resp := apiDo(t, ts, "apikey="+integrationAPIKey)
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // test cleanup
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", resp.StatusCode)
	}
}

// TestAPI_UnknownMode verifies the error response for unrecognized modes.
func TestAPI_UnknownMode(t *testing.T) {
	t.Parallel()
	_, ts, _ := buildAPIServer(t)
	resp := apiDo(t, ts, "mode=doesnotexist&apikey="+integrationAPIKey)
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // test cleanup
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", resp.StatusCode)
	}
}

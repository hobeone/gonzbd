package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/history"
)

// TestHistoryDefault_ScriptLineButNoScriptLog pins the §10 history output for
// a job whose script ran (#908): script_line carries the stored summary, and
// script_log is present but empty, though the log is stored. SABnzbd's
// mode=history returns the line and not the log, and the log can be up to
// 512 KiB per row.
func TestHistoryDefault_ScriptLineButNoScriptLog(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:      "nzo-script",
		Name:       "Movie",
		Status:     "Completed",
		Completed:  time.Now(),
		ScriptLog:  []byte("working\nnotified\n"),
		ScriptLine: "notified",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rr := apiGet(t, s.Handler(), "/api?mode=history&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		History struct {
			Slots []map[string]any `json:"slots"`
		} `json:"history"`
	}
	body := rr.Body.String()
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.History.Slots) != 1 {
		t.Fatalf("slots = %d, want 1; body %s", len(resp.History.Slots), body)
	}
	slot := resp.History.Slots[0]
	if got := slot["script_line"]; got != "notified" {
		t.Errorf("script_line = %v, want %q", got, "notified")
	}
	if got, ok := slot["script_log"]; !ok || got != "" {
		t.Errorf("script_log = %v (present %v), want present and empty", got, ok)
	}
}

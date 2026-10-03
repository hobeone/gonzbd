package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/api/apitest"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestQueueResume_ApprovesAnUnwantedBlockedJob pins that the user's resume
// is the approval: a job the unwanted-extension check paused leaves the
// blocked state when the user resumes it.
func TestQueueResume_ApprovesAnUnwantedBlockedJob(t *testing.T) {
	t.Parallel()
	d := newTestAPIDispatcher(t)
	s := testDispatcherServer(t, d)

	j := job.New("j1", "Blocked", job.Policy{})
	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := d.Add(context.Background(), j, dispatch.Header{Name: "Blocked", Bytes: 1000, Unwanted: unwanted.StateBlocked}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	w := apiGet(t, s.Handler(), "/api?mode=queue&name=resume&value=j1&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	row, ok := d.Row("j1")
	if !ok {
		t.Fatal("j1 is gone")
	}
	if row.Header.Unwanted != unwanted.StateApproved {
		t.Errorf("Unwanted = %v after the user's resume; want StateApproved", row.Header.Unwanted)
	}
	if j.Intent() != job.IntentRun {
		t.Errorf("Intent = %v; want IntentRun", j.Intent())
	}
}

// TestQueueSlot_UnwantedLabel pins the SABnzbd-compatible labels list: a job
// the check flagged carries "UNWANTED", blocked or approved, as SABnzbd's
// labels do for any non-zero unwanted_ext; any other job has an empty list.
func TestQueueSlot_UnwantedLabel(t *testing.T) {
	t.Parallel()
	d := newTestAPIDispatcher(t)
	s := testDispatcherServer(t, d)

	states := map[string]unwanted.State{
		"clean":    unwanted.StateNone,
		"blocked":  unwanted.StateBlocked,
		"approved": unwanted.StateApproved,
	}
	for id, st := range states {
		if err := d.Add(context.Background(), job.New(id, id, job.Policy{}), dispatch.Header{Name: id, Bytes: 1000, Unwanted: st}); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}

	w := apiGet(t, s.Handler(), "/api?mode=queue&apikey="+testAPIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	var resp struct {
		Queue struct {
			Slots []struct {
				NzoID  string    `json:"nzo_id"`
				Labels *[]string `json:"labels"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Queue.Slots) != len(states) {
		t.Fatalf("got %d slots; want %d", len(resp.Queue.Slots), len(states))
	}
	for _, slot := range resp.Queue.Slots {
		if slot.Labels == nil {
			t.Errorf("%s: labels missing or null; want a list", slot.NzoID)
			continue
		}
		want := states[slot.NzoID] != unwanted.StateNone
		if got := slices.Contains(*slot.Labels, "UNWANTED"); got != want {
			t.Errorf("%s: labels = %v; UNWANTED present = %v, want %v", slot.NzoID, *slot.Labels, got, want)
		}
	}
}

// TestHistorySlot_UnwantedExt pins that the history listing reports the
// entry's unwanted-extension state, which the UI's "Retry anyway" reads.
func TestHistorySlot_UnwantedExt(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)
	for i, st := range []unwanted.State{unwanted.StateNone, unwanted.StateBlocked, unwanted.StateApproved} {
		e := history.Entry{
			NzoID:     fmt.Sprintf("nzo_%d", st),
			Name:      fmt.Sprintf("Job%d", i),
			Status:    "Failed",
			Completed: time.Now().Add(-time.Duration(i) * time.Minute),
			Unwanted:  st,
		}
		if err := repo.Add(t.Context(), e, nil); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	rr := apiGet(t, s.Handler(), "/api?mode=history&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var resp struct {
		History struct {
			Slots []struct {
				NzoID       string `json:"nzo_id"`
				UnwantedExt *int   `json:"unwanted_ext"`
			} `json:"slots"`
		} `json:"history"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.History.Slots) != 3 {
		t.Fatalf("got %d slots; want 3", len(resp.History.Slots))
	}
	for _, slot := range resp.History.Slots {
		if slot.UnwantedExt == nil {
			t.Errorf("%s: unwanted_ext missing", slot.NzoID)
			continue
		}
		if want := fmt.Sprintf("nzo_%d", *slot.UnwantedExt); slot.NzoID != want {
			t.Errorf("%s: unwanted_ext = %d", slot.NzoID, *slot.UnwantedExt)
		}
	}
}

// retryRecorder records which retry the handler asked for.
type retryRecorder struct {
	apitest.NopApp
	plain, allowing []string
	err             error
}

func (a *retryRecorder) RetryHistoryJob(_ context.Context, id string) error {
	a.plain = append(a.plain, id)
	return a.err
}

func (a *retryRecorder) RetryHistoryJobAllowingUnwanted(_ context.Context, id string) error {
	a.allowing = append(a.allowing, id)
	return a.err
}

func TestHistoryRetry_AllowUnwanted(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		query        string
		wantAllowing bool
	}{
		{"", false},
		{"&allow_unwanted=0", false},
		{"&allow_unwanted=1", true},
	} {
		t.Run("q="+tc.query, func(t *testing.T) {
			t.Parallel()
			s, repo := testHistoryServer(t)
			rec := &retryRecorder{History: repo}
			s.setAppServices(rec)

			rr := apiGet(t, s.Handler(), "/api?mode=history&name=retry&value=job_1"+tc.query+"&apikey="+testAPIKey)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
			}
			gotAllowing := len(rec.allowing) == 1 && len(rec.plain) == 0
			gotPlain := len(rec.plain) == 1 && len(rec.allowing) == 0
			if tc.wantAllowing && !gotAllowing {
				t.Errorf("plain = %v, allowing = %v; want one allowing retry", rec.plain, rec.allowing)
			}
			if !tc.wantAllowing && !gotPlain {
				t.Errorf("plain = %v, allowing = %v; want one plain retry", rec.plain, rec.allowing)
			}
		})
	}
}

// TestHistoryRetry_UnwantedRefusalIsAConflict pins that a retry the
// unwanted-extension check refuses is reported as the caller's conflict, with
// the reason, rather than as a server fault.
func TestHistoryRetry_UnwantedRefusalIsAConflict(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)
	refusal := fmt.Errorf("app: retry job_1: Aborted, unwanted extension detected: setup.exe: %w", app.ErrUnwantedRefused)
	s.setAppServices(&retryRecorder{History: repo, err: refusal})

	rr := apiGet(t, s.Handler(), "/api?mode=history&name=retry&value=job_1&apikey="+testAPIKey)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	if msg, _ := m["error"].(string); !containsAll(msg, "unwanted extension", "setup.exe") {
		t.Errorf("error = %q; want the refusal's reason", msg)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

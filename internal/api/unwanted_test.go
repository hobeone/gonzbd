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

// TestQueueResume_NZBKeyCannotApprove pins that approval needs the full API
// key: the upload-only NZB key may resume an ordinary job, but resuming a
// blocked one would approve the NZB it just uploaded, so that is refused and
// nothing is resumed.
func TestQueueResume_NZBKeyCannotApprove(t *testing.T) {
	t.Parallel()
	d := newTestAPIDispatcher(t)
	s := testDispatcherServer(t, d)

	add := func(id string, st unwanted.State) *job.Job {
		j := job.New(id, id, job.Policy{})
		if err := j.SetIntent(job.IntentPause); err != nil {
			t.Fatalf("SetIntent: %v", err)
		}
		if err := d.Add(context.Background(), j, dispatch.Header{Name: id, Bytes: 1000, Unwanted: st}); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
		return j
	}
	blocked := add("blocked", unwanted.StateBlocked)
	plain := add("plain", unwanted.StateNone)

	w := apiGet(t, s.Handler(), "/api?mode=queue&name=resume&value=plain,blocked&nzbkey="+testNZBKey)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "blocked") {
		t.Errorf("body = %s; want it to name the blocked job", w.Body.String())
	}
	if row, _ := d.Row("blocked"); row.Header.Unwanted != unwanted.StateBlocked {
		t.Errorf("Unwanted = %v; an NZB-key resume approved the job", row.Header.Unwanted)
	}
	if blocked.Intent() != job.IntentPause || plain.Intent() != job.IntentPause {
		t.Error("a refused request resumed a job")
	}

	w = apiGet(t, s.Handler(), "/api?mode=queue&name=resume&value=plain&nzbkey="+testNZBKey)
	if w.Code != http.StatusOK {
		t.Fatalf("plain resume with the NZB key: status = %d; want 200", w.Code)
	}
	if plain.Intent() != job.IntentRun {
		t.Error("the NZB key can no longer resume an ordinary job")
	}
}

// TestHistoryRetry_NZBKeyCannotAllowUnwanted pins the same rule for retry.
func TestHistoryRetry_NZBKeyCannotAllowUnwanted(t *testing.T) {
	t.Parallel()
	s, repo := testHistoryServer(t)
	rec := &retryRecorder{History: repo}
	s.setAppServices(rec)

	rr := apiGet(t, s.Handler(), "/api?mode=history&name=retry&value=job_1&allow_unwanted=1&nzbkey="+testNZBKey)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 (body: %s)", rr.Code, rr.Body.String())
	}
	if len(rec.plain)+len(rec.allowing) != 0 {
		t.Errorf("plain = %v, allowing = %v; want no retry", rec.plain, rec.allowing)
	}

	rr = apiGet(t, s.Handler(), "/api?mode=history&name=retry&value=job_1&nzbkey="+testNZBKey)
	if rr.Code != http.StatusOK || len(rec.plain) != 1 {
		t.Errorf("plain retry with the NZB key: status = %d, plain = %v; want it to work", rr.Code, rec.plain)
	}
}

// renameRecorder records RenameJob calls and answers with err.
type renameRecorder struct {
	apitest.NopApp
	calls []string
	err   error
}

func (a *renameRecorder) RenameJob(id, name string) (string, error) {
	a.calls = append(a.calls, id+"="+name)
	return name, a.err
}

// TestQueueRename_GoesThroughTheNameOwner pins that the API renames through
// Application.RenameJob, the owner that sanitises and uniquifies the name,
// and reports a refused name as the caller's error.
func TestQueueRename_GoesThroughTheNameOwner(t *testing.T) {
	t.Parallel()
	d := newTestAPIDispatcher(t)
	s := testDispatcherServer(t, d)
	if err := d.Add(context.Background(), job.New("j1", "One", job.Policy{}), dispatch.Header{Name: "One"}); err != nil {
		t.Fatal(err)
	}
	rec := &renameRecorder{}
	s.setAppServices(rec)

	w := apiGet(t, s.Handler(), "/api?mode=queue&name=rename&value=j1&value2=New&apikey="+testAPIKey)
	if w.Code != http.StatusOK || len(rec.calls) != 1 || rec.calls[0] != "j1=New" {
		t.Fatalf("status = %d, calls = %v; want one RenameJob(j1, New)", w.Code, rec.calls)
	}
	if row, _ := d.Row("j1"); row.Header.Name != "One" {
		t.Errorf("the handler wrote the name itself (%q) instead of leaving it to the owner", row.Header.Name)
	}

	for _, refusal := range []error{app.ErrInvalidJobName, fmt.Errorf("x: %w", dispatch.ErrInvalidJobName)} {
		rec.err = refusal
		w = apiGet(t, s.Handler(), "/api?mode=queue&name=rename&value=j1&value2=..&apikey="+testAPIKey)
		if w.Code != http.StatusBadRequest {
			t.Errorf("refusal %v: status = %d; want 400", refusal, w.Code)
		}
	}
	rec.err = fmt.Errorf("x: %w", dispatch.ErrNotFound)
	if w = apiGet(t, s.Handler(), "/api?mode=queue&name=rename&value=nope&value2=x&apikey="+testAPIKey); w.Code != http.StatusNotFound {
		t.Errorf("unknown job: status = %d; want 404", w.Code)
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

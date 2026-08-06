package queue_test

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestMoveToHistory_CarriesFetchPolicy pins the carry, separately from the
// downgrade in TestResetForRetry_DowngradesDiscardedToHeld. RetainedFile
// copies a fixed field list, and a field that is not carried arrives as the
// zero value — FetchAlways — which re-downloads every recovery volume on a
// history retry. That is #323, and it fails silently, so it needs its own
// assertion rather than riding on the downgrade test.
//
// The value asserted is FetchIfNeeded rather than FetchNever because either
// proves the carry: an uncarried field is FetchAlways regardless of which
// non-zero value it held, and FetchIfNeeded is reachable from the public API.
func TestMoveToHistory_CarriesFetchPolicy(t *testing.T) {
	store, _, _ := setupTestStore(t)

	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "content.rar", Bytes: 10_000, Articles: []nzb.Article{{ID: "c1@t", Bytes: 10_000, Number: 1}}},
			{Subject: "content.vol000+01.par2", Bytes: 1_000, Articles: []nzb.Article{{ID: "v1@t", Bytes: 1_000, Number: 1}}},
		},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "par2.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if !job.HasDeferredPar2() {
		t.Fatal("fixture guard: recovery volume not held — nothing is being tested")
	}
	if err := store.Add(t.Context(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	entry := history.Entry{
		NzoID:     job.ID,
		Name:      job.Name,
		Status:    string(constants.StatusFailed),
		Completed: time.Now(),
		TimeAdded: job.Added,
	}
	if err := store.MoveToHistory(t.Context(), job, entry); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}

	retained, err := store.HistoryFileProgress(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("HistoryFileProgress: %v", err)
	}
	if len(retained) != 2 {
		t.Fatalf("retained %d files, want 2", len(retained))
	}
	if got := retained[1].Fetch; got != queue.FetchIfNeeded {
		t.Errorf("retained fetch policy = %d, want FetchIfNeeded — an uncarried field arrives as FetchAlways and re-downloads the volume (#323)", got)
	}
}

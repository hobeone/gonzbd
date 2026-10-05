package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestPersist_RecordsTheDownloadFinishOnlyOutsideFetching: the finish is the
// time a job last left Fetching, so a row at Fetching holds none, and the row
// written once the job has moved on does. A finish persisted at Fetching would
// come back with a restart that dispatches the job to fetch again.
func TestPersist_RecordsTheDownloadFinishOnlyOutsideFetching(t *testing.T) {
	st := &fakeStore{}
	runner := &stateRunner{}
	d := newTestDispatcher(t, withStore(st), withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	j.RestoreProgressState("", time.Unix(1700000100, 0), time.Unix(1700000200, 0), false)
	if got := j.DownloadFinished(); got.IsZero() {
		t.Fatal("fixture: the job carries no finish to persist")
	}
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())

	p, ok := st.row("j1")
	if !ok || p.State.State != job.Fetching {
		t.Fatalf("row = %v, %+v, want one at Fetching", ok, p)
	}
	if p.DownloadFinished != 0 {
		t.Errorf("row at Fetching holds DownloadFinished = %d, want 0", p.DownloadFinished)
	}
	if p.DownloadStarted == 0 {
		t.Error("row at Fetching lost DownloadStarted; only the finish is withheld")
	}

	if err := d.AdvanceFrom(j, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("AdvanceFrom: %v", err)
	}
	// Still at Fetching, with the report recorded as Assessing: the verdict
	// is saved with the finish it carries.
	if err := d.persistIfChanged(context.Background(), j); err != nil {
		t.Fatalf("persistIfChanged: %v", err)
	}
	p, _ = st.row("j1")
	if p.State.State != job.Fetching || p.State.Next != job.Assessing || p.DownloadFinished != 1700000200 {
		t.Errorf("row with the report recorded = state %v next %v finish %d, want Fetching, Assessing, 1700000200",
			p.State.State, p.State.Next, p.DownloadFinished)
	}
	d.tick(context.Background())
	p, _ = st.row("j1")
	if p.State.State != job.Assessing || p.DownloadFinished != 1700000200 {
		t.Errorf("row after the report = state %v finish %d, want Assessing with 1700000200", p.State.State, p.DownloadFinished)
	}
}

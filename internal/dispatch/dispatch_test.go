package dispatch

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

func TestLookup_FindsRegisteredJobsAndReportsMissingOnes(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := d.lookup("j1")
	if !ok || got != j {
		t.Errorf("lookup(j1) = (%v, %v), want (%v, true)", got, ok, j)
	}

	if _, ok := d.lookup("missing"); ok {
		t.Error("lookup(missing) reported ok=true for an ID that was never added")
	}
}

func TestSnapshotOrder_CopiesInQueueOrder(t *testing.T) {
	d := newTestDispatcher(t)
	for _, id := range []string{"c", "a", "b"} {
		if err := d.Add(job.New(id, id, job.Policy{}), Header{Name: id}); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}

	got := d.snapshotOrder()
	want := []string{"c", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("snapshotOrder returned %d jobs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID() != want[i] {
			t.Errorf("job %d is %s, want %s", i, got[i].ID(), want[i])
		}
	}
}

// captureLogger builds a *slog.Logger writing plain text to buf, so a test can
// assert on the record it produced.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestKick_CollapsesABurstIntoOneWakeup(t *testing.T) {
	d := &Dispatcher{wake: make(chan struct{}, 1)}

	d.kick()
	d.kick()
	d.kick()

	select {
	case <-d.wake:
	default:
		t.Fatal("kick never sent on wake — the ticker would never see a wakeup")
	}
	select {
	case <-d.wake:
		t.Fatal("wake carried a second pending wakeup — kick must collapse a burst into one")
	default:
	}
}

func TestLogHelpers_RecordTheJobIDAndTheError(t *testing.T) {
	wantErr := errors.New("boom")

	tests := []struct {
		name string
		call func(d *Dispatcher)
		want string
	}{
		{"logAdvanceError", func(d *Dispatcher) { d.logAdvanceError("j1", wantErr) }, "advance failed"},
		{"logResidencyError", func(d *Dispatcher) { d.logResidencyError("j1", wantErr) }, "residency reconcile failed"},
		{"logStoreError", func(d *Dispatcher) { d.logStoreError("j1", wantErr) }, "store write failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			d := &Dispatcher{log: captureLogger(&buf)}
			tc.call(d)

			out := buf.String()
			if !strings.Contains(out, tc.want) {
				t.Errorf("log output %q does not contain %q", out, tc.want)
			}
			if !strings.Contains(out, "job_id=j1") {
				t.Errorf("log output %q does not contain the job ID", out)
			}
			if !strings.Contains(out, "boom") {
				t.Errorf("log output %q does not contain the error text", out)
			}
		})
	}
}

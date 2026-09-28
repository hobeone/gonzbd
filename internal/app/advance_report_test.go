package app

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestCompleteFinalizedFile_ReportsFetchingToAssessing pins the download's
// exit report: the last file's completion records Assessing for a job still
// at Fetching, and a repeat for a job that has since moved on leaves it alone.
func TestCompleteFinalizedFile_ReportsFetchingToAssessing(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 1)
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}

	if err := application.completeFinalizedFile(t.Context(), FileComplete{JobID: j.ID(), FileIdx: 0}); err != nil {
		t.Fatalf("completeFinalizedFile: %v", err)
	}
	if got := j.Snapshot().State; got.State != job.Fetching || got.Next != job.Assessing {
		t.Fatalf("after the last file: %+v, want Fetching with next Assessing", got)
	}

	if err := j.Transition(job.Assessing); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if err := application.completeFinalizedFile(t.Context(), FileComplete{JobID: j.ID(), FileIdx: 0}); err != nil {
		t.Fatalf("repeated completeFinalizedFile: %v", err)
	}
	if got := j.Snapshot().State; got.State != job.Assessing || got.Next != job.StateUnset {
		t.Errorf("a repeated report moved a job that had left Fetching: %+v", got)
	}
}

type errReporter struct {
	reportRecorder
	err error
}

func (r *errReporter) AdvanceFrom(j *job.Job, from, next job.State) error {
	_ = r.reportRecorder.AdvanceFrom(j, from, next)
	return r.err
}

// TestAppRunner_AdvanceLogsByCause pins advance's logging: a verdict refused
// because the job was cancelled or removed is expected and logged at Debug,
// any other refusal at Warn, and a runner with no reporter reports nothing.
func TestAppRunner_AdvanceLogsByCause(t *testing.T) {
	j := job.New("j1", "n", job.Policy{})
	tests := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("wrapped: %w", dispatch.ErrStaleReport), "level=DEBUG"},
		{fmt.Errorf("wrapped: %w", dispatch.ErrNotFound), "level=DEBUG"},
		{errors.New("boom"), "level=WARN"},
	}
	for _, tc := range tests {
		var logs bytes.Buffer
		rec := &errReporter{err: tc.err}
		r := &appRunner{report: rec, log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
		r.advance(j, job.Extracting)
		if got, want := rec.advance(j.ID()), [2]job.State{job.Assessing, job.Extracting}; got != want {
			t.Errorf("err %v: reported %v, want %v", tc.err, got, want)
		}
		if tc.want == "" {
			if logs.Len() != 0 {
				t.Errorf("err %v: logged %q, want nothing", tc.err, logs.String())
			}
		} else if !strings.Contains(logs.String(), tc.want) {
			t.Errorf("err %v: logged %q, want %s", tc.err, logs.String(), tc.want)
		}
	}

	nilReport := &appRunner{log: slog.New(slog.DiscardHandler)}
	nilReport.advance(j, job.Extracting) // must not panic
}

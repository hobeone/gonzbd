package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// completeFlagAtReport reads the job's file-0 complete flag from SQLite each
// time Fetching -> Assessing is reported, which is the moment the dispatcher's
// next tick may persist the new position.
type completeFlagAtReport struct {
	reporter
	read  func() (int, bool)
	flags []int
}

func (r *completeFlagAtReport) AdvanceFrom(j *job.Job, from, next job.State) error {
	if from == job.Fetching && next == job.Assessing {
		if v, ok := r.read(); ok {
			r.flags = append(r.flags, v)
		}
	}
	return r.reporter.AdvanceFrom(j, from, next)
}

// TestReportDownloadComplete_RecordsTheFileBeforeTheReport: a crash after the
// dispatcher persists Assessing but before the recorder's next flush restores
// the job past Fetching with its files unrecorded, and nothing re-checks
// completeness there (#962). So the file's complete flag must be in SQLite
// before the report that lets the position be persisted.
func TestReportDownloadComplete_RecordsTheFileBeforeTheReport(t *testing.T) {
	t.Parallel()
	f := newFailAtAssessingFixture(t)
	repo := f.app.historyRepo

	// Admit seeds the job_files row the flush's UPDATE lands on; the fixture
	// adds its job to the dispatcher directly, so it seeds the row itself.
	if err := durability.NewStore(repo.DB()).Admit(t.Context(), f.j.ID(), []uint8{uint8(job.FetchAlways)}); err != nil {
		t.Fatalf("Admit: %v", err)
	}

	// The state completeFinalizedFile leaves: marked complete (the fixture did
	// that) and handed to the recorder, which has not flushed.
	f.app.markFileDirty(f.j, 0)

	spy := &completeFlagAtReport{
		reporter: f.d,
		read: func() (int, bool) {
			var c int
			err := repo.DB().QueryRowContext(t.Context(),
				`SELECT complete FROM job_files WHERE job_id = ? AND file_index = 0`, f.j.ID()).Scan(&c)
			return c, err == nil
		},
	}
	if reported, err := f.app.reportDownloadComplete(f.j, spy); !reported || err != nil {
		t.Fatalf("reportDownloadComplete = (%v, %v), want (true, nil)", reported, err)
	}
	if len(spy.flags) != 1 || spy.flags[0] != 1 {
		t.Fatalf("job_files.complete read at the Fetching->Assessing report = %v (an empty list means no row yet), want [1]", spy.flags)
	}
}

// TestReportDownloadComplete_ReportsDespiteAFailedFlush: a flush that fails is
// logged and the report still goes out. No later report is owed, so holding
// the job at Fetching would strand its lease.
func TestReportDownloadComplete_ReportsDespiteAFailedFlush(t *testing.T) {
	t.Parallel()
	f := newFailAtAssessingFixture(t)
	f.app.markFileDirty(f.j, 0)
	f.app.recorder.st = failRecordFor{recordStore: f.app.recorder.st, id: f.j.ID()}

	spy := &completeFlagAtReport{reporter: f.d, read: func() (int, bool) { return 0, true }}
	reported, err := f.app.reportDownloadComplete(f.j, spy)
	if !reported || err != nil {
		t.Fatalf("reportDownloadComplete with a failing flush = (%v, %v), want (true, nil)", reported, err)
	}
	if len(spy.flags) != 1 {
		t.Fatalf("Fetching->Assessing reports made = %d, want 1", len(spy.flags))
	}
}

package durability

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePartial creates a partial file of n bytes and returns its path.
func writePartial(t *testing.T, dir, name string, n int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	return path
}

// storeRun commits one article as a run and returns the store.
func storeRuns(t *testing.T, rs RunStore, jobID string, arts ...DurableArticle) {
	t.Helper()
	if _, err := rs.Commit(context.Background(), jobID, arts); err != nil {
		t.Fatalf("commit runs: %v", err)
	}
}

// TestResume_AdoptsWhenTheFileIsLongEnough pins §3.4's gate in the direction
// that costs nothing: the record is authoritative and the whole check is one
// stat, so a file at least as long as its runs claim is adopted with no read.
//
// A file that is LONGER is the ordinary pre-allocated case and must adopt too,
// which is the second subtest.
func TestResume_AdoptsWhenTheFileIsLongEnough(t *testing.T) {
	tests := []struct {
		name string
		size int
	}{
		{"exactly the recorded bound", 300},
		{"longer, which is pre-allocation", 4096},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			path := writePartial(t, dir, "f.bin", tt.size)
			rs := NewSQLiteRunStore(openTestDB(t))
			storeRuns(t, rs, "job-1",
				DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
				DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 200, CRC32: 2},
			)
			r := NewResumer(rs, testLogger(t))

			res, err := r.Resume(ctx, "job-1", 0, path)
			if err != nil {
				t.Fatal(err)
			}
			if res.Restart {
				t.Fatal("Restart set for a file at least as long as its runs claim")
			}
			if len(res.Runs) != 1 {
				t.Fatalf("adopted %d runs, want the one merged run: %+v", len(res.Runs), res.Runs)
			}
			if res.Runs[0].FirstArtIdx != 0 || res.Runs[0].LastArtIdx != 1 {
				t.Errorf("adopted run covers [%d,%d], want [0,1]",
					res.Runs[0].FirstArtIdx, res.Runs[0].LastArtIdx)
			}
			if res.Size != int64(tt.size) {
				t.Errorf("Size = %d, want %d", res.Size, tt.size)
			}
			// Nothing was deleted: the record is still there for the next start.
			stored, err := rs.ForFile(ctx, "job-1", 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(stored) != 1 {
				t.Errorf("the store holds %d runs after an ADOPT, want 1 — a resume that "+
					"adopts must not mutate anything", len(stored))
			}
		})
	}
}

// TestResume_ShortFileDiscardsItsRuns pins the half the gate exists for. A
// partial replaced or truncated between runs would otherwise have most of its
// articles reported complete, and the file would finish with holes exactly
// where those articles should have been.
//
// The runs must be DELETED, not merely withheld from the return: the caller
// installs the result into the live queue, but the next restart reads the
// store directly, so a withheld-but-stored run comes back.
func TestResume_ShortFileDiscardsItsRuns(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := writePartial(t, dir, "f.bin", 299)
	rs := NewSQLiteRunStore(openTestDB(t))
	storeRuns(t, rs, "job-1",
		DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
		DurableArticle{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 200, CRC32: 2},
	)
	r := NewResumer(rs, testLogger(t))

	res, err := r.Resume(ctx, "job-1", 0, path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Restart {
		t.Error("Restart not set for a file one byte shorter than its runs claim")
	}
	if len(res.Runs) != 0 {
		t.Errorf("returned %d runs for a disproved file, want none: %+v", len(res.Runs), res.Runs)
	}
	stored, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Errorf("the store still holds %d runs for a file that disproved them; the next "+
			"restart adopts them and the file finishes with holes", len(stored))
	}
}

// TestResume_DiscardIsScopedToTheFile pins that a disproved file takes only
// its OWN runs with it. A resume stat'ed one path and proved nothing about
// the job's other files; deleting theirs would turn one replaced partial into
// a full re-download of the job.
func TestResume_DiscardIsScopedToTheFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := writePartial(t, dir, "f0.bin", 10)
	rs := NewSQLiteRunStore(openTestDB(t))
	storeRuns(t, rs, "job-1",
		DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
		DurableArticle{FileIdx: 1, ArtIdx: 5, Offset: 0, Length: 100, CRC32: 2},
	)
	r := NewResumer(rs, testLogger(t))

	if _, err := r.Resume(ctx, "job-1", 0, path); err != nil {
		t.Fatal(err)
	}
	if got, err := rs.ForFile(ctx, "job-1", 0); err != nil || len(got) != 0 {
		t.Errorf("file 0 kept %v (err %v), want its runs discarded", got, err)
	}
	got, err := rs.ForFile(ctx, "job-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("file 1 holds %d runs, want 1 — the resume never looked at it", len(got))
	}
}

// TestResume_MissingFileRestarts pins that a deleted partial starts over, and
// that the deletion reaches the STORE.
//
// A missing file is the strongest possible disproof of every run recorded for
// it. Returning Restart alone would leave the rows armed: the assembler
// recreates the file, the next barrier records over them, and the next start
// adopts articles this process never wrote.
func TestResume_MissingFileRestarts(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))
	storeRuns(t, rs, "job-1",
		DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1})
	r := NewResumer(rs, testLogger(t))

	res, err := r.Resume(ctx, "job-1", 0, filepath.Join(t.TempDir(), "gone.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Restart {
		t.Error("Restart not set for a missing file")
	}
	if len(res.Runs) != 0 {
		t.Errorf("returned %d runs for a missing file", len(res.Runs))
	}
	if got, _ := rs.ForFile(ctx, "job-1", 0); len(got) != 0 {
		t.Errorf("the store still holds %d runs for a file that is gone", len(got))
	}
}

// TestResume_FileWithNoRunsAdopts pins the ordinary first-start case: a file
// with nothing recorded has a bound of 0, which every size satisfies, so it
// adopts an empty set rather than reporting Restart.
//
// The distinction is not cosmetic. Restart is what the caller reads as "this
// file was disproved"; a file nobody ever wrote a run for was not disproved.
func TestResume_FileWithNoRunsAdopts(t *testing.T) {
	ctx := context.Background()
	path := writePartial(t, t.TempDir(), "f.bin", 0)
	r := NewResumer(NewSQLiteRunStore(openTestDB(t)), testLogger(t))

	res, err := r.Resume(ctx, "job-1", 0, path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restart {
		t.Error("Restart set for a file that never had a run recorded for it")
	}
	if len(res.Runs) != 0 {
		t.Errorf("returned %d runs, want none", len(res.Runs))
	}
}

// TestResume_StatErrorIsReturned pins that a stat failure other than "not
// exists" is surfaced rather than read as absence. Reading an EACCES as a
// missing file would discard the runs of a partial whose bytes are still
// there (A2).
func TestResume_StatErrorIsReturned(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sub := filepath.Join(dir, "locked")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	path := writePartial(t, sub, "f.bin", 100)
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o750) })
	if os.Geteuid() == 0 {
		t.Skip("running as root: the directory mode does not deny the stat")
	}

	rs := NewSQLiteRunStore(openTestDB(t))
	storeRuns(t, rs, "job-1",
		DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1})
	r := NewResumer(rs, testLogger(t))

	if _, err := r.Resume(ctx, "job-1", 0, path); err == nil {
		t.Fatal("Resume returned nil for an unreadable directory")
	}
	if got, _ := rs.ForFile(ctx, "job-1", 0); len(got) != 1 {
		t.Errorf("the runs were discarded on a stat error; %d remain, want 1", len(got))
	}
}

// errRunStore fails ForFile so the read-failure path can be pinned.
type errRunStore struct {
	RunStore
	err error
}

func (e *errRunStore) ForFile(context.Context, string, int32) ([]Run, error) {
	return nil, e.err
}

// TestResume_RunReadFailureIsReturned pins that an unreadable store stops the
// resume rather than yielding an empty run set — which would read as "nothing
// is recorded" and re-download an intact file.
func TestResume_RunReadFailureIsReturned(t *testing.T) {
	boom := errors.New("run store unreadable")
	path := writePartial(t, t.TempDir(), "f.bin", 100)
	r := NewResumer(&errRunStore{RunStore: NewSQLiteRunStore(openTestDB(t)), err: boom}, testLogger(t))

	if _, err := r.Resume(context.Background(), "job-1", 0, path); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the read failure", err)
	}
}

// delErrStore fails DeleteFile so the discard's own failure can be pinned.
type delErrStore struct {
	RunStore
	err error
}

func (d *delErrStore) DeleteFile(context.Context, string, int32) error { return d.err }

// TestResume_SurfacesADiscardFailure pins that a discard which could not be
// made durable is REPORTED rather than swallowed.
//
// Reporting success would let the sweep move on believing the disproof is
// recorded. It is not: the next start reads the store, adopts the runs the
// file has already contradicted, and finishes it with holes.
func TestResume_SurfacesADiscardFailure(t *testing.T) {
	boom := errors.New("delete rejected")
	rs := &delErrStore{RunStore: NewSQLiteRunStore(openTestDB(t)), err: boom}
	storeRuns(t, rs.RunStore, "job-1",
		DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1})
	r := NewResumer(rs, testLogger(t))

	_, err := r.Resume(context.Background(), "job-1", 0, filepath.Join(t.TempDir(), "gone.bin"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the discard failure", err)
	}
}

// TestDiscard_NamesTheJobAndFile pins the one thing this wrapper adds over the
// store's own DeleteFile: an error naming which file's disproof was lost.
//
// It is the only mutation Resumer performs, so its failure is the only way the
// gate's decision can fail to reach stable storage. A bare "delete failed"
// would leave the operator, and the sweep's own log line, unable to say which
// partial the next start is about to adopt anyway.
func TestDiscard_NamesTheJobAndFile(t *testing.T) {
	boom := errors.New("delete rejected")
	r := NewResumer(&delErrStore{RunStore: NewSQLiteRunStore(openTestDB(t)), err: boom}, testLogger(t))

	err := r.discard(context.Background(), "job-7", 3)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the store failure", err)
	}
	for _, want := range []string{"job=job-7", "file=3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	}

	// The success path returns nil rather than an error built from a nil
	// cause, which a naive wrap would produce.
	ok := NewResumer(NewSQLiteRunStore(openTestDB(t)), testLogger(t))
	if err := ok.discard(context.Background(), "job-7", 3); err != nil {
		t.Errorf("discarding a file with no runs returned %v, want nil", err)
	}
}

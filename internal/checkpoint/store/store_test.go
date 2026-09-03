package store_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/checkpoint/store"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
)

// newTestStore opens a real migrated database. The point of these tests is the
// driver's and the schema's behaviour — foreign keys, the UPDATE's affected-row
// count, the actual column set — so an in-memory double would test nothing that
// checkpoint's own fake store does not already cover.
func newTestStore(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	db, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	raw := history.NewRepository(db).DB()
	return store.New(raw), raw
}

// seedRows writes the jobs row and the two job_files rows this package updates
// but does not create.
//
// It is written here in raw SQL deliberately: the code that inserts these rows
// in production lived in internal/queue and has no successor yet, so there is
// nothing to call. Doing it by hand is also what makes the ErrNoRow test below
// meaningful — a store that inserted on demand would pass every other test
// here identically.
func seedRows(t *testing.T, db *sql.DB, jobID string, files int) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO jobs (id, filename, name, priority, status, pp, time_added, md5, avg_age, sort_key)
		 VALUES (?, 'x.nzb', 'x', 0, 'Queued', 3, 0, '', 0, 0)`, jobID); err != nil {
		t.Fatalf("seed jobs row: %v", err)
	}
	for fi := range files {
		if _, err := db.ExecContext(t.Context(),
			`INSERT INTO job_files (job_id, file_index, subject, date, bytes, article_count)
			 VALUES (?, ?, 's', 0, 0, 0)`, jobID, fi); err != nil {
			t.Fatalf("seed job_files row %d: %v", fi, err)
		}
	}
}

// storedFile is one job_files row's mutable half, as SaveBatch writes it.
type storedFile struct {
	Complete        bool
	Fetch           int
	Filename        string
	AssembledCRC32  uint32
	FailedBytes     int64
	BytesDownloaded int64
}

func readFile(t *testing.T, db *sql.DB, jobID string, fi int) storedFile {
	t.Helper()
	var got storedFile
	// filename is nullable and seedRows leaves it NULL, which is exactly the
	// state a row is in before any assembly has resolved a name. Scanning it
	// through NullString rather than making the column non-null keeps the
	// fixture honest about what a freshly inserted row looks like.
	var filename sql.NullString
	err := db.QueryRowContext(t.Context(),
		`SELECT complete, fetch_policy, filename, assembled_crc32, failed_bytes, bytes_downloaded
		 FROM job_files WHERE job_id = ? AND file_index = ?`, jobID, fi).
		Scan(&got.Complete, &got.Fetch, &filename, &got.AssembledCRC32,
			&got.FailedBytes, &got.BytesDownloaded)
	if err != nil {
		t.Fatalf("read job_files %s/%d: %v", jobID, fi, err)
	}
	got.Filename = filename.String
	return got
}

// residentJob builds a two-file job with content attached: file 0 holds
// articles 0-1 at 100 bytes each, file 1 holds article 2 at 50.
func residentJob(t *testing.T, id string) *job.Job {
	t.Helper()
	j := job.New(id, "test", job.PolicyFromPP(3))
	m := job.NewManifest([]job.JobFile{
		{
			Subject: "big.rar", Bytes: 200,
			Articles: []job.JobArticle{{ID: "a0@x", Bytes: 100}, {ID: "a1@x", Bytes: 100}},
		},
		{
			Subject: "big.vol000+01.par2", Bytes: 50, IsPar2Recovery: true,
			Articles: []job.JobArticle{{ID: "p0@x", Bytes: 50}},
		},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	return j
}

// TestSaveBatch_WritesTheByteFiguresAndTheCompleteFlag drives the progress
// record through the doors that produce those figures — a durable run and a
// permanent failure — rather than assigning them, so the columns are checked
// against state the accounting actually produces.
func TestSaveBatch_WritesTheByteFiguresAndTheCompleteFlag(t *testing.T) {
	s, db := newTestStore(t)
	seedRows(t, db, "j1", 2)

	j := residentJob(t, "j1")
	if err := j.SeedFromRuns([]durability.Run{{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0}}); err != nil {
		t.Fatalf("SeedFromRuns: %v", err)
	}
	if _, err := j.AckPermanentFailure([]int32{1}); err != nil {
		t.Fatalf("AckPermanentFailure: %v", err)
	}
	if err := j.MarkFileComplete(0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}

	if err := s.SaveBatch(t.Context(), []job.Checkpoint{j.Checkpoint()}); err != nil {
		t.Fatalf("SaveBatch: %v", err)
	}

	got := readFile(t, db, "j1", 0)
	want := storedFile{Complete: true, BytesDownloaded: 100, FailedBytes: 100}
	if got != want {
		t.Fatalf("file 0 = %+v, want %+v", got, want)
	}
	if got := readFile(t, db, "j1", 1); got != (storedFile{}) {
		t.Fatalf("file 1 = %+v, want the zero row: nothing touched it", got)
	}
}

// TestSaveBatch_WritesTheAssemblyColumns covers the three columns no exported
// mutator on *job.Job can set on this branch — the assembler's filename and
// CRC, and the on-demand par2 fetch policy. They are installed through
// RestoreContent, which is the door a hydration uses, so what is checked is a
// real path rather than a test-only reach into the record.
func TestSaveBatch_WritesTheAssemblyColumns(t *testing.T) {
	s, db := newTestStore(t)
	seedRows(t, db, "j1", 2)

	j := residentJob(t, "j1")
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	var p job.JobProgress
	blob := `{"done":[false,false,false],"failed":[false,false,false],"files":[
		{"complete":true,"filename":"big.rar","assembled_crc32":3735928559},
		{"fetch_policy":1}]}`
	if err := json.Unmarshal([]byte(blob), &p); err != nil {
		t.Fatalf("Unmarshal progress: %v", err)
	}
	if err := j.RestoreContent(m, &p); err != nil {
		t.Fatalf("RestoreContent: %v", err)
	}

	if err := s.SaveBatch(t.Context(), []job.Checkpoint{j.Checkpoint()}); err != nil {
		t.Fatalf("SaveBatch: %v", err)
	}

	got := readFile(t, db, "j1", 0)
	want := storedFile{Complete: true, Filename: "big.rar", AssembledCRC32: 0xdeadbeef}
	if got != want {
		t.Fatalf("file 0 = %+v, want %+v", got, want)
	}
	if got, want := readFile(t, db, "j1", 1).Fetch, int(job.FetchIfNeeded); got != want {
		t.Fatalf("file 1 fetch_policy = %d, want %d", got, want)
	}
}

// TestSaveBatch_ReportsAMissingRow pins the refusal. A checkpoint with no row
// behind it went nowhere, and the next hydration would read a Complete flag and
// a CRC from whatever is on disk — the direction that finalizes a file over
// bytes that are not there.
func TestSaveBatch_ReportsAMissingRow(t *testing.T) {
	s, db := newTestStore(t)
	seedRows(t, db, "j1", 1) // one row, two files in the manifest

	j := residentJob(t, "j1")
	err := s.SaveBatch(t.Context(), []job.Checkpoint{j.Checkpoint()})
	if !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("SaveBatch with a missing row = %v, want ErrNoRow", err)
	}
}

// TestSaveBatch_RollsBackTheWholeBatch pins atomicity. A partial batch is
// indistinguishable on the next read from a complete one, because the rows
// carry no generation — so a failure must leave nothing behind for
// Checkpointer.Flush to re-drive over.
func TestSaveBatch_RollsBackTheWholeBatch(t *testing.T) {
	s, db := newTestStore(t)
	seedRows(t, db, "good", 2)
	seedRows(t, db, "bad", 1) // one row short of its manifest

	good := residentJob(t, "good")
	if err := good.MarkFileComplete(0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	bad := residentJob(t, "bad")

	err := s.SaveBatch(t.Context(), []job.Checkpoint{good.Checkpoint(), bad.Checkpoint()})
	// The rollback is asserted BEFORE the error, and with Errorf rather than
	// Fatalf, so that a mutation which stops a per-job failure aborting the
	// batch is reported against atomicity rather than against the error value
	// — the two failures are different findings and the ordering is what keeps
	// them distinguishable. See testdata/rollback.spec.
	if readFile(t, db, "good", 0).Complete {
		t.Error("the good job's write survived a failed batch; the transaction did not roll back")
	}
	if !errors.Is(err, store.ErrNoRow) {
		t.Errorf("SaveBatch = %v, want ErrNoRow", err)
	}
}

// TestSaveBatch_SkipsAJobWithNoContent covers the nil-Progress case: a job that
// has been admitted but has no manifest attached yet has nothing to write, and
// that is not a missing row.
func TestSaveBatch_SkipsAJobWithNoContent(t *testing.T) {
	s, db := newTestStore(t)
	seedRows(t, db, "j1", 1)

	j := job.New("j1", "test", job.PolicyFromPP(3))
	if err := s.SaveBatch(t.Context(), []job.Checkpoint{j.Checkpoint()}); err != nil {
		t.Fatalf("SaveBatch: %v", err)
	}
	if got := readFile(t, db, "j1", 0); got != (storedFile{}) {
		t.Fatalf("file 0 = %+v, want the zero row", got)
	}
}

// TestSaveBatch_EmptyIsANoOp covers the early return, which is what keeps the
// Checkpointer's idle ticks from opening a transaction each time.
func TestSaveBatch_EmptyIsANoOp(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.SaveBatch(t.Context(), nil); err != nil {
		t.Fatalf("SaveBatch(nil) = %v, want nil", err)
	}
}

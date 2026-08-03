package queue

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"
)

func newWriteTxStore(t *testing.T) (*SQLiteStore, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	return NewSQLiteStore(repo.DB(), dir, repo), dir
}

// TestWithWriteTx_CommitsOnSuccess pins that work done inside fn is durable
// once withWriteTx returns nil. The helper owns the commit, so a caller has
// no other way to know its writes landed.
func TestWithWriteTx_CommitsOnSuccess(t *testing.T) {
	store, _ := newWriteTxStore(t)

	err := store.withWriteTx(t.Context(), func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(t.Context(),
			"INSERT INTO queue_meta (key, value) VALUES ('committed', 'yes')")
		return execErr
	})
	if err != nil {
		t.Fatalf("withWriteTx: %v", err)
	}

	var got string
	if err := store.db.QueryRowContext(t.Context(),
		"SELECT value FROM queue_meta WHERE key = 'committed'").Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != "yes" {
		t.Errorf("value = %q, want %q", got, "yes")
	}
}

// TestWithWriteTx_RollsBackAndDoesNotRetryOnOtherErrors pins the two halves
// of the failure path: fn's work is discarded, and the helper does not run it
// again.
//
// Retrying anything other than contention would be wrong twice over — the
// same transaction would fail the same way, and a caller returning a sentinel
// like ErrNotFound would have its non-idempotent work replayed on the way to
// the same answer.
func TestWithWriteTx_RollsBackAndDoesNotRetryOnOtherErrors(t *testing.T) {
	store, _ := newWriteTxStore(t)
	sentinel := errors.New("caller's own failure")

	attempts := 0
	err := store.withWriteTx(t.Context(), func(tx *sql.Tx) error {
		attempts++
		if _, execErr := tx.ExecContext(t.Context(),
			"INSERT INTO queue_meta (key, value) VALUES ('rolled-back', 'yes')"); execErr != nil {
			return execErr
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("withWriteTx = %v, want the caller's error unwrapped", err)
	}
	if attempts != 1 {
		t.Errorf("fn ran %d times for a non-contention error, want 1", attempts)
	}

	var n int
	if err := store.db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM queue_meta WHERE key = 'rolled-back'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d rows survived a failed transaction, want 0", n)
	}
}

// TestWithWriteTx_ReportsBeginFailure pins that a database which cannot open
// a transaction at all is reported rather than retried into the timeout.
func TestWithWriteTx_ReportsBeginFailure(t *testing.T) {
	store, _ := newWriteTxStore(t)
	if err := store.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ran := false
	err := store.withWriteTx(t.Context(), func(*sql.Tx) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("withWriteTx succeeded against a closed database")
	}
	if ran {
		t.Error("fn ran although the transaction never opened")
	}
}

// TestWithWriteTx_HonoursContextCancellation pins that a cancelled context
// stops the helper rather than working through its retry budget.
func TestWithWriteTx_HonoursContextCancellation(t *testing.T) {
	store, _ := newWriteTxStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := store.withWriteTx(ctx, func(*sql.Tx) error { return nil }); err == nil {
		t.Fatal("withWriteTx succeeded with a cancelled context")
	}
}

// TestIsBusyErr pins the classification that decides whether a transaction is
// worth restarting. A false positive replays work that will fail again; a
// false negative surfaces a transient lock to the user as a hard failure.
func TestIsBusyErr(t *testing.T) {
	if isBusyErr(nil) {
		t.Error("nil classified as busy")
	}
	if isBusyErr(errors.New("database is locked")) {
		t.Error("a plain error with a busy-looking message classified as busy; " +
			"classification must come from the result code, not the text")
	}

	// A real driver error that is emphatically not contention.
	store, _ := newWriteTxStore(t)
	ctx := t.Context()
	if _, err := store.db.ExecContext(ctx,
		"INSERT INTO queue_meta (key, value) VALUES ('dup', 'a')"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, dupErr := store.db.ExecContext(ctx,
		"INSERT INTO queue_meta (key, value) VALUES ('dup', 'b')")
	if dupErr == nil {
		t.Fatal("expected a primary-key violation")
	}
	if isBusyErr(dupErr) {
		t.Errorf("constraint violation %v classified as busy; it would be retried forever", dupErr)
	}
}

// TestAddTx_InsertsJobAndFiles pins Add's transactional half in isolation
// from the manifest write its caller performs, which is the split that keeps
// the transaction safe to run again after contention.
func TestAddTx_InsertsJobAndFiles(t *testing.T) {
	store, _ := newWriteTxStore(t)
	ctx := t.Context()

	job := makeMultiFileJob(t, "addtx", 2, 3)
	m := job.manifest

	if err := store.withWriteTx(ctx, func(tx *sql.Tx) error {
		return store.addTx(ctx, tx, job, m, true)
	}); err != nil {
		t.Fatalf("addTx: %v", err)
	}

	var files int
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM job_files WHERE job_id = ?", job.ID).Scan(&files); err != nil {
		t.Fatalf("count job_files: %v", err)
	}
	if files != 2 {
		t.Errorf("job_files rows = %d, want 2", files)
	}

	// hasManifest=false must insert the job row and no file rows: that is
	// how a job persisted while its manifest is evicted is stored.
	bare := makeMultiFileJob(t, "addtx-bare", 2, 3)
	if err := store.withWriteTx(ctx, func(tx *sql.Tx) error {
		return store.addTx(ctx, tx, bare, nil, false)
	}); err != nil {
		t.Fatalf("addTx without manifest: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM job_files WHERE job_id = ?", bare.ID).Scan(&files); err != nil {
		t.Fatalf("count job_files (bare): %v", err)
	}
	if files != 0 {
		t.Errorf("job_files rows for a manifest-less job = %d, want 0", files)
	}
}

// TestShiftSortKeyTx_ReordersAndReportsMissing pins the other transaction
// split out for retry: the reorder itself, and the not-found case that must
// travel out through withWriteTx unretried.
func TestShiftSortKeyTx_ReordersAndReportsMissing(t *testing.T) {
	store, _ := newWriteTxStore(t)
	ctx := t.Context()

	ids := []string{"shift00000000001", "shift00000000002", "shift00000000003"}
	for i, id := range ids {
		job := makeMultiFileJob(t, "shift", 1, 1)
		job.ID = id
		job.Name = "shift-" + id
		if err := store.Add(ctx, job); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	if err := store.withWriteTx(ctx, func(tx *sql.Tx) error {
		return store.shiftSortKeyTx(ctx, tx, ids[2], 0)
	}); err != nil {
		t.Fatalf("shiftSortKeyTx: %v", err)
	}

	rows, err := store.db.QueryContext(ctx, "SELECT id FROM jobs ORDER BY sort_key ASC")
	if err != nil {
		t.Fatalf("query order: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		got = append(got, id)
	}
	want := []string{ids[2], ids[0], ids[1]}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}

	err = store.withWriteTx(ctx, func(tx *sql.Tx) error {
		return store.shiftSortKeyTx(ctx, tx, "absent0000000001", 0)
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("shift of an unknown job = %v, want ErrNotFound", err)
	}
}

// TestWithWriteTx_RetriesThenGivesUpOnRealContention pins the retry loop
// itself against a genuine SQLITE_BUSY rather than a synthesised one — the
// driver's error type has no exported constructor, so the only honest way to
// produce one is to actually contend for the write lock.
//
// A second pool with a 1ms busy_timeout stands in for a slow one with the
// production 5s: the classification and the retry budget are the same, only
// the wait is short enough to run in a test.
func TestWithWriteTx_RetriesThenGivesUpOnRealContention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.db")

	// Create the schema through the normal path.
	seed, err := history.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = seed.Close() })

	// A holder that takes the write lock and keeps it for the duration.
	holder, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	holdTx, err := holder.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	if _, err := holdTx.ExecContext(t.Context(),
		"INSERT INTO queue_meta (key, value) VALUES ('held', '1')"); err != nil {
		t.Fatalf("holder write: %v", err)
	}
	defer func() { _ = holdTx.Rollback() }()

	// The contender, impatient enough to fail fast.
	contender, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(1)&_txlock=immediate")
	if err != nil {
		t.Fatalf("open contender: %v", err)
	}
	t.Cleanup(func() { _ = contender.Close() })
	store := NewSQLiteStore(contender, dir, nil)

	attempts := 0
	err = store.withWriteTx(t.Context(), func(tx *sql.Tx) error {
		attempts++
		_, execErr := tx.ExecContext(t.Context(),
			"INSERT INTO queue_meta (key, value) VALUES ('contended', '1')")
		return execErr
	})
	if err == nil {
		t.Fatal("withWriteTx succeeded while another connection held the write lock")
	}
	if !strings.Contains(err.Error(), "gave up after") {
		t.Errorf("error %q does not report exhausting the retry budget", err)
	}
	// attempts stays 0 here and that is correct: with _txlock=immediate the
	// write lock is taken at BEGIN, so a contended transaction never reaches
	// its body. What must be retried is the BEGIN itself, which the "gave up
	// after" wrapper above is the evidence for — an unretried BeginTx failure
	// returns the driver error bare.
	if attempts != 0 {
		t.Errorf("fn ran %d time(s) although the transaction never opened", attempts)
	}
}

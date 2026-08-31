package queue

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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
	t.Parallel()
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

// TestWithWriteTx_RollsBackAndRunsOnce pins the two halves of the failure
// path: fn's work is discarded, and it is not run again.
//
// The "once" half is the load-bearing one. An earlier version retried
// contended transactions, which under _txlock=immediate multiplied a
// five-second busy_timeout wait by the attempt count while Queue.Add held
// q.mu — 48.8s measured. busy_timeout is the whole retry policy now, and
// this pins that nothing has quietly reintroduced a loop on top of it.
func TestWithWriteTx_RollsBackAndRunsOnce(t *testing.T) {
	t.Parallel()
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
		t.Errorf("fn ran %d times, want 1; withWriteTx must not retry", attempts)
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
	t.Parallel()
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
// stops the helper rather than opening a transaction.
func TestWithWriteTx_HonoursContextCancellation(t *testing.T) {
	t.Parallel()
	store, _ := newWriteTxStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := store.withWriteTx(ctx, func(*sql.Tx) error { return nil }); err == nil {
		t.Fatal("withWriteTx succeeded with a cancelled context")
	}
}

// TestAddTx_InsertsJobAndFiles pins Add's transactional half in isolation
// from the manifest write its caller performs, which is the split that keeps
// the transaction safe to run again after contention.
func TestAddTx_InsertsJobAndFiles(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

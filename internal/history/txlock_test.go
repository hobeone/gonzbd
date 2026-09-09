package history

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestOpen_TakesTheWriteLockAtBegin pins _txlock=immediate by its BEHAVIOUR
// rather than by matching the DSN string.
//
// The setting is the fix for a real defect: without it a transaction that
// reads before it writes must upgrade mid-transaction, and SQLite answers a
// contended upgrade with SQLITE_BUSY immediately rather than invoking the busy
// handler, so busy_timeout never applies to the case it was configured for.
// Add's `SELECT MAX(sort_key)` then `INSERT` is exactly that shape, and a job
// finalizing concurrently with a job being added failed the add outright.
//
// Until now the only thing standing behind it was
// test/integration/duplicate_test.go, whose unpatched failure rate was
// measured at 13 in 90 runs. A gate that catches a regression 14% of the time
// is not a gate: silently dropping the DSN parameter would have passed.
//
// What makes this deterministic is that the two settings differ in KIND, not
// in timing. Under `immediate` BEGIN takes the write lock, so a second BeginTx
// must wait for the first transaction to end; under the default `deferred`
// BEGIN takes no lock at all and returns at once. The assertion is therefore
// "still blocked after a moment, and completes once the lock is released" —
// never a race against a deadline.
//
// It does NOT wait the block out. A context deadline would not shorten it
// anyway: measured against this driver, a contended BEGIN IMMEDIATE runs for
// the full busy_timeout (5s) regardless of the context passed to BeginTx, so
// the first version of this test took 5.02s to assert what the goroutine below
// establishes in a fraction of that.
//
// Two things make the observation trustworthy. The pool allows 25 connections,
// so the second BeginTx is not merely waiting for a free connection — which
// would make this pass whatever the DSN said. And the second half requires the
// blocked call to SUCCEED once the lock is released: a database that refused
// every transaction would satisfy the first half alone.
func TestOpen_TakesTheWriteLockAtBegin(t *testing.T) {
	t.Parallel()

	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	first, err := db.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("first BeginTx: %v", err)
	}
	firstDone := false
	t.Cleanup(func() {
		if !firstDone {
			_ = first.Rollback()
		}
	})

	type result struct {
		tx  *sql.Tx
		err error
	}
	second := make(chan result, 1)
	go func() {
		tx, err := db.db.BeginTx(t.Context(), nil)
		second <- result{tx: tx, err: err}
	}()

	select {
	case r := <-second:
		if r.tx != nil {
			_ = r.tx.Rollback()
		}
		t.Fatalf("a second BeginTx returned (err=%v) while another write transaction was open; "+
			"BEGIN took no write lock, so _txlock=immediate is not in effect and a "+
			"read-then-write transaction will fail its upgrade with SQLITE_BUSY instead "+
			"of waiting out busy_timeout", r.err)
	case <-time.After(250 * time.Millisecond):
		// Still blocked, which is the point.
	}

	if err := first.Rollback(); err != nil {
		t.Fatalf("rollback first: %v", err)
	}
	firstDone = true

	select {
	case r := <-second:
		if r.err != nil {
			t.Fatalf("the blocked BeginTx failed after the lock was released: %v — it was not "+
				"waiting on the write lock, so this test is not observing what it claims", r.err)
		}
		if err := r.tx.Rollback(); err != nil {
			t.Fatalf("rollback second: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the blocked BeginTx never completed after the first transaction ended")
	}
}

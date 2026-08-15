package queue_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
)

// TestSQLiteStore_AddSurvivesConcurrentCommits pins that adding a job does
// not fail because something else committed at the same time.
//
// SQLiteStore's write methods open a deferred transaction and read before
// they write — Add does SELECT MAX(sort_key) then INSERT. Under WAL, a
// commit from another connection between those two steps makes the write
// return SQLITE_BUSY_SNAPSHOT (517). busy_timeout does not cover that code:
// there is nothing to wait for, the transaction's snapshot is simply stale
// and it has to be restarted.
//
// This is reachable in production without any test scaffolding — a job
// finalizing (MoveToHistory) while another is added is exactly this shape —
// and the wider the gap between the read and the write, the likelier it is.
// Adding an NZB backup write to AddJob in #298 was enough to take one
// integration test from never failing to ~14% (13 failures in 90 runs), which
// is what surfaced it.
//
// The fix is _txlock=immediate in internal/history/db.go: taking the write
// lock at BEGIN removes the upgrade, so contention is an ordinary wait that
// busy_timeout(5000) covers. That wait is the WHOLE retry policy — there is
// no second layer. SQLiteStore.withWriteTx makes exactly one attempt, and
// deliberately so: an earlier retry loop was removed after it was measured
// to buy nothing (a single attempt succeeded as reliably as ten) and to cost
// a great deal (contention surfaces at BeginTx, which blocks for the full
// busy_timeout, so ten attempts took 48.8s to fail). See the withWriteTx doc
// in sqlite_store.go for the measurements.
//
// Retrying WITHOUT the DSN change was measurably not enough either — this
// test failed 4 of 48 adds with ten attempts before _txlock=immediate landed.
// That is the reason the DSN is the fix and the retry loop is not; it is not
// evidence that both are in place.
func TestSQLiteStore_AddSurvivesConcurrentCommits(t *testing.T) {
	store, _, _ := setupTestStore(t)
	ctx := t.Context()

	// An unbroken stream of commits from another connection — deliberately
	// harsher than anything the daemon produces, where the competing writer
	// is an occasional job finalizing. It is kept this aggressive because
	// the fix should make the add path indifferent to how busy the database
	// is, and a gentler writer would pass even against the broken version.
	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Go(func() {
		for paused := false; ; paused = !paused {
			select {
			case <-stop:
				return
			default:
			}
			_ = store.SetPaused(ctx, paused)
		}
	})
	t.Cleanup(func() {
		close(stop)
		writer.Wait()
	})

	const adders, perAdder = 4, 12
	errCh := make(chan error, adders*perAdder)
	var adderWG sync.WaitGroup
	for a := range adders {
		adderWG.Go(func() {
			for i := range perAdder {
				id := fmt.Sprintf("contend%03d%05d", a, i)
				job := newTestJobWithManifest(t, id, "contend-"+id, 2, 3)
				if err := store.Add(ctx, job); err != nil {
					errCh <- fmt.Errorf("Add(%s): %w", id, err)
				}
			}
		})
	}
	adderWG.Wait()
	close(errCh)

	var failures []error
	for err := range errCh {
		failures = append(failures, err)
	}
	if len(failures) > 0 {
		t.Errorf("%d of %d concurrent adds failed; first: %v",
			len(failures), adders*perAdder, failures[0])
	}
}

// TestSQLiteStore_AddWritesManifestOutsideTransaction pins that a job whose
// manifest cannot be written leaves no row behind.
//
// The manifest write moved ahead of the transaction so the write lock is not
// held across an fsync. Ordering it before rather than after keeps the
// failure clean: a write error means no row is inserted at all, and a crash
// between the two leaves an orphan manifest, which Prune already sweeps
// against the active job set.
func TestSQLiteStore_AddWritesManifestOutsideTransaction(t *testing.T) {
	store, _, dir := setupTestStore(t)

	// Block the manifest path with a non-empty directory so the write fails
	// with something other than "not exist".
	const id = "manifestblock001"
	blocker := filepath.Join(dir, "manifests", id+".json.gz")
	if err := os.MkdirAll(blocker, 0o750); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker child: %v", err)
	}

	job := newTestJobWithManifest(t, id, "manifest-block", 1, 2)
	if err := store.Add(t.Context(), job); err == nil {
		t.Fatal("Add succeeded although the manifest could not be written")
	}
	if _, err := store.Get(t.Context(), id); !errors.Is(err, queue.ErrNotFound) {
		t.Errorf("Get after a failed manifest write = %v, want ErrNotFound; a row was left behind", err)
	}
}

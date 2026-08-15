package durability

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResume_ClearsTheStoredExtentWhenTheFileIsGone pins the missing-file half
// of the write-back.
//
// A deleted partial is the strongest disproof a resume can hold: not one
// article's bytes are on disk. The in-memory answer said so — every bit clear,
// Restart set — but the stored row was left untouched, and nothing downstream
// re-reads the file to notice. The resurrection chain is the one
// TestResume_WritesTheRecomputationBackToTheStore documents, and it does not
// care how the row was disproved: the assembler recreates the file,
// Barrier.priorExtent ORs the stale bitmap as its OR-base, buildExtent stamps
// a FRESH Size/ModTimeNs from its own Stat, and the next start's stat fast path
// adopts every stale bit without reading a byte. The job then finishes with
// holes where those articles should be.
//
// The assertion is on the STORE, because the in-memory result was already
// correct and is not what resurrects.
func TestResume_ClearsTheStoredExtentWhenTheFileIsGone(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "movie.mkv")

	// A previous run recorded both articles durable, with a stamp that
	// described the file it then had.
	exts := NewSQLiteExtentStore(openTestDB(t))
	prior := NewBitmap(2)
	prior.Set(0)
	prior.Set(1)
	if err := exts.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: prior, VerifiedTo: 200, PrefixCRC: 0xABCD, HasPrefixCRC: true,
		BytesDurable: 200, Size: 200, ModTimeNs: 424242,
	}}); err != nil {
		t.Fatal(err)
	}

	// Grounding: the file must actually be absent, or this exercises the
	// recomputation path and pins nothing about the branch under test.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture file exists (stat err = %v), so Resume will not take the missing-file branch", err)
	}

	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), exts, testLogger(t))
	res, err := r.Resume(ctx, "job-1", 0, path, 0, 2)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !res.Restart {
		t.Fatal("fixture did not take the missing-file branch")
	}

	stored, err := exts.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("Load returned %d extents, want 1 — the row is expected to be cleared, not deleted", len(stored))
	}
	got := stored[0]
	if n := got.Durable.Count(); n != 0 {
		t.Errorf("the store still records %d articles as durable for a file that does not "+
			"exist. priorExtent ORs this bitmap as its base, so the next checkpoint "+
			"re-commits those bits with a fresh stamp that validates, and the next start "+
			"adopts articles this process never wrote", n)
	}
	if got.BytesDurable != 0 {
		t.Errorf("BytesDurable = %d for a file that does not exist, want 0", got.BytesDurable)
	}
	// VerifiedTo and its CRC describe a prefix of bytes that are not there.
	// Left standing, FinalizeFile would hand QuickCheck a whole-file CRC for a
	// file whose contents this process wrote from scratch.
	if got.VerifiedTo != 0 || got.HasPrefixCRC {
		t.Errorf("VerifiedTo = %d, HasPrefixCRC = %v for a file that does not exist, want 0 and false",
			got.VerifiedTo, got.HasPrefixCRC)
	}
	// The stamp must not describe any real file, or S7's fast path could adopt
	// the cleared row instead of recomputing against the recreated bytes.
	if got.Size != 0 || got.ModTimeNs != 0 {
		t.Errorf("stamp = (%d, %d), want (0, 0)", got.Size, got.ModTimeNs)
	}
}

// TestResume_DoesNotMintAnExtentForAFileThatNeverHadOne pins the existence
// check in clearCommitted.
//
// Every file of every job that has not started downloading yet takes the
// missing-file branch on each restart. Committing unconditionally would write a
// zeroed row for all of them, and a zeroed row is not the same statement as no
// row: it reads as "a resume examined this file and disproved every bit",
// which is a claim about evidence that was never gathered.
func TestResume_DoesNotMintAnExtentForAFileThatNeverHadOne(t *testing.T) {
	ctx := context.Background()
	exts := NewSQLiteExtentStore(openTestDB(t))
	r := NewResumer(NewSQLiteFactLog(openTestDB(t)), exts, testLogger(t))

	res, err := r.Resume(ctx, "job-1", 0, filepath.Join(t.TempDir(), "nothing.mkv"), 0, 2)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !res.Restart {
		t.Fatal("fixture did not take the missing-file branch")
	}

	stored, err := exts.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Errorf("Load returned %d extents, want 0 — a resume that found neither a file "+
			"nor a record wrote one anyway", len(stored))
	}
}

// TestResume_SurfacesAClearFailureWhenTheFileIsGone pins that the clear's own
// durability failing is returned rather than logged, on the same reasoning as
// TestResumeWriteBack_SurfacesACommitFailure: the sweep would otherwise report
// a clean restart while the store still holds the record it was meant to
// erase, and the caller could not tell the difference.
func TestResume_SurfacesAClearFailureWhenTheFileIsGone(t *testing.T) {
	ctx := context.Background()
	real := NewSQLiteExtentStore(openTestDB(t))
	prior := NewBitmap(1)
	prior.Set(0)
	if err := real.Commit(ctx, "job-1", []FileExtent{{
		FileIdx: 0, Durable: prior, BytesDurable: 100, Size: 100, ModTimeNs: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("database is locked")
	r := NewResumer(NewSQLiteFactLog(openTestDB(t)),
		failingExtentStore{ExtentStore: real, err: boom}, testLogger(t))

	_, err := r.Resume(ctx, "job-1", 0, filepath.Join(t.TempDir(), "nothing.mkv"), 0, 1)
	if err == nil {
		t.Fatal("Resume reported success while its clear failed; the store still records " +
			"a deleted file's articles as durable and nothing says so")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want one wrapping the commit failure", err)
	}
	if !strings.Contains(err.Error(), "write-back") {
		t.Errorf("err = %q, want it to name the failing step", err)
	}
}

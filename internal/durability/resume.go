package durability

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
)

// ResumeResult is what one file's resume established from stable storage.
type ResumeResult struct {
	// Runs are the file's stored runs, adopted as they are. Their article
	// ranges are what a resume may skip; everything else in the file is
	// Outstanding (R17). Empty when the gate discarded them, or when the
	// file never had any.
	Runs []Run
	// Restart reports that nothing could be resumed against this file, so
	// every one of its articles is Outstanding. Resume has already deleted
	// the file's rows before returning it, so the disproof is recorded
	// rather than only reported.
	Restart bool
	// Size is the file's size at the moment the gate ran. It is the
	// surviving half of S7's validity stamp — see SyncTarget.Stat for why
	// the mtime half was deleted rather than merely unused.
	Size int64
}

// Resumer answers, from stable storage alone, which of a file's articles still
// need fetching.
//
// It is a READER and a DELETER, never a writer. That asymmetry is the whole
// justification for §3.4's decision to trust the record without verifying a
// byte of it: the barrier is the sole writer, and it writes only after an fsync
// it performed, so nothing in the store can assert bytes that were never
// written. The one thing this type may do is take a claim AWAY when the file on
// disk contradicts it.
//
// Until this change it was a second writer — recompute() re-derived a file's
// state from its bytes and writeBack() committed that answer as the file's
// record. Both are gone with the two-record design; see Resume for what
// replaced them and what that trade gives up.
type Resumer struct {
	runs RunStore
	log  *slog.Logger
}

// NewResumer wires a resumer. It owns none of its collaborators' lifecycles
// and holds no lock: Resume does I/O throughout, is per-file, and shares no
// state between calls, which is how it stays off other jobs' way (R15).
func NewResumer(rs RunStore, log *slog.Logger) *Resumer {
	return &Resumer{runs: rs, log: log}
}

// Resume establishes what is actually on disk for one file.
//
// It is one stat and no reads. The record is authoritative (§3.4) — this
// INVERTS S4, which used to say a recomputation from the bytes beats the
// stored record and that the record is never authoritative. Believing it
// absolutely would be wrong, though: if the partial file were deleted or
// replaced between runs, the record would report most articles complete and
// only the remainder would be fetched, producing a file with holes exactly
// where the "done" articles were.
//
// So trust gets a floor, and it is a size comparison:
//
//	stat(path).size >= max(Offset+Length) over the file's runs
//
// A file that satisfies it is adopted whole. A missing file, or one shorter
// than its runs claim, has those runs DELETED and is downloaded again. A file
// that is merely LONGER is the ordinary pre-allocated case and is adopted.
//
// # Why size alone, and not (size, mtime)
//
// S7's stamp used to be the pair, and the mtime half is not merely redundant
// now — it is actively harmful, because of what the RESPONSE to a mismatch
// costs. It used to fall through to a recomputation: the file was re-read, the
// records corrected, and the stamp cost one read. With recompute deleted the
// only response left is discard-and-refetch, so the same stamp would cost the
// whole file. An mtime moves without a byte moving — a restore from backup, a
// copy that does not preserve timestamps, a tool that touches the file — and
// each of those would trigger a full re-download of an intact file. A size
// shortfall cannot happen that way: it means bytes the record claims are
// genuinely not there.
//
// # What this gives up
//
// In-place corruption that preserves the file's length is no longer detected
// at startup. par2 detects and repairs it at completion, which is the same
// answer §3.3 gives for an overlap.
//
// # The gate depends on WHERE the sweep runs, not only on what it compares
//
// The sweep calling this must complete inside Start BEFORE the downloader can
// dispatch. That is what stops the assembler re-creating and pre-allocating a
// deleted partial underneath the gate, which would make a file of zeros pass a
// comparison the real file would have failed. It is a property of the sweep's
// PLACEMENT rather than of this function, and it is written down here because
// a refactor that moves the sweep later breaks the guarantee silently.
func (r *Resumer) Resume(ctx context.Context, jobID string, fileIdx int32, path string) (ResumeResult, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A missing file is the strongest possible disproof of every run
			// the record holds for it. Every article is Outstanding, which is
			// S3's safe default rather than a failure.
			r.log.Warn("durability resume found no partial file; discarding its durable runs",
				"job", jobID, "file", fileIdx, "path", path)
			if err := r.discard(ctx, jobID, fileIdx); err != nil {
				return ResumeResult{}, err
			}
			return ResumeResult{Restart: true}, nil
		}
		// Any other stat failure is surfaced. Reading it as absence would
		// restart a file whose bytes are still there (A2).
		return ResumeResult{}, fmt.Errorf("durability: resume stat job=%s file=%d: %w", jobID, fileIdx, err)
	}

	runs, err := r.runs.ForFile(ctx, jobID, fileIdx)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("durability: resume runs job=%s file=%d: %w", jobID, fileIdx, err)
	}
	bound := boundOver(runs, nil)
	if fi.Size() < bound {
		// Never silent (A2): this is the one place a job loses ground it had
		// recorded, and the operator's copy of the file changed underneath it.
		r.log.Warn("the partial file is shorter than its durable runs claim; discarding them "+
			"and re-downloading the file",
			"job", jobID, "file", fileIdx, "path", path,
			"size", fi.Size(), "runs_claim", bound, "runs", len(runs))
		if err := r.discard(ctx, jobID, fileIdx); err != nil {
			return ResumeResult{}, err
		}
		return ResumeResult{Restart: true, Size: fi.Size()}, nil
	}
	return ResumeResult{Runs: runs, Size: fi.Size()}, nil
}

// discard removes one file's runs after the file on disk has disproved them.
//
// The only mutation this type performs. See the Resumer type doc for why that
// direction is the one a resume is entitled to.
func (r *Resumer) discard(ctx context.Context, jobID string, fileIdx int32) error {
	if err := r.runs.DeleteFile(ctx, jobID, fileIdx); err != nil {
		return fmt.Errorf("durability: resume discard runs job=%s file=%d: %w", jobID, fileIdx, err)
	}
	return nil
}

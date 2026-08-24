package durability

import (
	"fmt"
	"os"
)

// TrimToRuns trims a CLOSED file to the bound its durable runs imply and
// fsyncs it, returning the bound it applied — or 0 when it trimmed nothing.
//
// It exists for the startup pass that finishes a finalize a crash interrupted
// (docs/durability-contract.md, accepted limitation 6). Barrier.FinalizeFile
// cannot be re-run there: its first act is Truncator.Drain, which answers
// ErrFileNotOpen and takes the early exit, and nothing reopens a file during
// the resume sweep. What that pass actually needs is only this step — no
// drain, because a fresh process has an empty write cache; no commit, because
// there are no new articles; no ack, because the sweep has already re-set the
// bits from the record.
//
// # The bound rule has ONE owner and this is not it
//
// boundOver is that owner, and both callers reach the rule only through it.
// The two differ in how they APPLY the bound, not in what it is: FinalizeFile
// truncates through the open handle it is already holding, because the file
// has buffered writes it has just drained and fsynced, while this truncates a
// path with no handle anywhere. Neither may compute the bound for itself. A
// second implementation of the RULE is the second enforcement point AGENTS.md
// §2 forbids; a second way of applying one rule is not.
//
// # It never grows a file
//
// os.File.Truncate extends with zeros when the size exceeds the file's, so a
// bound above the file's length would silently manufacture content the record
// then vouches for. The stat below refuses that, and it is a real guard rather
// than a defensive one even though §3.4's resume gate has already established
// size >= max(Offset+Length) for every file it adopted: this function does not
// get to assume its caller ran that gate, and the failure it prevents is
// exactly the one the gate exists for, arriving from the other side.
//
// # Both the truncate and the fsync must succeed
//
// The caller marks the file complete on a nil error, and that claim outlives
// the process. A truncate whose fsync failed can be lost by a crash, leaving a
// file recorded complete that still carries pre-allocation's tail — which is
// the untrimmed-file hazard the barrier's own ordering note describes, reached
// by a different route. So a partial success is reported as a failure.
func TrimToRuns(path string, runs []Run) (int64, error) {
	bound := boundOver(runs, nil)
	if bound <= 0 {
		// No run claims any byte, so there is no bound to trim to. Truncating
		// to 0 here would cut away a file whose articles the record cannot
		// account for, which is the opposite of conservative — the same
		// reason FinalizeFile guards its own truncate on bound > 0.
		return 0, nil
	}

	//nolint:gosec // G304: path is caller-supplied and names a file the caller has already
	// resolved through the queue's own recorded filename, the same derivation the assembler
	// used to open it for writing
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("durability: open %s to trim: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("durability: stat %s to trim: %w", path, err)
	}
	if fi.Size() <= bound {
		// Already at or below the bound. Below is not this function's
		// business: a file shorter than its runs claim has disproved them, and
		// §3.4's gate answers that by discarding the runs and re-fetching.
		return 0, nil
	}

	if err := f.Truncate(bound); err != nil {
		return 0, fmt.Errorf("durability: trim %s to %d: %w", path, bound, err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("durability: fsync %s after trimming to %d: %w", path, bound, err)
	}
	return bound, nil
}

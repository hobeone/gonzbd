package durability

import (
	"context"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// WrittenArticle is one article whose bytes the target handed to the OS
// without error. It is a *Written* article, not a Durable one — the barrier
// is what closes that gap, and until its fsync returns nil this struct
// asserts nothing about stable storage (S1, S2).
type WrittenArticle struct {
	// FileIdx is the target file within the job.
	FileIdx int32
	// ArtIdx is the article's global index within the job's manifest. It is
	// what a DurableProof names, so it must match the queue's numbering.
	ArtIdx int32
	// Offset is the decoded byte position within the target file.
	Offset int64
	// Length is the decoded byte length.
	Length int32
}

// SyncTarget is the barrier's view of a job's open files. It is deliberately
// narrow: the barrier never sees a file handle, a write cache, or a byte, so
// it cannot write, cannot ack early, and cannot be tempted to derive
// durability from anything but a completed Sync.
//
// X1 makes the implementation the single writer of each file it reports.
type SyncTarget interface {
	// Files returns the job's currently open files. R8 bounds barrier cost
	// by this set rather than by job size: the barrier fsyncs open files,
	// not every file the job will eventually produce.
	Files() []int32

	// Drain flushes the file's buffered writes and returns the articles
	// whose bytes reached WriteAt without error.
	//
	// It must NOT return an article that is merely buffered — S2 makes
	// acceptance and durability different things, and this return value is
	// the only evidence the barrier has. Returning a buffered article here
	// is exactly defect #355 relocated: the barrier would fsync bytes that
	// are not in the file and ack an article that is not on disk.
	//
	// Re-reporting an article a previous Drain already returned is
	// permitted and expected — R12 makes at-least-once delivery the
	// contract and the barrier's apply absorbs the duplicate.
	Drain(ctx context.Context, fileIdx int32) ([]WrittenArticle, error)

	// Sync fsyncs the handle. Until this returns nil, nothing the preceding
	// Drain reported may be claimed (S1). An implementation that returns
	// nil without an actual fsync — or that treats a buffered flush as
	// sufficient — makes every downstream claim a lie that survives a
	// process crash but not a power loss.
	Sync(ctx context.Context, fileIdx int32) error

	// Stat returns the file's size and modification time as they are now.
	// The pair is the S7 validity stamp a later resume checks the file
	// against before adopting any Class B value, so it must be read after
	// the Sync it describes.
	Stat(fileIdx int32) (size int64, modTimeNs int64, err error)

	// Path returns the file's path on disk, for a stall reason a user can
	// act on (R27).
	//
	// A storage fault stalls the job until a human clears the condition, and
	// "ENOSPC on sync" without a path does not say which volume filled up.
	// A job's files can sit on different mounts — a category directory, a
	// symlinked incomplete dir — so the job name does not identify the
	// device either.
	//
	// It returns a string rather than (string, error) and has no context:
	// an implementation that cannot name the file returns "", which degrades
	// the message and nothing else. Nothing may branch on this value; it is
	// diagnostic only.
	Path(fileIdx int32) string

	// ArticleCount returns how many articles this file has in total. The
	// barrier needs it to size the durable bitmap, and to re-derive a
	// loaded one at its true width rather than the byte width persistence
	// rounded it up to.
	ArticleCount(fileIdx int32) int

	// FileLocalOrdinal maps a global article index to this file's
	// bit position. The second result is false when the article does not
	// belong to the file; the barrier treats that as a bookkeeping defect
	// and fails loudly (A2, R28) rather than skipping the article.
	FileLocalOrdinal(fileIdx int32, artIdx int32) (int, bool)
}

// Acker receives the barrier's download-success acks.
//
// AckDurable takes a DurableProof, which has no exported constructor. That
// is X3 made concrete: "ack only after fsync" is not a rule six call sites
// must each remember, it is a signature no caller outside this package can
// satisfy WITH ANY ARTICLE IN IT. R9 is enforced by the compiler to that
// bound, and no further — see the DurableProof type doc for what an outside
// package can still construct, and why it is inert rather than impossible.
type Acker interface {
	AckDurable(p DurableProof) error
}

// Stallable is how a storage fault reaches the job, per A1 and R18-R21.
//
// Neither method may mark an article failed. A storage fault resolves
// against storage: the articles involved stay Outstanding, the health
// percentage and the failed-byte count are untouched (R21), and the job
// either waits for the condition to clear or stops.
type Stallable interface {
	// Stall parks the job with a surfaced, actionable reason and leaves its
	// articles Outstanding, to be re-evaluated on an interval or on user
	// action (R19).
	Stall(jobID string, f *storagefault.Fault)
	// Fail stops the job with that reason (R20). Still no article is
	// marked failed.
	Fail(jobID string, f *storagefault.Fault)
}

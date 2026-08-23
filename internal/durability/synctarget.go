package durability

import (
	"context"
	"errors"

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
	// CRC32 is the decoded article's CRC32, the same value the decoder
	// validated against the yEnc trailer. It is carried here so a later
	// durable-run record can be built from confirmed-written articles without
	// re-deriving the checksum. Nothing reads it yet — a later task groups
	// articles into contiguous runs and combines their CRCs.
	CRC32 uint32
}

// ErrFileNotOpen reports that a file the barrier listed is no longer open.
//
// It is part of SyncTarget's contract rather than an assembler detail, because
// the barrier has to be able to tell it apart from a storage failure and the
// interface is the only thing both sides share.
//
// It is never a fault. A file leaves the open set for three deliberate
// reasons — a completed finalize closing its handle, a cancelled job, a job
// entering post-processing — and every one of them drains and syncs before
// closing. So the barrier has nothing left to do for that file, and nothing to
// surface: classifying it as storage would park a healthy job with a reason
// naming a device that did not fail and an operator action that does not
// exist. The worker's own comment calls it "a bookkeeping disagreement, not a
// storage fault ... the A1 conflation in reverse".
//
// The race is structural, not exotic: finalizeCompletedFile releases the
// per-job barrier mutex before its deferred CloseFile, so a checkpoint can
// hold the lock, take the file from Files(), and have the close processed
// before it calls Drain.
var ErrFileNotOpen = errors.New("durability: file is not open")

// ErrTargetUnavailable reports an operation that never ran, for a reason that
// is not about storage.
//
// It is the general form of the rule ErrFileNotOpen states for one case, and
// it exists because storagefault.Classify defaults everything it does not
// recognise to RETRYABLE. Any non-storage condition that reaches it therefore
// comes back as a storage fault, and Stallable parks a healthy job naming a
// disk that did not fail — with a reason no operator action can clear. The
// conditions that actually arrived here were an ordinary shutdown's cancelled
// context and a deliberate close; neither is evidence about a device.
//
// The rule the implementation owes the barrier is: return either a
// *storagefault.Fault, or an error wrapping one of these sentinels. Never a
// bare error whose meaning has to be guessed by errno matching. That is a
// BOUNDARY rule rather than a per-call-site fix, because the same conflation
// was found at six sites and patching them one at a time leaves the seventh.
//
// A timeout is the case the rule is easiest to get wrong on, and it splits.
// The implementation's OWN bound expiring — the worker did not answer within
// barrierOpTimeout — IS evidence about storage: the worker is parked in a
// syscall against a mount that is not answering, which is precisely the
// condition R19 asks to be surfaced. The CALLER's deadline expiring is not:
// the caller chose to stop waiting, and on the shutdown path it always does.
// So the implementation converts the first into a fault and wraps the second
// in this sentinel.
var ErrTargetUnavailable = errors.New("durability: sync target unavailable")

// ErrFaultRouted marks a storage fault the barrier has ALREADY dispatched to
// Stallable, so a caller does not park the job a second time for it.
//
// It exists because the inference it replaces stopped being sound. The
// application layer used to read "the error chain contains a *storagefault.Fault"
// as "the barrier routed it", which held only while routeFault was the one
// thing that let a fault escape this package. It no longer is: the SyncTarget
// boundary now mints a fault of its own when the worker does not answer, and
// that one is NOT routed — so the old inference silently swallowed it, and the
// job carried on with a file that was never trimmed.
//
// A marker rather than a wrapper type, so errors.As still finds the fault
// underneath and callers can keep reading its Permanent flag.
var ErrFaultRouted = errors.New("durability: storage fault already routed")

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

	// Confirm releases the file's drain report, and must be called only once
	// the cycle that consumed it has fully landed: the extent committed and
	// the articles acked.
	//
	// It exists because a successful fsync is not the end of a barrier. The
	// commit and the ack follow it and can both fail, and until they have
	// succeeded the report is the only record that those articles reached
	// disk. A target that released it on the fsync lost them to any later
	// failure, and Drain's re-report — the thing that makes a retried barrier
	// correct — had nothing left to re-report.
	//
	// It returns nothing. It records that work already succeeded, so a caller
	// has no outcome to act on: a Confirm that does not arrive costs one
	// redundant re-report, which R12 requires the apply to absorb anyway.
	Confirm(ctx context.Context, fileIdx int32)

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

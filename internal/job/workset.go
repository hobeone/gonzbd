package job

import (
	"errors"
	"fmt"

	"github.com/hobeone/gonzbd/internal/durability"
)

// ErrProofForAnotherJob is returned by AckDurable when the proof names a
// different job.
//
// internal/queue answered this question by LOOKUP: the proof carried a job id
// and the Queue found the job for it. With the accounting on *Job there is no
// lookup left, so the same question has to be asked as a check — the receiver
// is chosen by the caller, and a caller that picks the wrong one would
// otherwise mark articles done on a job whose manifest merely happens to be
// long enough. Every index in the proof is meaningful only against the
// manifest it was numbered against.
var ErrProofForAnotherJob = errors.New("job: durable proof names another job")

// ErrProofNamesUnknownArticles is returned by AckDurable when a proof carries
// an article index this job's manifest does not have. The articles that were
// IN range have already been applied when it is returned: they were made
// durable by a real fsync, and discarding their acks would cost a re-download
// of bytes already on disk.
//
// It is an error rather than a log line because this package does no I/O and
// holds no logger, and A2 forbids dropping the finding silently. A caller that
// wants internal/queue's warn-and-continue behaviour must say so explicitly —
// classify with errors.Is, report it, and return nil — which is a decision at
// the call site rather than an omission.
var ErrProofNamesUnknownArticles = errors.New("job: durable proof names articles this job does not have")

// A *Job is the barrier's Acker directly — no adapter, which is the point.
// #355/#356 were the same defect refiled because six sites could ack, and an
// app-local adapter that reached the done bits its own way would be that shape
// again. Asserted here rather than left to a call site so that a change to
// either signature fails in this package.
var _ durability.Acker = (*Job)(nil)

// AckDurable resolves the articles a completed fsync covers.
//
// It takes a durability.DurableProof rather than a slice of indices, and that
// is the whole point. DurableProof has no exported constructor outside
// internal/durability, so no code path that has not run a barrier can reach
// this method WITH ANY ARTICLE IN HAND — R9 is enforced by the compiler rather
// than by six call sites each remembering it (X3).
//
// The bound is on the payload, not on the type: `durability.DurableProof{}`
// compiles in any package. The `len(arts) == 0` early return below is what
// makes such a proof inert, so it is part of the invariant rather than a
// defensive nicety. See the DurableProof type doc.
//
// This gate covers THIS door only. SeedFromRuns and ReplaceFromRuns also
// reach markDone without a proof, deliberately — their evidence is stable
// storage, which a proof cannot express. Do not read "AckDurable is
// proof-gated" as "nothing marks an article done without a barrier".
//
// Both of those doors are narrower than they were, though, and it is worth
// saying exactly how far that goes. durability.Run is an exported struct with
// exported fields, so ANY package can build one — the narrowing is not that
// the type is unforgeable. It is that a Run carries an article RANGE and a
// byte range that runsCoverage validates against the job's own manifest, where
// the FileExtent these replaced carried a fully exported, settable Bitmap
// whose set bits were taken at face value. A caller that fabricates a Run
// still has to name articles the manifest has.
//
// Before this design the assembler could ack from six places, each
// independently responsible for knowing that acceptance into a buffer is not
// evidence about disk. That is why the same defect kept being refiled (#355,
// #356). There is now one place, and it is not here: this method applies what
// the barrier decided, it does not decide anything.
//
// The apply is idempotent because markDone is (R12). At-least-once delivery is
// the contract — SyncTarget.Drain is explicitly permitted to re-report an
// article a previous Drain returned — so a replayed proof must not double-count
// bytes. markDone early-returns on an already-done article before touching
// BytesDownloaded, so it does not.
//
// This is the signature durability.Acker requires, so a *Job satisfies that
// interface with no adapter between the barrier and the article accounting —
// which is the arrangement #355/#356 were refiled against. A Barrier serves a
// whole process, though, and a *Job answers only for itself: routing a proof
// to the job it names is the caller's, and the JobID check below is what makes
// a mis-routed proof loud instead of silently wrong.
func (j *Job) AckDurable(p durability.DurableProof) error {
	return j.ackDurable(p.JobID(), p.Articles())
}

// ackDurable is AckDurable's body, taking the proof already unwrapped.
//
// The split is for testability and buys nothing else: a DurableProof carrying
// ANY article cannot be minted outside internal/durability — that is the whole
// point of the type — and internal/durability's own tests cannot import this
// package, because this package imports it. So the exported door is reachable
// from a test here only with an empty proof, which is by design inert, and
// every branch below would otherwise be unreachable from any test that could
// exist.
//
// It weakens nothing. This is unexported, so outside the package the only door
// is still AckDurable and still takes a proof; inside the package markDone was
// already reachable. The evidence bound is on what CROSSES the package
// boundary, and that is unchanged.
func (j *Job) ackDurable(proofJobID string, arts []int32) error {
	if len(arts) == 0 {
		return nil
	}
	if proofJobID != j.id {
		return fmt.Errorf("job %s: AckDurable: %w: %s", j.id, ErrProofForAnotherJob, proofJobID)
	}
	var invalid, nArt int
	if err := j.withResidentContent(func(m *Manifest, prog *JobProgress) error {
		nArt = m.NumArticles()
		for _, idx := range arts {
			i := int(idx)
			if i < 0 || i >= nArt {
				invalid++
				continue
			}
			prog.markDone(m, i)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("job %s: AckDurable: %w", j.id, err)
	}
	if invalid > 0 {
		// A proof naming an article this job does not have is a numbering
		// defect upstream, not a storage condition. The in-range articles above
		// have already been applied, so this reports the anomaly without
		// discarding acks a real fsync earned.
		return fmt.Errorf("job %s: AckDurable: %w: %d of %d indices out of range (%d articles)",
			j.id, ErrProofNamesUnknownArticles, invalid, len(arts), nArt)
	}
	return nil
}

// FailureAck is what AckPermanentFailure observed, handed back as a value.
//
// It exists because this package does no I/O and holds no logger, while the
// operation it describes has three consumers that are not article accounting:
// the failed_articles rows that must be persisted, the operator-facing warning
// that a job lost articles, and the note that on-demand par2 released its
// recovery volumes. internal/queue did all three inline, under and around its
// own lock. Returning them makes each the caller's explicit step instead of a
// side effect buried in a mutation.
type FailureAck struct {
	// Persist is the in-range subset of the requested indices, in the order
	// given. Only these may be written to failed_articles: a row for an
	// out-of-range index could never be interpreted against any manifest.
	Persist []int32
	// FirstTime counts the articles this call actually moved to failed — an
	// already-resolved article contributes nothing.
	FirstTime int
	// Invalid counts indices this job's manifest does not have.
	Invalid int
	// NumArticles is the manifest's article count, for reporting Invalid
	// against.
	NumArticles int
	// ReleasedPar2 reports that the deferred recovery volumes were un-deferred
	// by this call.
	ReleasedPar2 bool
	// FailedBytes and RecoveryBytes are the job's totals AFTER the marks, the
	// pair internal/queue logged together so the warning says how much damage
	// there is against how much repair capacity the job claims.
	FailedBytes   int64
	RecoveryBytes int64
}

// AckPermanentFailure records articles that will never be fetched, on every
// eligible server.
//
// Unlike AckDurable this needs no proof, and R10 says why: a permanent failure
// asserts nothing about disk. There is no fsync for it to be ordered after,
// and losing one in a crash is safe — the restart re-attempts an article that
// fails again, costing one request. The asymmetry is the design's, not an
// oversight: over-fetching is a cost and over-claiming is a defect, so only the
// claim direction needs the compiler's help.
//
// A storage fault must never reach this method. That is A1: ENOSPC or EIO
// resolves against storage and stalls the job with the articles left
// Outstanding, while a missing or corrupt article resolves against the article
// and is counted as damage. Routing a full disk here would burn the article's
// retry budget, inflate the job's failed-byte count and degrade its reported
// health (R21).
//
// # What moved out of it
//
// internal/queue's version wrote failed_articles itself, below its own lock,
// and took a second mutex plus a reload generation counter to order that write
// against a concurrent reversal. None of that is here: this method mutates and
// returns, and the persistence is the caller's, from FailureAck.Persist. The
// ordering problem does not disappear — a caller that persists these rows must
// still order them against whatever reverses them — but it is now a question
// asked where the writes are, rather than one this type has to answer about
// writes it does not perform.
//
// On-demand par2 stays, because it is article accounting: a permanent
// data-article failure proves this job will need repair, so the deferred
// recovery volumes are released now — while the connection is live and the
// articles are freshest — rather than at the download-complete verify. The
// release is reported rather than logged, and it sets par2ReleaseReason only
// inside the branch where undeferRecovery reported a change, which is what
// keeps HasPar2Verdict's documented exception exact.
func (j *Job) AckPermanentFailure(artIdxs []int32) (FailureAck, error) {
	var r FailureAck
	if len(artIdxs) == 0 {
		return r, nil
	}
	err := j.withResidentContent(func(m *Manifest, p *JobProgress) error {
		r.NumArticles = m.NumArticles()
		// Only the in-range indices are collected for persistence. An
		// out-of-range one is a numbering defect and a row for it could never
		// be interpreted against any manifest; it is counted instead.
		r.Persist = make([]int32, 0, len(artIdxs))
		for _, idx := range artIdxs {
			i := int(idx)
			if i < 0 || i >= r.NumArticles {
				r.Invalid++
				continue
			}
			r.Persist = append(r.Persist, idx)
			if p.markFailed(m, i) {
				r.FirstTime++
			}
		}
		if r.FirstTime > 0 {
			r.FailedBytes, r.RecoveryBytes = p.failedBytes, m.RecoveryBytes()
			if !p.par2Recovered && m.RecoveryFiles() > 0 {
				if p.undeferRecovery(m, p.DeferredRecoveryIndices()) {
					p.par2ReleaseReason = "permanent article download failure detected on active queue"
					r.ReleasedPar2 = true
				}
			}
		}
		return nil
	})
	if err != nil {
		return FailureAck{}, fmt.Errorf("job %s: AckPermanentFailure: %w", j.id, err)
	}
	return r, nil
}

// SeedFromRuns installs a job's stored durable runs into its live work set, so
// a restart does not re-fetch bytes an earlier run already got onto stable
// storage (L3).
//
// A run is written only after the fsync that makes its bytes durable, and this
// is the point at which that record becomes the running job's belief about
// what is outstanding.
//
// It is ADDITIVE, deliberately and permanently: it only ever SETS a bit. An
// article this does not name keeps whatever state it already had.
//
// That is the right contract for the stall-recovery caller and the wrong one
// for the startup sweep, which is why there are two entry points. A stall
// recovery replays runs LOADED FROM THE STORE: it is re-delivering an ack whose
// fsync already landed, and it has stat'ed nothing. A clear there would discard
// the acks this process made since the last commit — precisely the bits that
// phase exists to preserve.
//
// The startup resume sweep is the caller that HAS stat'ed each file, and it
// uses ReplaceFromRuns instead. Do not merge the two back into one entry
// point, with or without a flag: the union of the two contracts is either #362
// (a stale bit outliving the check that disproved it) or a stall recovery that
// throws away live acks.
//
// Every run is checked before any article is marked, which is the same
// validate-then-apply shape ReplaceFromRuns uses to build its covered set. An
// interleaved form applies each run as it clears, which leaks exactly the
// corruption runsCoverage exists to detect: a run it refuses means the manifest
// was rebuilt under rows keyed on the old numbering, and that is a statement
// about the whole batch, not about the one row. markDone is one-way on this
// path (R9) and decrements pendingArticles, so an article wrongly marked here
// is never enumerated for dispatch again.
//
// The indexing rule that makes both safe lives in runsCoverage; see its doc.
func (j *Job) SeedFromRuns(runs []durability.Run) error {
	if len(runs) == 0 {
		return nil
	}
	if err := j.withResidentContent(func(m *Manifest, p *JobProgress) error {
		type span struct{ lo, hi int }
		spans := make([]span, 0, len(runs))
		for _, r := range runs {
			lo, hi, err := runsCoverage(m, r)
			if err != nil {
				return err
			}
			spans = append(spans, span{lo: lo, hi: hi})
		}
		for _, s := range spans {
			for i := s.lo; i <= s.hi; i++ {
				p.markDone(m, i)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("job %s: SeedFromRuns: %w", j.id, err)
	}
	return nil
}

// ReplaceFromRuns installs what a fresh resume established about a job's
// files, in place of what was recorded about them. It is the authoritative
// half of the pair SeedFromRuns documents, and it closes #362.
//
// It returns the number of article bits it CLEARED, which the caller needs for
// two things it must do and this method cannot: warn that the operator's copy
// of a file changed underneath a previous run, and get the correction onto disk
// before anything can re-hydrate over it. See "The caller's two obligations"
// below.
//
// # Why an authority is needed at all
//
// The restore path derives every article's state from the SAME runs before any
// of this dispatches, so on the ordinary path the two agree. They diverge in
// exactly one case, and it is the case that matters: durability.Resumer stats
// each file and DELETES the runs of a file shorter than they claim. The sweep
// therefore hands back a smaller set than the restore installed, and those
// articles have to go back to Outstanding. With only an additive entry point
// the earlier belief always won, so a truncated or deleted partial finished as
// a complete file with a zero-filled hole in it and no warning (#362).
//
// # What it replaces, and what it deliberately does not
//
// For every file the sweep named, an article no surviving run covers goes back
// to Outstanding — S3's absence of evidence read as absence, rather than as
// evidence. ResumeResult.Restart needs no case of its own: a discarded file
// comes back with no runs at all, which already says "nothing here is
// recorded".
//
// Two things are NOT touched, and both are limits on what the caller's
// evidence covers rather than concessions:
//
//   - A file not named in files keeps its state entirely. The startup sweep
//     omits a file it never resumed — one whose name was never resolved, one
//     it did not reach before a storage fault, and every file of a job past
//     downloading — and an omission is silence, not a finding of absence.
//     This is why the file indices are carried separately from the runs: a
//     file whose runs were all discarded contributes NO run, and would be
//     indistinguishable from a file the sweep never looked at.
//   - A permanently failed article is never cleared. See markNotDone: its
//     bytes were never on disk, so their absence is the recorded outcome and
//     not new information.
//
// # A file whose bytes no longer support it does not stay Complete
//
// Complete means "the assembler is finished with this file", NOT "every
// article arrived" — the pipeline hands a permanently failed article to the
// assembler, which closes the file with a gap. So Complete cannot be
// re-derived from the article bits, and it is cleared on exactly the evidence
// this method has: the flag is dropped for a file where a bit was actually
// CLEARED, and left alone otherwise. A Complete file whose successful articles
// all verify keeps its flag even though a failed article is permanently
// missing, which is the case the naive "Complete implies fully populated" rule
// would re-download on every restart.
//
// AssembledCRC32 goes with the flag. It is the combined CRC of a whole
// assembled file, and a file that has lost bytes is not that file any more;
// leaving it would hand postproc's QuickCheck a checksum describing bytes that
// are no longer there. Zero is its documented "unavailable" value (#349), so
// clearing it costs a par2 verify rather than producing a wrong verdict.
//
// # The derived figures
//
// recompute re-derives pendingArticles, articlesResolved, articlesFailed,
// failedBytes and every per-file byte figure from the bitmaps, so the job's
// reported health matches its per-article state. Clearing bits without it is
// the half-inverse that produced #300 from the other direction. It runs
// unconditionally rather than only when something changed: after a pure
// markDone pass it is a no-op by construction, and a condition whose two arms
// agree is untestable branching.
//
// # The caller's two obligations
//
// A cleared bit must reach the store before anything re-hydrates this job, and
// this method cannot put it there — it does no I/O. internal/queue persisted
// inline, under the same call; here the caller must flush, synchronously, when
// the returned count is non-zero. checkpoint.Checkpointer.Flush exists for
// exactly that caller and says so in its own doc.
//
// The window is reached without any concurrency: the startup sweep can stall a
// job right after this returns, and a stall evicts the manifest with the
// correction still in memory only. Only a CLEAR needs the flush — a bit this
// sweep merely SET can be lost to the same window at the cost of a re-fetch,
// which is the safe direction under S3, while a bit it cleared is the direction
// that finalizes a file over bytes that are not there.
//
// The second obligation is the warning. This is the one place a job loses
// ground it had recorded, and A2 forbids it being silent.
//
// # Residency
//
// It requires a resident manifest and does not hydrate one. internal/queue's
// version hydrated a paused job for the duration and evicted it again,
// because a PAUSED job was not resident and is exactly the job that needs
// this. Residency is no longer this type's to arrange — internal/dispatch owns
// it — so a caller must make the job resident first and put it back
// afterwards. The requirement itself is unchanged: skip a paused job and #362
// survives in that branch.
func (j *Job) ReplaceFromRuns(files []int32, runs []durability.Run) (cleared int, err error) {
	if len(files) == 0 {
		return 0, nil
	}
	if err := j.withResidentContent(func(m *Manifest, p *JobProgress) error {
		// covered is built over the WHOLE job before any file is walked,
		// because a run names articles globally and the loop below asks the
		// question per article rather than per run. Runs for a file the sweep
		// did not name are simply never consulted — the file loop is what
		// bounds the authority.
		covered := make([]bool, m.NumArticles())
		for _, r := range runs {
			lo, hi, cErr := runsCoverage(m, r)
			if cErr != nil {
				return cErr
			}
			for i := lo; i <= hi; i++ {
				covered[i] = true
			}
		}
		// Validate every file index before clearing anything, for the reason
		// SeedFromRuns validates every run first: a bad index means the
		// manifest was rebuilt under rows keyed on the old numbering, which is
		// a statement about the batch rather than about the one entry, and a
		// partial apply would leave articles cleared against a numbering
		// nothing agrees with.
		for _, fi := range files {
			f := int(fi)
			if f < 0 || f >= m.NumFiles() {
				return fmt.Errorf("file index %d out of range (%d files)", f, m.NumFiles())
			}
		}
		for _, fi := range files {
			f := int(fi)
			lo, hi := m.FileRange(f)
			fileCleared := 0
			for i := lo; i < hi; i++ {
				if covered[i] {
					p.markDone(m, i)
					continue
				}
				if p.markNotDone(i) {
					fileCleared++
				}
			}
			if fileCleared > 0 {
				fp := &p.files[f]
				fp.Complete = false
				fp.AssembledCRC32 = 0
				cleared += fileCleared
			}
		}
		p.recompute(m)
		return nil
	}); err != nil {
		return 0, fmt.Errorf("job %s: ReplaceFromRuns: %w", j.id, err)
	}
	return cleared, nil
}

// runsCoverage returns the inclusive GLOBAL article range one run accounts
// for, after checking it against the file the run claims.
//
// It exists because both seeding entry points need the same check and it is
// easy to get wrong in a way no bounds check downstream catches. A run's
// FirstArtIdx and LastArtIdx are already global — that is exactly what
// replaced the file-local ordinal conversion the deleted durable bitmap
// needed — so there is no arithmetic here, only the verification that the pair
// really lies inside the file the row names.
//
// A run that does not is refused LOUDLY rather than clamped or skipped (A2,
// R28). It means the manifest was rebuilt to a different shape under rows
// keyed on the old numbering, and marking whatever articles happen to sit at
// those indices done would skip real downloads silently. The retry path
// already drops the rows for that case before requeuing; anything reaching
// here has escaped it.
func runsCoverage(m *Manifest, r durability.Run) (first, last int, err error) {
	fi := int(r.FileIdx)
	nFiles := m.NumFiles()
	if fi < 0 || fi >= nFiles {
		return 0, 0, fmt.Errorf("file index %d out of range (%d files)", fi, nFiles)
	}
	lo, hi := m.FileRange(fi)
	first, last = int(r.FirstArtIdx), int(r.LastArtIdx)
	if first > last || first < lo || last >= hi {
		return 0, 0, fmt.Errorf(
			"file %d: run covers articles [%d,%d], outside the file's range [%d,%d)",
			fi, first, last, lo, hi)
	}
	return first, last, nil
}

// undeferRecovery clears the on-demand hold on the given recovery volumes,
// re-activating their articles for dispatch, and reports whether anything
// changed. The caller must hold the job's contentMu for writing — every site
// reaches it through withResidentContent.
//
// RemainingBytes needs no fixup of its own: it derives from the fetch policy
// on every read, so clearing the hold is what makes the file's bytes start
// counting as remaining — see derivedRemainingBytes. Indices that are out of
// range or not held are ignored rather than refused, because its caller passes
// DeferredRecoveryIndices and needs the apply step to skip anything stale; a
// caller that wants a request rejected wholesale must check the indices first.
func (p *JobProgress) undeferRecovery(m *Manifest, fileIdxs []int) bool {
	changed := false
	for _, fi := range fileIdxs {
		if fi < 0 || fi >= m.NumFiles() || p.files[fi].Fetch != FetchIfNeeded {
			continue
		}
		p.files[fi].Fetch = FetchAlways
		changed = true
	}
	if changed {
		p.par2Recovered = true
		p.recompute(m)
	}
	return changed
}

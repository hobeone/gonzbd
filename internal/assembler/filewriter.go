package assembler

import (
	"os"
	"slices"

	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/telemetry"
)

// articleID identifies an article as it travels with its bytes to the write
// that lands them, so the write can say which article it carried.
//
// artIdx is identity: it is what FileWriter.seenDone / seenFailed key duplicate
// handling on, and what FileWriter.noteWritten puts on a durability.WrittenArticle,
// so it must match the queue's numbering. msgID travels alongside it for
// logging and telemetry. Neither reaches the queue from here: this package has
// no ack path in either direction.
type articleID struct {
	msgID  string
	artIdx int32
}

// sameArticle reports whether two identities name the same article.
//
// The index alone decides. msgID travels with the article for logging and
// telemetry and is deliberately NOT compared: identity is the manifest index,
// which is what FileWriter's seen-sets key on. Comparing the pair as well would
// give the assembler a second, finer notion of sameness than the one its
// accounting uses, and the two could disagree.
func (a articleID) sameArticle(b articleID) bool { return a.artIdx == b.artIdx }

// FileWriter owns one target file: its handle and its write path. It has no
// authority over anything externally visible.
//
// It cannot ack an article, record a CRC part, decide a file is complete, or
// truncate. Those decisions moved to durability.Barrier, which is the only
// component that knows whether an fsync has happened. The writer's entire
// contract to the outside world is: bytes it reports from Drain reached
// WriteAt without error, and everything else is its own business.
//
// This continues the direction #358 started rather than reversing it. That
// change moved the ack from accept to WriteAt — Decoded to Written. This one
// moves it from Written to Durable, which is the step #358 explicitly declined
// ("this does not defer acks to fsync — that would push every ack to file
// completion"). That objection was correct at the time: Sync ran only in
// finalizeFile, so acking after fsync really did mean acking at file
// completion. The barrier removes the premise by fsyncing on a cadence, so
// the cost is one checkpoint interval rather than a whole file.
//
// Single-goroutine, no locking, like everything else the assembler worker
// owns (X1). The barrier reaches it through the worker's control-message
// channel, never directly — see syncTarget.
type FileWriter struct {
	handle *os.File
	path   string
	key    fileKey

	// written accumulates the articles whose bytes reached WriteAt without
	// error and have not yet been reported by a Drain. It is the ONLY evidence
	// the barrier has, so nothing may be appended here that has not come back
	// from a successful writeAt (S2).
	written []durability.WrittenArticle

	// reported holds what a Drain has already handed to the barrier while its
	// cycle awaits confirmation after a successful Sync. Drain returns it
	// again across post-Sync failures, and Confirm is what discards it once
	// the cycle lands; a failed Sync poisons and releases it (#760).
	//
	// A successful fsync makes the bytes durable, but the barrier still has to
	// commit the run rows and ack the articles, and both can fail after it.
	// Retaining the report across those post-Sync failures is R12's
	// at-least-once delivery: without it, a retry after a failed commit or ack
	// drains nothing, so FinalizeFile's bound sits below bytes that are
	// genuinely on disk (#342, #350).
	//
	// By contrast, a failed Sync itself must NOT retain the report (#760).
	// Linux reports a writeback error to a file descriptor once (errseq) and
	// marks the failed pages clean, so a later fsync on the same handle can
	// return nil even though the bytes never reached disk. On a Sync failure,
	// poisonSync discards reported and written and rolls their articles back
	// into poisoned so the caller returns them to Outstanding.
	reported []durability.WrittenArticle

	// seenDone and seenFailed keep duplicate handling idempotent (R12).
	//
	// Both are keyed on ArtIdx, the article's manifest index, rather than its
	// Message-ID. ArtIdx has no value that doubles as "absent" — an NZB may
	// omit the Message-ID, but never the index — so every article has a key
	// these maps can hold, and there is no empty-key class to guard against.
	//
	// Both are membership-only: handleSuccessArticle's duplicate arm releases
	// the second copy's buffer and returns, because either way its bytes are
	// redundant and re-writing them would be a second WriteAt over the same
	// range.
	seenDone   map[int32]struct{}
	seenFailed map[int32]struct{}

	// owned records which article owns each byte range this writer has
	// WRITTEN: first writer wins (#383, #759). A range is claimed only after
	// its write returned nil, so an article whose write faulted owns nothing and
	// its retry, or a rival at the same range, is accepted. acceptArticle
	// refuses an arrival that intersects another article's range before Accept
	// is called.
	//
	// Entries are never removed, including by a failed Sync's rollback (see
	// rollbackSyncedArticle). The question is "who wrote these bytes", not
	// "who currently holds a part", so owned is intentionally not equal to
	// seenDone. Residency is the writer's, so this is per-open-episode.
	owned ownedRanges

	// poisoned accumulates the articles a failed Sync rolled back (#760), for
	// releaseSyncRollback to route to Outstanding via takePoisoned.
	poisoned []int32
	// partsWritten counts how many of the file's parts have been accounted
	// for, whether by a successful accept or by a permanent failure.
	//
	// It lives here rather than on openFile because it is DERIVED from
	// seenDone and seenFailed — it is their combined size. Kept one struct
	// away from its source and maintained by four external call sites, every
	// defect it produced was the same one: the count and the record moved at
	// different moments, and something in between observed the disagreement.
	//
	// Recomputing it on demand would be O(n) per article, so it stays a cached
	// aggregate. What changes is that only this file may move it, and each
	// mover applies a complete transition — admitAccepted, admitRetryOfFailed,
	// admitPermanentFailure, fail — so a partial application is not
	// expressible rather than merely not written.
	partsWritten int

	// writeAt is handle.WriteAt in production. Tests override it before first
	// use to inject storage faults, mirroring how diskProbe.statfs is
	// overridden. Never reassigned after the writer is in service, so the
	// single-goroutine ownership rule covers it.
	writeAt func(p []byte, off int64) (int, error)

	// syncFile is handle.Sync in production, on the same terms as writeAt.
	//
	// It exists so that REMOVING the fsync is detectable. Deleting
	// w.handle.Sync() used to leave the entire suite green, crash suite
	// included, because no unprivileged test can tell a synced file from an
	// unsynced one — dirty page cache survives process death, so only a
	// machine-level power cut distinguishes them. The seam does not pin
	// fsync-to-platter and cannot; it pins that the syscall is on the path at
	// all, which is the part that was silently deletable.
	syncFile func() error

	// closeFile is handle.Close in production, on the same terms as writeAt
	// and syncFile.
	//
	// It exists because the close arm of drainAndClose is documented as
	// load-bearing — on network-backed mounts the close is frequently where a
	// deferred write error first surfaces — and had no coverage at all. The
	// only way to fail a real Close is to close the handle first, which makes
	// Sync fail one line earlier and never reaches the arm under test.
	closeFile func() error
}

// newFileWriter wraps an already-open handle.
func newFileWriter(handle *os.File, path string, key fileKey) *FileWriter {
	w := &FileWriter{
		handle:     handle,
		path:       path,
		key:        key,
		seenDone:   make(map[int32]struct{}),
		seenFailed: make(map[int32]struct{}),
	}
	w.writeAt = handle.WriteAt
	w.syncFile = handle.Sync
	w.closeFile = handle.Close
	return w
}

// noteWritten records an article whose bytes reached WriteAt without error.
//
// Every append to w.written goes through here, so there is exactly one place
// where "this article is Written" is asserted, and it is only ever reached
// from below a successful writeAt. That is the structural half of S2: the
// claim cannot be made from an accept path because no accept path can call
// this.
func (w *FileWriter) noteWritten(id articleID, off int64, n int, crc32 uint32) {
	w.written = append(w.written, durability.WrittenArticle{
		FileIdx: int32(w.key.fileIdx), //nolint:gosec // G115: file counts are far below int32
		ArtIdx:  id.artIdx,
		Offset:  off,
		Length:  int32(n), //nolint:gosec // G115: an article's decoded length is far below int32
		CRC32:   crc32,
	})
}

// writtenSoFar returns the articles reported Written since the last Drain,
// without draining. Used by tests to assert what a write has claimed.
func (w *FileWriter) writtenSoFar() []durability.WrittenArticle { return w.written }

// unconfirmed returns the articles a Drain has reported that no Confirm has
// yet released. Used by tests to assert the report survives a successful Sync
// until Confirm and is discarded by a failed Sync (#760).
func (w *FileWriter) unconfirmed() []durability.WrittenArticle { return w.reported }

// rollbackPart undoes the part and seen-set state an admitted article holds,
// without deciding what becomes of the article. The caller owns that.
//
// fail is the only caller, and it is the whole give-back — routeAcceptFailure
// no longer routes a decrement of its own around it. It used to reach a
// giveBackUntrackedPart branch here for an article with no Message-ID, since
// no Message-ID-keyed map could record one. Keying on ArtIdx instead means
// every article has a key this function's maps can hold, and that branch is
// gone rather than reachable only from a test. giveBackUntrackedPart itself is
// gone too: with fail no longer returning early on any identity, it was the
// only caller that decrement needed.
func (w *FileWriter) rollbackPart(artIdx int32) {
	_, wasDone := w.seenDone[artIdx]
	_, wasFailed := w.seenFailed[artIdx]
	delete(w.seenDone, artIdx)
	// An article already counted as permanently FAILED keeps its count:
	// admitPermanentFailure counted it, and a later retry whose write fails
	// must not decrement what a different code path added. Only an article
	// counted as WRITTEN loses its count here.
	if wasDone && !wasFailed && w.partsWritten > 0 {
		w.partsWritten--
	}
}

// fail rolls one article back to never-having-arrived after its write failed.
//
// It clears seenDone and gives the article's part back, and does NOT set
// seenFailed. A failed WRITE is a storage condition, and A1 forbids resolving
// it against the article: the bytes are still available on the server and the
// article is still wanted. Recording it as failed made a redelivery take the
// "retry of an article already counted as failed" branch, which writes the
// bytes but does not count them, so the file's part total was permanently one
// short of the truth for every rolled-back article.
//
// Returning the article to Outstanding is the caller's: its Emitted bit is
// still set from dispatch and ForEachUnfinishedArticle skips a set Emitted
// bit, so an article merely dropped here is stranded. fail has two callers —
// `git grep -n '\.fail(' -- 'internal/assembler/*.go' ':!*_test.go'` finds 3 lines,
// this comment and the calls in writeOne and rollbackSyncedArticle. After
// writeOne, the one article is the one whose Accept returned the error, and
// routeAcceptFailure reports it; after rollbackSyncedArticle, it is in
// w.poisoned and releasePoisoned reports it.
func (w *FileWriter) fail(id articleID) {
	w.rollbackPart(id.artIdx)
}

// parts reports how many of the file's parts have been accounted for. The
// caller compares it to FileInfo.TotalParts to decide the file is complete.
func (w *FileWriter) parts() int { return w.partsWritten }

// admitAccepted takes an article on as a part of this file, before its bytes
// are handed to Accept.
//
// The seenDone record and the count are applied together, and that pairing is
// the point of the method. fail decides whether to give a part back by asking
// whether the article is in seenDone, so seenDone is the de facto record of
// "this article holds a part". While the two lived at separate call sites the
// count could be applied after a write that had already rolled it back, and
// every transient accept-time write fault undercounted the file by one,
// permanently, putting partsWritten >= TotalParts out of reach.
func (w *FileWriter) admitAccepted(artIdx int32) {
	w.seenDone[artIdx] = struct{}{}
	w.partsWritten++
}

// admitRetryOfFailed records a redelivery of an article already counted as
// permanently failed, WITHOUT counting it again.
//
// Its bytes are still written, because they are still the file's content, but
// the part was charged when the article was failed and charging it twice would
// carry the file past TotalParts.
func (w *FileWriter) admitRetryOfFailed(artIdx int32) {
	w.seenDone[artIdx] = struct{}{}
}

// admitPermanentFailure counts a permanently failed article toward the file's
// part total, and reports whether it was newly admitted.
//
// It records no ack. A permanent failure is the queue's to record via
// Job.MarkArticleFailed (R10). What is left here is the dedup that keeps
// partsWritten from overshooting TotalParts, which is purely local
// bookkeeping.
func (w *FileWriter) admitPermanentFailure(artIdx int32) bool {
	if _, dup := w.seenFailed[artIdx]; dup {
		return false
	}
	_, alreadyCounted := w.seenDone[artIdx]
	w.seenFailed[artIdx] = struct{}{}
	// An article already counted as a success must not increment the part
	// total a second time.
	if alreadyCounted {
		return false
	}
	w.partsWritten++
	return true
}

// failPermanent records an article this package will never write, keeping its
// count toward the file's part total.
//
// The article is resolved elsewhere — Options.OnArticleRejected carries it to
// the queue — so it will not arrive again, and a file that stopped counting it
// could never reach TotalParts. That is the opposite of fail's case, where the
// article is coming back.
func (w *FileWriter) failPermanent(artIdx int32) {
	delete(w.seenDone, artIdx)
	w.seenFailed[artIdx] = struct{}{}
}

// takePoisoned returns and clears the articles a failed Sync rolled back since
// the last call.
//
// Taken rather than read, because each set must be routed exactly once: the
// caller returns them to Outstanding, and reporting one twice would clear an
// Emitted bit a later dispatch had legitimately set.
func (w *FileWriter) takePoisoned() []int32 {
	out := w.poisoned
	w.poisoned = nil
	return out
}

// Accept writes one article's bytes through writeOne.
//
// It takes ownership of data and returns it to the decoder pool on every path,
// including failure, so a caller never has to reason about who frees it.
//
// A returned error is always a *storagefault.Fault. It reports that STORAGE
// failed, never that the article did (A1, R19).
//
// The caller must not discard it. Dropping it neither stalls the job nor
// returns the article to Outstanding — see fail's doc for why absence from a
// Drain is not enough on its own — and the file goes on to complete over bytes
// that never landed.
func (w *FileWriter) Accept(id articleID, off int64, data []byte, crc32 uint32) error {
	return w.writeOne(id, off, data, crc32)
}

// writeOne writes a single article and reports it Written on success.
func (w *FileWriter) writeOne(id articleID, off int64, data []byte, crc32 uint32) error {
	telemetry.DiskWrites.Add(1)
	telemetry.DiskWriteBytes.Add(int64(len(data)))
	_, err := w.writeAt(data, off)
	if data != nil {
		defer decoder.PutBuffer(data)
	}
	if err != nil {
		telemetry.PipelineErrors.Add(telemetry.ErrClassDiskWriteError, 1)
		w.fail(id)
		return storagefault.Classify("write", w.path, err)
	}
	w.owned.claim(Range{Off: off, Len: int64(len(data))}, id)
	w.noteWritten(id, off, len(data), crc32)
	return nil
}

// Drain returns the articles whose bytes reached WriteAt without error since
// the last confirmed cycle (and not poisoned by a failed Sync) — NOT merely
// since the last Drain.
//
// The distinction is load-bearing and this comment used to get it wrong. take()
// re-reports everything a Drain has already handed over across post-Sync
// failures until Confirm releases it (or a failed Sync poisons and rolls it
// back, #760), which is R12's at-least-once delivery. Reading "since the last
// call" as the contract makes the re-report look redundant, and removing it
// destroys bytes on a retried finalize after a failed commit or ack: the retry
// drains nothing, so the bound FinalizeFile trims to sits below bytes genuinely
// on disk. See take() and the FileWriter.reported field doc, which are the
// authority.
//
// It writes nothing: Accept writes every article before reporting it, so the
// return value is the barrier's only evidence and holds only bytes that
// reached WriteAt (S2). The error result stays because durability.SyncTarget
// declares it.
//
// # Why there is no context here
//
// There was one, and it guarded the entry with ctx.Err(). Nothing could reach
// the guard: every caller is the assembler's worker goroutine, which had no
// context to pass and supplied context.Background().
//
// Wiring the real one through was the other repair and it is the wrong one.
// The barrier treats an error this returns as a storage condition unless it
// names itself otherwise — a *storagefault.Fault, ErrFileNotOpen or
// ErrTargetUnavailable — and anything else is classified and routed to
// Stallable. A cancelled context is none of those, so it would park a healthy
// job on a device fault that does not exist. That is the A1 conflation the rest
// of this package works to avoid, arriving through a guard that looks like
// caution. See durability.ErrTargetUnavailable for the boundary rule.
//
// Nothing is left unbounded by removing it. The operation is bounded at the
// submit side by barrierOpTimeout, which is where a caller that has given up
// stops waiting; and once the op reaches the worker there is nothing to
// abandon it for, since the worker owns the handle and a half-finished drain
// leaves the file's state undescribed.
func (w *FileWriter) Drain() ([]durability.WrittenArticle, error) {
	return w.take(), nil
}

// take moves the newly written articles into the unconfirmed set and returns
// the whole of it — everything written since the last confirmed cycle (and not
// poisoned by a failed Sync).
//
// The split between w.written and w.reported is what keeps an article written
// BETWEEN a Drain and a successful Sync from being discarded when that cycle's
// Confirm runs: it stays in w.written, which Confirm does not touch, so the
// next Drain reports it. Folding the two together would silently drop it —
// covered by the fsync, but never claimed, so never acked.
func (w *FileWriter) take() []durability.WrittenArticle {
	w.reported = append(w.reported, w.written...)
	w.written = nil
	return slices.Clone(w.reported)
}

// Sync fsyncs the handle. Until this returns nil, nothing a preceding Drain
// reported may be claimed (S1).
//
// If the fsync fails, poisonSync discards the retained report (and any
// articles written since the Drain) and rolls them back into poisoned so the
// caller returns them to Outstanding (#760): Linux reports a writeback error
// once per file descriptor (errseq) and marks the failed pages clean, so a
// retry fsync on the same handle can return nil even though the bytes never
// reached disk.
//
// It takes no context, for the reason Drain documents at length: the guard was
// unreachable, and reaching it would have turned a cancellation into a storage
// fault and stalled a healthy job.
func (w *FileWriter) Sync() error {
	if err := w.syncFile(); err != nil {
		w.poisonSync()
		return storagefault.Classify("sync", w.path, err)
	}
	// The report is deliberately NOT discarded on a successful fsync. The
	// fsync makes the bytes durable, but the runs are not yet committed and
	// nothing is acked, and either of those can still fail. Confirm is what
	// releases it once the cycle lands.
	return nil
}

// poisonSync discards the unconfirmed report and rolls every article in
// w.reported and w.written back into w.poisoned so the caller returns them to
// Outstanding (#760).
func (w *FileWriter) poisonSync() {
	for _, a := range w.reported {
		w.rollbackSyncedArticle(a.ArtIdx)
	}
	for _, a := range w.written {
		w.rollbackSyncedArticle(a.ArtIdx)
	}
	w.reported = nil
	w.written = nil
}

// rollbackSyncedArticle rolls the article back into w.poisoned if it is not
// already pending there.
//
// It leaves w.owned alone: the article wrote its range, so it keeps it. Its
// redelivery carries the same artIdx and is accepted to rewrite the bytes, and
// a different article intersecting the range is refused at acceptArticle.
func (w *FileWriter) rollbackSyncedArticle(artIdx int32) {
	if slices.Contains(w.poisoned, artIdx) {
		return
	}
	w.fail(articleID{artIdx: artIdx})
	w.poisoned = append(w.poisoned, artIdx)
}

// Confirm releases the drain report, and is called only once the barrier has
// committed the runs and acked the articles.
//
// It is what bounds the retained set across post-Sync failures. Drain
// re-reports across any failure after a successful Sync (while a failed Sync
// poisons and releases the report, #760), so without a confirmation the set
// would grow to every article ever written to this file and every later
// checkpoint would redo all of it.
//
// Deliberately cannot fail. It records that work already succeeded, so there
// is no outcome for a caller to handle: a missed Confirm costs one redundant
// re-report, which R12 makes the barrier absorb, while a Confirm that could
// fail would need its own recovery path for a cycle that has already landed.
func (w *FileWriter) Confirm() {
	w.reported = nil
}

// Stat returns the file's size as it is now. It is S7's validity stamp — which
// a resume checks the file against — so it must be read after the Sync it
// describes.
//
// It used to return the modification time as the stamp's second half. See
// durability.SyncTarget.Stat for why that half was deleted rather than left
// unread: with no recomputation left, an mtime mismatch's only remaining
// response would be a full re-download of an intact file.
func (w *FileWriter) Stat() (size int64, err error) {
	fi, err := w.handle.Stat()
	if err != nil {
		return 0, storagefault.Classify("stat", w.path, err)
	}
	return fi.Size(), nil
}

// Truncate trims the file to n bytes.
//
// Only ever called with a bound derived from the durable runs, never from
// this run's high-water mark — see the assembler's completion path. S6 permits
// metadata to shrink a file and never to grow it, so a target above the file
// on disk is refused rather than clamped: growing appends zeros, which asserts
// content that exists nowhere.
func (w *FileWriter) Truncate(n int64) error {
	if n < 0 {
		return nil
	}
	fi, err := w.handle.Stat()
	if err != nil {
		return storagefault.Classify("stat", w.path, err)
	}
	if n >= fi.Size() {
		return nil
	}
	if err := w.handle.Truncate(n); err != nil {
		return storagefault.Classify("truncate", w.path, err)
	}
	return nil
}

// Close releases the handle.
//
// The error reports STORAGE: on network-backed mounts the close is where a
// deferred write error first surfaces, and drainAndClose classifies it into the
// barrier's fault handling.
func (w *FileWriter) Close() error {
	if err := w.closeFile(); err != nil {
		return storagefault.Classify("close", w.path, err)
	}
	return nil
}

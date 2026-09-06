package assembler

import (
	"os"
	"slices"

	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/telemetry"
)

// FileWriter owns one target file: its handle, its share of the write cache,
// its coalescing, and its pre-allocation. It has no authority over anything
// externally visible.
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

	// wc is the assembler-wide write cache, and fb is this file's entry in
	// it. The brief's shape was a per-writer *fileBuf; the shared cache comes
	// with it because the memory bound in B2 is global across files, not
	// per-file — forceFlushLargest has to compare files against each other,
	// and the coalescing scratch buffer is reused across all of them. A
	// per-writer cache would make the bound per-file and multiply the scratch
	// allocation by the number of open files.
	wc *writeCache

	// written accumulates the articles whose bytes reached WriteAt without
	// error and have not yet been reported by a Drain. It is the ONLY evidence
	// the barrier has, so nothing may be appended here that has not come back
	// from a successful writeAt (S2).
	written []durability.WrittenArticle

	// reported holds what a Drain has already handed to the barrier but whose
	// CYCLE has not been confirmed. Drain returns it again, and Confirm — not
	// Sync — is what finally discards it.
	//
	// The distinction is the whole point. A successful fsync makes the bytes
	// durable, but the barrier still has to commit the run rows and ack the
	// articles, and both can fail after it. Releasing on the
	// fsync covered only the first of the failures this retention exists for,
	// while the doc below claimed all three.
	//
	// This is R12's at-least-once delivery, which SyncTarget.Drain explicitly
	// blesses ("re-reporting an article a previous Drain already returned is
	// permitted and expected"), and it is load-bearing rather than tidy. A
	// barrier that drains and then fails — the sync, the run commit, the
	// truncate — used to lose the report outright. For a file still being
	// written that only cost a re-fetch. For a COMPLETED file it costs bytes:
	// the retry drains nothing, so the bound FinalizeFile trims to
	// sits below bytes that are genuinely on disk, and the truncate destroys
	// them. That is the #342/#350 family arriving through the recovery path.
	//
	// An earlier version of this comment argued the opposite — that retaining
	// the report would "trade a bounded cost for an unbounded slice". The
	// bound it was worried about is real but small: entries accumulate only
	// between CONFIRMED cycles, and a job whose barriers are failing stalls
	// (R19) and stops writing.
	reported []durability.WrittenArticle

	// seenDone and seenFailed keep duplicate handling idempotent (R12). They
	// moved here from openFile because the writer owns the cache the first
	// copy may still be sitting in.
	//
	// Both are keyed on ArtIdx, the article's manifest index, rather than its
	// Message-ID. ArtIdx has no value that doubles as "absent" — an NZB may
	// omit the Message-ID, but never the index — so every article has a key
	// these maps can hold, and there is no empty-key class to guard against.
	//
	// Both are membership-only. seenDone used to carry the offset the article
	// was accepted at, and this comment said the duplicate branch needed it in
	// order to ask wc.buffered whether the first copy had left the cache. It
	// never asked: handleSuccessArticle's duplicate arm releases the buffer
	// and returns, because the answer does not change what it does — either
	// way the second copy's bytes are redundant and re-writing them would be a
	// second WriteAt over the same range. The offset was written and never
	// read.
	seenDone   map[int32]struct{}
	seenFailed map[int32]struct{}

	// acceptedAt records which article OWNS each offset this writer has
	// accepted bytes for. It is the collision detector (#383).
	//
	// Detection used to live in writeCache.buffer, which decided a collision
	// by finding the first article still resident in fb.articles. That answers
	// "are these bytes still unwritten", which is a question about caching,
	// not about what the file has been told. buildContiguousRun deletes each
	// article it flushes and advances fb.writeCursor past it, so for an
	// IN-ORDER download the first article was gone before the duplicate
	// arrived: the duplicate was buffered, never picked up by a run, and
	// written over the first at drain time. No detection, no roll-back, both
	// counted, file completes. Two further paths missed it — caching disabled
	// (write_cache_size has no validation floor and accepts zero) and
	// zero-length articles, which buffer refuses. Both route to writeOne.
	//
	// A collision is decided by IDENTITY, not by occupancy: an offset already
	// owned by THIS article is a re-accept, not a collision. That is what
	// makes the index safe without any removal.
	//
	// The index answers TWO questions, and they resolve opposite ways. Whether
	// the owner has been reported Written decides which article loses:
	// offsetSettledBy refuses the ARRIVAL when it has, because the incumbent's
	// bytes already back a durable claim; Accept displaces the INCUMBENT when
	// it has not, because a buffered article contradicts nothing. Only the
	// second reaches failDisplaced, and acceptArticle has already refused
	// everything the first covers before Accept is called.
	//
	// The occupancy form would have needed it. An article whose write faults
	// is rolled back and re-dispatched, and comes back at the same offset —
	// req.ArtIdx is the manifest index, so the redelivered articleID is
	// identical — and without removal every such retry would report a
	// collision with itself. Removal used to be additionally blocked for one
	// class of article: fail returned early on an empty Message-ID, so an
	// untracked article's entry would never have been removed, which was that
	// false positive arriving on the one path the mechanism could not reach.
	// fail no longer has that early return, but removal still is not how
	// acceptedAt behaves — see below, entries are never removed by design,
	// independent of what fail does. Identity comparison answers all of it and
	// leaves seenDone alone.
	//
	// Entries are never removed, and that is deliberate rather than a leak.
	// The question is "who owns this offset", not "who currently holds a
	// part", so the index is intentionally NOT equal to seenDone: a
	// write-faulted article keeps its entry here while losing its seenDone
	// one. Residency is the writer's, so this is per-open-episode — a
	// collision spanning a close-handles cycle or a restart is invisible,
	// exactly as seenDone's duplicate handling is.
	acceptedAt map[int64]offsetOwner

	// postAnomalyReported latches once this file has raised a job-level
	// warning about a collision. See faultedArticle.firstCollision.
	postAnomalyReported bool

	// faulted accumulates the articles a failed write rolled back, for the
	// caller to route to Outstanding. See fail.
	faulted []faultedArticle

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

// faultedArticle is one article a failed write rolled back, and how the caller
// must dispose of it.
//
// It used to carry an uncount flag as well, reporting whether the article's
// part had to be given back. That flag was itself derived state stored one
// struct away from its source — the answer is exactly "was it in seenDone and
// not in seenFailed", which fail already knows at the moment it appends here.
// fail now applies the give-back directly and the flag is gone, so there is no
// window in which the two can disagree.
//
//nolint:govet // field order follows the doc's narrative, not alignment
type faultedArticle struct {
	// displaced marks an article a later article at the same offset pushed
	// out of the cache, rather than one a write failed on. See failDisplaced.
	displaced bool
	id        articleID

	// offset and displacedBy carry the diagnosis, and are meaningful only
	// when displaced is set. They exist because the assembler has to tell the
	// user WHAT it saw — which byte range, and which two segments claimed it —
	// and the writer is the only layer that knows.
	//
	// They are set by the same statement that sets displaced, which is the
	// whole point of failDisplaced no longer patching this record after the
	// fact: "displaced with no displacer" is not a state a caller can reach.
	offset      int64
	displacedBy articleID

	// firstCollision marks the one record per file that should raise the
	// job-level warning, so an obfuscated post with N colliding segments
	// reports once rather than overwriting job.Warning N times with the same
	// sentence. The per-ARTICLE report is unaffected — every displaced article
	// still goes to OnArticleRejected.
	firstCollision bool
}

// offsetOwner is the article that owns one offset, and whether its bytes have
// been reported Written.
//
// written is latched by noteWritten and is never cleared while the entry
// survives, which is the whole reason it lives here rather than being derived
// from w.written/w.reported.
//
// "While the entry survives" is the load-bearing qualifier, and it was missing
// when this comment was first written: Accept re-recorded the arrival as owner
// unconditionally, so a re-accept by the owner ITSELF replaced the entry with a
// fresh one and dropped the latch. Accept now rewrites the entry only when the
// owner actually changes.
//
// Those two slices are the barrier's pending evidence: Confirm empties them
// once the articles have been acked durable. An article that has been acked is
// the strongest possible claim on its offset, and a claim derived from the
// pending slices would read it as no claim at all — so a collision arriving
// after a checkpoint would displace an article the queue has already recorded
// as durably written. That is the same double-disposition defect one
// checkpoint later, and the derived form cannot see it.
type offsetOwner struct {
	id      articleID
	written bool
}

// newFileWriter wraps an already-open handle.
func newFileWriter(handle *os.File, path string, key fileKey, wc *writeCache) *FileWriter {
	w := &FileWriter{
		handle:     handle,
		path:       path,
		key:        key,
		wc:         wc,
		seenDone:   make(map[int32]struct{}),
		seenFailed: make(map[int32]struct{}),
		acceptedAt: make(map[int64]offsetOwner),
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
//
// It also latches the offset's owner as written, which is what makes the
// offset settled against a later article claiming it. Latched HERE for the
// same reason the append is here: both assert "these bytes are the file's
// content at this offset", and applying one without the other is the
// derived-state split #375 was about.
func (w *FileWriter) noteWritten(id articleID, off int64, n int, crc32 uint32) {
	if owner, taken := w.acceptedAt[off]; taken && owner.id.sameArticle(id) {
		owner.written = true
		w.acceptedAt[off] = owner
	}
	w.written = append(w.written, durability.WrittenArticle{
		FileIdx: int32(w.key.fileIdx), //nolint:gosec // G115: file counts are far below int32
		ArtIdx:  id.artIdx,
		Offset:  off,
		Length:  int32(n), //nolint:gosec // G115: an article's decoded length is far below int32
		CRC32:   crc32,
	})
}

// writtenSoFar returns the articles reported Written since the last Drain,
// without draining. Used by tests to assert that a merely-buffered article has
// made no claim.
func (w *FileWriter) writtenSoFar() []durability.WrittenArticle { return w.written }

// unconfirmed returns the articles a Drain has reported that no Sync has yet
// confirmed. Used by tests to assert the report survives a failed Sync.
func (w *FileWriter) unconfirmed() []durability.WrittenArticle { return w.reported }

// failDisplaced resolves an article a LATER article displaced from the same
// offset, which is a different disposition from a write that failed.
//
// Its bytes are gone and nothing will write them, but it must not be returned
// to Outstanding: the collision is a property of what the server sent — two
// segments claiming one offset — so re-fetching it produces the same
// collision, and the re-fetched copy displaces the article that displaced it.
// Observed as a [0 1 0 1 0] ping-pong when this went through the un-written
// path.
//
// So it is resolved permanently failed instead, which is the same disposition
// handleLateDuplicate reaches for an article that can never be written now or
// later. See releaseFaulted.
//
// # A resolved article must be COUNTED for a part, not merely keep one
//
// TotalParts counts manifest segments, so two segments claiming one offset are
// two parts the file waits for: one supplies the bytes, the other is
// permanently failed, and a file that stopped counting the loser could never
// reach TotalParts. Giving the part back while resolving the article left the
// file permanently one short — OnFileComplete never fired and the job sat at
// 100% with nothing outstanding, across restarts (#386).
//
// admitPermanentFailure rather than failPermanent, because the incumbent is
// not guaranteed to hold a part to keep. failPermanent only KEEPS one, which
// suffices at acceptArticle's call sites because admitAccepted ran a statement
// earlier. Here it does not: acceptedAt entries are never removed, so a
// write-faulted article keeps its offset ownership after fail has taken its
// part and its seenDone entry away, and a later arrival at that offset
// displaces a stale owner holding nothing. failPermanent would count nothing
// for it and the file would wedge exactly as before. admitPermanentFailure
// counts if and only if the article is not already counted.
//
// It also leaves the seenDone entry in place, and both duplicate-handling
// readers — handleSuccessArticle and handleLateDuplicate — test seenDone
// before seenFailed, so a redelivery of the loser takes the duplicate arm
// rather than being re-written and displacing the winner in turn.
// admitPermanentFailure itself reads the two in the opposite order, which is
// how it tells an already-counted article from a new one.
//
// # Precondition: the incumbent must not have been reported Written
//
// This is only correct for an article that made no durability claim. One that
// reached noteWritten is in w.written, or in w.reported after a Drain, or
// already acked and gone from both after a Confirm — and in every one of those
// the barrier will record it. Rolling it back here as well gives one article
// two terminal dispositions, and lets the displacer overwrite bytes a run
// records the incumbent's CRC over, so the record describes a range the file
// no longer holds.
//
// acceptArticle enforces the precondition by refusing the ARRIVAL when the
// offset is settled, so this is reached only from the cache-eviction case it
// was written for. Do not add a caller without checking offsetSettledBy first.
// It does NOT delegate to fail. It used to, and then reached back to
// specialize the record fail had appended:
//
//	w.fail(id)
//	if n := len(w.faulted); n > 0 && w.faulted[n-1].id == id {
//		w.faulted[n-1].displaced = true
//	}
//
// That is the shape #375 removed from faultedArticle — a field applied to a
// record after it was built, by a call site that had to find it again — and it
// carried a live defect. fail returned early on an empty Message-ID at the
// time, so for an untracked article nothing was appended, the positional
// guard matched nothing, and the whole displacement was dropped: no report,
// and nothing resolved the article in either direction. (That early return is
// gone now — see fail's doc — but the reason failDisplaced still does not
// delegate to it is independent: fail rolls a part back, and a displaced
// article keeps its part, so delegating would decrement the wrong way
// regardless of identity.) Keeping its part is now the correct outcome and no
// longer part of the complaint; what was missing was the faulted record that
// makes routeFaulted report it at all, without which it stayed Emitted
// forever and no later run re-dispatched it.
//
// Building the record in one statement makes a half-specialized faultedArticle
// unrepresentable, which is what lets Accept's diagnosis fields be added to it
// without reopening that window.
func (w *FileWriter) failDisplaced(id articleID, off int64, by articleID) {
	w.admitPermanentFailure(id.artIdx)
	w.faulted = append(w.faulted, faultedArticle{
		id:             id,
		displaced:      true,
		offset:         off,
		displacedBy:    by,
		firstCollision: w.notePostAnomaly(),
	})
}

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

// fail rolls one article back to never-having-arrived, and records it so the
// caller can return it to Outstanding.
//
// # Rolled back, not marked failed
//
// It clears seenDone and — unlike the version this replaces — does NOT set
// seenFailed. A failed WRITE is a storage condition, and A1 forbids resolving
// it against the article: the bytes are still available on the server and the
// article is still wanted. Recording it as failed made a redelivery take the
// "retry of an article already counted as failed" branch, which writes the
// bytes but does not count them, so the file's part total was permanently one
// short of the truth for every rolled-back article.
//
// # Recording it is the whole point
//
// Absence from Drain's return does NOT leave the article Outstanding. Its
// Emitted bit is still set from dispatch and ForEachUnfinishedArticle skips a
// set Emitted bit, so an article that is merely dropped here is stranded for
// the life of the process: neither Done, nor Failed, nor Outstanding, and only
// a restart's ClearAllEmitted recovers it.
//
// Every batch failure rolls back MORE articles than the one that triggered it
// — a coalesced run loses every part, a drain loses everything after the
// write that failed — and the caller was given only one article index to
// route. So the set is accumulated here and taken by the caller, rather than
// inferred from an error. A cache displacement adds to the same set without
// coming through this function, since it resolves its article rather than
// rolling it back.
//
// # It gives the part back itself
//
// The give-back used to live in Assembler.releaseFaulted, one struct away from
// the seenDone entry it is derived from, carried across by a stored uncount
// flag. Both moved here: the delete and the decrement are now one statement
// pair, so an article cannot lose its seenDone record while keeping its part.
//
// The rollback is exempt from the count's own > 0 clamp by construction rather
// than by luck. An article only loses a part if it held one, which means it
// was in seenDone and not in seenFailed, which means partsWritten counted it.
func (w *FileWriter) fail(id articleID) {
	w.rollbackPart(id.artIdx)
	w.faulted = append(w.faulted, faultedArticle{id: id})
}

// parts reports how many of the file's parts have been accounted for. The
// caller compares it to FileInfo.TotalParts to decide the file is complete.
func (w *FileWriter) parts() int { return w.partsWritten }

// offsetSettledBy reports the article that owns off when that owner has
// already made a durability claim, so the ARRIVING article must be refused
// rather than allowed to displace it.
//
// This is the half of collision handling that failDisplaced cannot do. That
// function was written when the only caller was the write cache's eviction,
// where the incumbent was still buffered and had made no claim — its own doc
// said so. Detecting the FLUSHED case (#383) falsified that precondition: the
// incumbent may already be in w.written, or in w.reported after a Drain handed
// it to the barrier.
//
// Failing it there gives one article two terminal dispositions at once —
// routeFaulted resolves it permanently failed while the barrier still holds it
// as evidence and acks it durable. And the ack would be a lie: the drain
// reports this article's CRC at this offset, and the barrier records a run
// over exactly that range, so letting the arrival overwrite it leaves a
// record describing bytes the file no longer holds.
//
// So an offset whose owner has been reported Written is SETTLED, and the later
// article loses. That is the opposite of the cache-resident case, deliberately:
// there the incumbent has no claim to protect, so the arrival wins and the
// incumbent is resolved permanently failed. Which article loses differs; the
// accounting does not, since both dispositions keep the loser counted.
func (w *FileWriter) offsetSettledBy(off int64, arriving articleID) (articleID, bool) {
	owner, taken := w.acceptedAt[off]
	if !taken || owner.id.sameArticle(arriving) || !owner.written {
		return articleID{}, false
	}
	return owner.id, true
}

// notePostAnomaly reports whether this file has yet raised a job-level warning
// about a collision, latching so it raises exactly one. See
// faultedArticle.firstCollision, which is the same latch on the other
// disposition.
func (w *FileWriter) notePostAnomaly() bool {
	if w.postAnomalyReported {
		return false
	}
	w.postAnomalyReported = true
	return true
}

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

// takeFaulted returns and clears the articles rolled back since the last call.
//
// Taken rather than read, because each set must be routed exactly once: the
// caller returns them to Outstanding, and reporting one twice would clear an
// Emitted bit a later dispatch had legitimately set.
func (w *FileWriter) takeFaulted() []faultedArticle {
	out := w.faulted
	w.faulted = nil
	return out
}

// Accept buffers or writes one article's bytes.
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
	// Collision detection happens HERE, before the cache is consulted, so it
	// is independent of cache residency and of whether caching is configured
	// at all. See acceptedAt for why identity rather than occupancy.
	//
	// The displaced article is the incumbent, not the arrival: its bytes are
	// either already on disk or about to be overwritten, and nothing will
	// write them again. Recording the arrival as the new owner afterwards is
	// what lets a third article at the same offset be detected in turn.
	// A settled offset never reaches here — acceptArticle refuses the arrival
	// before calling Accept — so any incumbent found here has made no
	// durability claim, and displacing it contradicts nothing.
	owner, taken := w.acceptedAt[off]
	if taken && !owner.id.sameArticle(id) {
		w.failDisplaced(owner.id, off, id)
		// Take the incumbent's bytes away as well as its accounting. Failing an
		// article whose payload is still buffered leaves the next drain to
		// write it and the barrier to ack it durable, for an article
		// routeFaulted has already reported permanently failed. See discardAt:
		// wc.buffer evicts the entry itself on most paths, but not for a
		// zero-length arrival, which it refuses before touching the map.
		w.wc.discardAt(w.key, off)
	}
	// A re-accept by the offset's OWN owner keeps the entry it already has,
	// written latch included. Rewriting it unconditionally reset written to
	// false and unsettled an offset whose bytes were already durable, after
	// which the next article to claim it was no longer refused and displaced
	// an article the barrier had acked — the double disposition again.
	//
	// seenDone is keyed on ArtIdx now, so a genuine DUPLICATE delivery of an
	// already-accepted article is caught by handleSuccessArticle's dedup arm
	// and never reaches acceptArticle at all, whatever its Message-ID — that
	// used to be the only way an untracked article reached here, because
	// admitAccepted recorded nothing for an empty Message-ID and the dedup
	// missed it. What still reaches this branch is a write-fault RETRY:
	// rollbackPart deletes the seenDone entry so the redelivery is not a
	// duplicate, but acceptedAt's entry for this offset is never removed (see
	// acceptedAt), so the same identity finds itself already the owner. A
	// re-accept that writes through immediately hid the old defect by
	// re-latching in noteWritten; one that lands in the cache did not.
	if !taken || !owner.id.sameArticle(id) {
		w.acceptedAt[off] = offsetOwner{id: id}
	}

	art := bufferedArticle{offset: off, data: data, id: id, crc32: crc32}
	if w.wc.buffer(w.key, art) {
		telemetry.CacheHits.Add(1)
		if run := w.wc.flushContiguous(w.key); run != nil {
			return w.flushRun(run)
		}
		return nil
	}
	// Caching disabled, or the article is zero-length and the cache refused
	// it. Write it straight through.
	return w.writeOne(art)
}

// writeOne writes a single article and reports it Written on success.
func (w *FileWriter) writeOne(art bufferedArticle) error {
	telemetry.DiskWrites.Add(1)
	telemetry.DiskWriteBytes.Add(int64(len(art.data)))
	_, err := w.writeAt(art.data, art.offset)
	if art.data != nil {
		defer decoder.PutBuffer(art.data)
	}
	if err != nil {
		telemetry.PipelineErrors.Add(telemetry.ErrClassDiskWriteError, 1)
		w.fail(art.id)
		return storagefault.Classify("write", w.path, err)
	}
	w.noteWritten(art.id, art.offset, len(art.data), art.crc32)
	return nil
}

// flushRun writes a coalesced run and reports every article in it.
//
// On failure every article in the run loses its bytes, not just whichever one
// triggered the flush: buildContiguousRun coalesced them all into one buffer
// and pooled the originals before this write was attempted. Reporting only the
// triggering article would leave the rest believed Written with their bytes
// freed and no run able to fetch them again.
func (w *FileWriter) flushRun(run *flushRun) error {
	telemetry.CacheFlushes.Add(1)
	telemetry.CacheFlushBytes.Add(int64(len(run.data)))
	telemetry.DiskWrites.Add(1)
	telemetry.DiskWriteBytes.Add(int64(len(run.data)))
	if _, err := w.writeAt(run.data, run.offset); err != nil {
		telemetry.PipelineErrors.Add(telemetry.ErrClassDiskWriteError, 1)
		for _, p := range run.parts {
			w.fail(p.id)
		}
		return storagefault.Classify("write", w.path, err)
	}
	for _, p := range run.parts {
		w.noteWritten(p.id, p.offset, p.length, p.crc32)
	}
	return nil
}

// Drain flushes every buffered article for this file and returns the articles
// whose bytes reached WriteAt without error since the last SUCCESSFUL Sync —
// NOT since the last Drain.
//
// The distinction is load-bearing and this comment used to get it wrong. take()
// re-reports everything a Drain has already handed over but no Sync has yet
// confirmed, which is R12's at-least-once delivery. Reading "since the last
// call" as the contract makes the re-report look redundant, and removing it
// destroys bytes on a retried finalize: the retry drains nothing, so the
// bound FinalizeFile trims to sits below bytes genuinely on disk. See
// take() and the FileWriter.reported field doc, which are the authority.
//
// It must NOT return an article that is merely buffered. S2 makes acceptance
// and durability different things, and this return value is the only evidence
// the barrier has — returning a buffered article here is defect #355
// relocated: the barrier would fsync bytes that are not in the file and ack an
// article that is not on disk.
//
// On the first write failure it returns the articles that DID land plus the
// classified fault, so the barrier can see both what it may claim and why the
// drain stopped. It stops rather than continuing, because a storage fault is
// a condition of the device and the next write is overwhelmingly likely to hit
// it too.
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
	_, arts := w.wc.drainFile(w.key)
	for i, art := range arts {
		if err := w.writeOne(art); err != nil {
			// writeOne pooled the article it just handled. Everything after it
			// was never attempted and still holds a pooled buffer, so release
			// those here or the decoder's pool leaks one buffer per article
			// for the rest of the drain.
			for _, rest := range arts[i+1:] {
				if rest.data != nil {
					decoder.PutBuffer(rest.data)
				}
			}
			// Those articles are neither Written nor failed — they stay
			// Outstanding, which is what S3 requires of an article whose state
			// cannot be established. Their seen-set entries are cleared so a
			// re-delivery is not mistaken for a duplicate.
			for _, rest := range arts[i+1:] {
				w.fail(rest.id)
			}
			return w.take(), err
		}
	}
	return w.take(), nil
}

// take moves the newly written articles into the unconfirmed set and returns
// the whole of it — everything written since the last SUCCESSFUL Sync.
//
// The split between the two slices is what keeps an article written BETWEEN a
// Drain and its Sync from being discarded by that Sync: it is still in
// w.written, which Sync does not touch, so the next Drain reports it. Folding
// the two together would silently drop it — covered by the fsync, but never
// claimed, so never acked.
func (w *FileWriter) take() []durability.WrittenArticle {
	w.reported = append(w.reported, w.written...)
	w.written = nil
	return slices.Clone(w.reported)
}

// Sync fsyncs the handle. Until this returns nil, nothing a preceding Drain
// reported may be claimed (S1).
//
// It takes no context, for the reason Drain documents at length: the guard was
// unreachable, and reaching it would have turned a cancellation into a storage
// fault and stalled a healthy job.
func (w *FileWriter) Sync() error {
	if err := w.syncFile(); err != nil {
		return storagefault.Classify("sync", w.path, err)
	}
	// The report is deliberately NOT discarded here. The fsync makes the
	// bytes durable, but the runs are not yet committed and nothing is acked,
	// and either of those can still fail. Clearing on the fsync covered only
	// the first of the failures the retention exists for. Confirm is what
	// releases it.
	return nil
}

// Confirm releases the drain report, and is called only once the barrier has
// committed the runs and acked the articles.
//
// It is what bounds the retained set. Drain re-reports across any failure, so
// without a confirmation the set would grow to every article ever written to
// this file and every later checkpoint would redo all of it.
//
// Deliberately cannot fail. It records that work already succeeded, so there
// is no outcome for a caller to handle: a missed Confirm costs one redundant
// re-report, which R12 makes the barrier absorb, while a Confirm that could
// fail would need its own recovery path for a cycle that has already landed.
func (w *FileWriter) Confirm() {
	w.reported = nil
}

// Stat returns the file's size as it is now. It is S7's validity stamp — which
// a resume checks the file against, and which the barrier compares Σ Length to
// — so it must be read after the Sync it describes.
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

// Close releases the handle, and hands back any articles that were rolled back
// and never routed.
//
// # Why the set is a return value
//
// Close is the writer's last act: everything it holds is unreachable
// afterwards, w.faulted included. An article left in that set is neither Done,
// nor Failed, nor Outstanding — its Emitted bit is still set from dispatch and
// ForEachUnfinishedArticle skips a set Emitted bit — so it is stranded for the
// life of the process and only a restart's ClearAllEmitted recovers it.
//
// There are two call sites — `grep -n 'w\.Close()' internal/assembler/*.go |
// grep -v _test.go` returns the cancel arm and drainAndClose — and the set is
// empty at both whenever every producer of w.faulted has been drained before
// the worker returns to its select loop, which is the ordinary case.
//
// One path leaves it non-empty on purpose. The cancel arm's KeepFiles branch
// calls Drain, whose error path calls w.fail on everything it did not attempt,
// and then deliberately skips the releaseFaulted that would empty the set
// again — because routing those articles would re-dispatch work for a job
// that has left the queue. So a non-empty set there is expected rather than a
// defect, and that arm distinguishes the two cases by whether the drain
// failed.
//
// This return value does not fix a leak. It converts the emptiness from a
// property that has to be re-argued across the whole file into one the
// compiler restates at each call site — the same reason takeFaulted is a take
// and not a read. A caller that adds a new path into Close now has to say what
// happens to the articles, instead of silently dropping them.
//
// Taken rather than read, on takeFaulted's terms: each set must be routed
// exactly once, and reporting one twice would clear an Emitted bit a later
// dispatch had legitimately set.
//
// The error is unchanged and still reports STORAGE. A non-empty set is a
// defect in this package, not a condition of the volume, so it is deliberately
// not folded into the error: drainAndClose classifies that error into the
// barrier's fault handling, and a bug reported as a storage fault would stall
// a job over a healthy disk.
func (w *FileWriter) Close() ([]faultedArticle, error) {
	leaked := w.takeFaulted()
	if err := w.closeFile(); err != nil {
		return leaked, storagefault.Classify("close", w.path, err)
	}
	return leaked, nil
}

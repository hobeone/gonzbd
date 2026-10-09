package assembler

import (
	"cmp"
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
	// into faulted so the caller returns them to Outstanding.
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

	// accepted records the pairwise-disjoint byte intervals [off, end) this
	// writer has accepted bytes for, sorted by off. It is the collision and
	// range-overlap detector (#383, #759).
	//
	// Detection lives here rather than in writeCache.buffer because cache
	// membership only sees articles still unwritten, whereas an in-order
	// download flushes and evicts an article before a colliding or overlapping
	// segment arrives.
	//
	// A collision is decided by IDENTITY, not by occupancy: a range already
	// owned by THIS article is a re-accept, not a collision. That is what
	// makes the set safe without removing entries on a write-fault rollback.
	//
	// Any arrival whose range intersects an already-accepted range owned by
	// another article is refused in acceptArticle via offsetSettledBy, except
	// when the sole intersecting range is an unwritten buffered incumbent at
	// the exact same start offset and length (r.canBeDisplacedBy), which Accept
	// displaces in place via failDisplaced.
	//
	// Entries are never removed on rollback, and that is deliberate rather
	// than a leak. An article whose write faults is rolled back and
	// re-dispatched, and comes back with the same ArtIdx, so identity
	// comparison recognises it as the owner. Residency is the writer's, so
	// this is per-open-episode — a collision spanning a close-handles cycle or
	// a restart is invisible, exactly as seenDone's duplicate handling is.
	accepted []acceptedRange

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
	// when displaced is set.
	//
	// They are set by the same statement that sets displaced, which is the
	// whole point of failDisplaced no longer patching this record after the
	// fact: "displaced with no displacer" is not a state a caller can reach.
	offset      int64
	displacedBy articleID
}

// acceptedRange is one byte interval [off, end) this writer has accepted, the
// article that owns it, and whether its bytes have been reported Written.
//
// written is latched by noteWritten and is never cleared while the entry
// survives, which is the whole reason it lives here rather than being derived
// from w.written/w.reported. Those two slices are the barrier's pending
// evidence: Confirm empties them once the articles have been acked durable. An
// article that has been acked is the strongest possible claim on its range,
// and a claim derived from the pending slices would read it as no claim at all.
type acceptedRange struct {
	off     int64
	end     int64
	id      articleID
	written bool
}

// overlaps reports whether a write at [off, end) conflicts with r.
// Two ranges sharing a start offset always conflict, even when zero-length;
// otherwise non-empty half-open intervals [off, end) and [r.off, r.end)
// intersect when each starts before the other ends. A zero-length entry at a
// different start offset occupies no bytes and does not conflict with a
// covering interval.
func (r acceptedRange) overlaps(off, end int64) bool {
	if off == r.off {
		return true
	}
	if end == off || r.end == r.off {
		return false
	}
	if off < r.off {
		return r.off < end
	}
	return off < r.end
}

// canBeDisplacedBy reports whether an unwritten buffered incumbent r may be
// displaced in place by an arriving range [off, end) from another article.
// Only a same-start-offset arrival with the exact same length (end == r.end)
// can replace the single write-cache slot at r.off without leaving a partial
// slice or overlapping a neighbour.
func (r acceptedRange) canBeDisplacedBy(off, end int64) bool {
	return !r.written && r.off == off && end == r.end
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
	}
	w.writeAt = handle.WriteAt
	w.syncFile = handle.Sync
	w.closeFile = handle.Close
	return w
}

// firstCandidateIdx returns the earliest index in w.accepted that could
// overlap a range starting at off. Because w.accepted is sorted by off and
// pairwise disjoint (accepted[i].end <= accepted[i+1].off), at most one entry
// starting before off — the immediate predecessor idx-1 — can extend past off.
func (w *FileWriter) firstCandidateIdx(off int64) int {
	idx, _ := slices.BinarySearchFunc(w.accepted, off, func(r acceptedRange, target int64) int {
		return cmp.Compare(r.off, target)
	})
	if idx > 0 {
		return idx - 1
	}
	return 0
}

// ownerAt returns the accepted range starting at off, if any.
func (w *FileWriter) ownerAt(off int64) (acceptedRange, bool) {
	idx, found := slices.BinarySearchFunc(w.accepted, off, func(r acceptedRange, target int64) int {
		return cmp.Compare(r.off, target)
	})
	if !found {
		return acceptedRange{}, false
	}
	return w.accepted[idx], true
}

// noteWritten records an article whose bytes reached WriteAt without error.
//
// Every append to w.written goes through here, so there is exactly one place
// where "this article is Written" is asserted, and it is only ever reached
// from below a successful writeAt. That is the structural half of S2: the
// claim cannot be made from an accept path because no accept path can call
// this.
//
// It also latches the range's owner as written, which is what makes the range
// settled against a later article claiming the same offset. Latched HERE for
// the same reason the append is here: both assert "these bytes are the file's
// content at this offset", and applying one without the other is the
// derived-state split #375 was about.
func (w *FileWriter) noteWritten(id articleID, off int64, n int, crc32 uint32) {
	end := off + int64(n)
	for i := w.firstCandidateIdx(off); i < len(w.accepted) && w.accepted[i].off <= off; i++ {
		if w.accepted[i].overlaps(off, end) && w.accepted[i].id.sameArticle(id) {
			w.accepted[i].written = true
			break
		}
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

// unconfirmed returns the articles a Drain has reported that no Confirm has
// yet released. Used by tests to assert the report survives a successful Sync
// until Confirm and is discarded by a failed Sync (#760).
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
// earlier. Here it does not: accepted entries are never removed, so a
// write-faulted article keeps its range ownership after fail has taken its
// part and its seenDone entry away, and a later arrival at that range
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
// range is settled, so this is reached only from the cache-eviction case it
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
		id:          id,
		displaced:   true,
		offset:      off,
		displacedBy: by,
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
// set Emitted bit, so an article that is merely dropped here is stranded until
// something clears that bit: neither Done, nor Failed, nor Outstanding. A
// restart clears it by not persisting it — jobProgressJSON excludes emitted
// deliberately (internal/job/progress.go) — and a downloader reload clears it
// in-process, unless #417 withholds that job's clear.
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

// offsetSettledBy reports the article that owns an already-accepted range
// intersecting [off, off+length) when the ARRIVING article must be refused
// rather than allowed to displace it (#383, #759).
//
// Any overlap with an interval at a different start offset, an interval of a
// different non-zero length, or an interval whose owner has already been
// reported Written settles the range against the arrival so the arriving
// article costs only its own bytes and never writes a splice over an accepted
// neighbour. Only an unwritten buffered incumbent occupying the exact same
// range (see acceptedRange.canBeDisplacedBy) is left unsettled here for Accept
// to displace in place.
func (w *FileWriter) offsetSettledBy(off, length int64, arriving articleID) (articleID, bool) {
	end := off + length
	for i := w.firstCandidateIdx(off); i < len(w.accepted); i++ {
		r := w.accepted[i]
		if !r.overlaps(off, end) {
			if r.off > off && r.off >= end {
				break
			}
			continue
		}
		if r.id.sameArticle(arriving) || r.canBeDisplacedBy(off, end) {
			continue
		}
		return r.id, true
	}
	return articleID{}, false
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

// recordAccepted inserts or updates [off, end) in w.accepted while maintaining
// its start-sorted, pairwise-disjoint invariant. Any unwritten incumbent owned
// by a different article that overlaps [off, end) is displaced and its
// buffered bytes are discarded; a re-accept by the same article preserves its
// written latch and merged span.
func (w *FileWriter) recordAccepted(id articleID, off, end int64) {
	first := w.firstCandidateIdx(off)
	if first < len(w.accepted) {
		r := w.accepted[first]
		if off == end && r.off < off && off < r.end {
			return
		}
		if !r.overlaps(off, end) && r.off < off {
			first++
		}
	}
	last := first
	written := false
	for last < len(w.accepted) && (w.accepted[last].overlaps(off, end) || w.accepted[last].off < end) {
		r := w.accepted[last]
		if !r.overlaps(off, end) {
			last++
			continue
		}
		if !r.id.sameArticle(id) {
			w.failDisplaced(r.id, r.off, id)
			w.wc.discardAt(w.key, r.off)
		} else {
			written = written || r.written
			off = min(off, r.off)
			end = max(end, r.end)
		}
		last++
	}
	w.accepted = slices.Replace(w.accepted, first, last, acceptedRange{
		off:     off,
		end:     end,
		id:      id,
		written: written,
	})
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
	w.recordAccepted(id, off, off+int64(len(data)))

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
// whose bytes reached WriteAt without error since the last confirmed cycle
// (and not poisoned by a failed Sync) — NOT merely since the last Drain.
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
// articles written since the Drain) and rolls them back into faulted so the
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
// w.reported and w.written back into w.faulted so the caller returns them to
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

// rollbackSyncedArticle unlatches written on w.accepted for artIdx and rolls
// the article back into w.faulted if it is not already pending there.
func (w *FileWriter) rollbackSyncedArticle(artIdx int32) {
	id := articleID{artIdx: artIdx}
	for i := range w.accepted {
		if w.accepted[i].id.artIdx == artIdx {
			w.accepted[i].written = false
			id = w.accepted[i].id
		}
	}
	for _, f := range w.faulted {
		if !f.displaced && f.id.artIdx == artIdx {
			return
		}
	}
	w.fail(id)
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

// Close releases the handle, and hands back any articles that were rolled back
// and never routed.
//
// # Why the set is a return value
//
// Close is the writer's last act: everything it holds is unreachable
// afterwards, w.faulted included. An article left in that set is neither Done,
// nor Failed, nor Outstanding — its Emitted bit is still set from dispatch and
// ForEachUnfinishedArticle skips a set Emitted bit — so it is stranded for the
// life of the process unless something clears that bit. A restart clears it by
// not persisting it — jobProgressJSON excludes emitted deliberately
// (internal/job/progress.go) — and a downloader reload clears it in-process,
// unless #417 withholds that job's clear.
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

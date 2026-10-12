package assembler

import (
	"fmt"
	"os"

	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/telemetry"
)

// articleID identifies an article as it travels with its bytes to the write
// that lands them, so the write can say which article it carried.
//
// artIdx is identity: it is what fileWriter.seenDone / seenFailed key duplicate
// handling on and what fileWriter.owned names a range's owner by, so it must
// match the queue's numbering. msgID is set alongside it, but no code
// reads it back: the log lines that name a Message-ID take it from the
// WriteRequest. Neither reaches the queue from here: this package has no ack
// path in either direction.
type articleID struct {
	msgID  string
	artIdx int32
}

// sameArticle reports whether two identities name the same article.
//
// The index alone decides. msgID is deliberately NOT compared: identity is
// the manifest index, which is what fileWriter's seen-sets key on. Comparing
// the pair as well would
// give the assembler a second, finer notion of sameness than the one its
// accounting uses, and the two could disagree.
func (a articleID) sameArticle(b articleID) bool { return a.artIdx == b.artIdx }

// disposition is how an article leaves the writer's accounting: the one value
// resolve dispatches on, so that every exit is a named arm of one switch and
// an exit nobody named is a panic rather than a fallthrough.
//
// The zero value is deliberately not a disposition. An outcome built without
// one reaches resolve as zero and is refused there (R28), which is what makes
// "left in flight with no disposition" loud at the first test that walks the
// path, instead of a silent undercount discovered at TotalParts.
type disposition uint8

const (
	// dispositionWritten: the article's bytes reached WriteAt without error.
	// The admission stands: seenDone kept, the part kept.
	dispositionWritten disposition = iota + 1

	// dispositionRejected: this package refused the article's bytes because
	// of the article — an out-of-range offset, or a range another article
	// already wrote. It is resolved permanently failed (OnArticleRejected) and
	// will not arrive again, so it keeps the part its admission charged and
	// moves from seenDone to seenFailed. Keeping the part claims nothing
	// about its bytes: it owns no range, so the completion trim (finish,
	// bounded by ownedRanges.maxEnd) never reaches past it on its account.
	dispositionRejected

	// dispositionFetchFailed: the downloader could not fetch the article
	// (WriteRequest.FatalErr). It never enters the write path and is never
	// admitted, so this is the one disposition with no admission to resolve:
	// it charges a part unless the article already holds one, and records
	// seenFailed. A copy already in seenDone keeps it: the bytes that landed
	// are not retracted by a later failure to fetch them again.
	dispositionFetchFailed

	// dispositionRolledBack: storage failed the article's write. A storage
	// fault says nothing about the article (A1), so it is rolled back to
	// never-having-arrived: seenDone
	// cleared, the part given back if the admission charged one, seenFailed
	// untouched. The caller returns it to Outstanding.
	dispositionRolledBack
)

// String names the disposition for the panic resolve raises on one it does
// not know. Every other reader switches on the value.
func (d disposition) String() string {
	switch d {
	case dispositionWritten:
		return "written"
	case dispositionRejected:
		return "rejected"
	case dispositionFetchFailed:
		return "fetch-failed"
	case dispositionRolledBack:
		return "rolled-back"
	default:
		return fmt.Sprintf("disposition(%d)", uint8(d))
	}
}

// fileWriter owns one target file: its handle and its write path. It has no
// authority over anything externally visible.
//
// It cannot ack an article, record a CRC part or decide a file is complete.
// Its durability steps are finish (fsync, trim to its owned ranges, fsync) and
// sync, the fsync at close. A failed sync changes nothing here: the writer is
// closed right after, and its caller untrusts the file (OnFileUntrusted).
//
// Single-goroutine, no locking, like everything else the assembler worker
// owns (X1).
type fileWriter struct {
	handle *os.File
	path   string
	key    fileKey

	// seenDone and seenFailed keep duplicate handling idempotent (R12).
	//
	// Both are keyed on ArtIdx, the article's manifest index, rather than its
	// Message-ID. ArtIdx has no value that doubles as "absent" — an NZB may
	// omit the Message-ID, but never the index — so every article has a key
	// these maps can hold, and there is no empty-key class to guard against.
	//
	// Both are membership-only: admit's duplicate answer releases the second
	// copy's buffer at the caller, because either way its bytes are redundant
	// and re-writing them would be a second WriteAt over the same range.
	//
	// seenDone is also the record of admission: an article is in it from
	// admit until resolve moves it out (Rejected, RolledBack) or leaves it
	// (Written, FetchFailed). resolve refuses a Written, Rejected or
	// RolledBack for an article not in it, which is how resolving an article
	// twice, or one nobody admitted, fails loudly.
	//
	// Read and written only here: `git grep -n -E 'w\.seen(Done|Failed)' --
	// 'internal/assembler/*.go' ':!*_test.go'` returns 10 lines, all in this
	// file, inside holdsPart, admit and resolve.
	// TestFileWriterAccounting_HasOneOwner enumerates the functions.
	seenDone   map[int32]struct{}
	seenFailed map[int32]struct{}

	// owned records which article owns each byte range of the file: first
	// writer wins (#383, #759). It is filled two ways. At open,
	// Assembler.seedOwned seeds it with the ranges a restart verified
	// (FileInfo.Owned), owned by seededOwner. After that, writeOne claims a
	// range only once its write returned nil, so an article whose write
	// faulted owns nothing and its retry, or a rival at the same range, is
	// accepted. acceptArticle refuses an arrival that intersects another
	// article's range before accept is called.
	//
	// Entries are never removed. The question is "who wrote these bytes",
	// not "who currently holds a part", so owned is intentionally not equal to
	// seenDone. Residency is the writer's, so this is per-open-episode.
	owned ownedRanges

	// partsWritten counts how many of the file's parts have been accounted
	// for, whether by a successful accept or by a permanent failure.
	//
	// It is DERIVED from seenDone and seenFailed: it equals the size of their
	// union, and TestFileWriter_PartsIsTheSizeOfTheUnion walks every
	// transition checking that. Kept one struct away from its source and
	// maintained by four external call sites, every defect it produced was
	// the same one: the count and the record moved at different moments, and
	// something in between observed the disagreement.
	//
	// Recomputing it on demand would be O(n) per article, so it stays a cached
	// aggregate. What changes is that only admit and resolve may move it,
	// and each applies a complete transition, so a partial application is not
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
	// It exists because the close arm of syncAndClose is documented as
	// load-bearing — on network-backed mounts the close is frequently where a
	// deferred write error first surfaces — and had no coverage at all. The
	// only way to fail a real close is to close the handle first, which makes
	// sync fail one line earlier and never reaches the arm under test.
	closeFile func() error
}

// newFileWriter wraps an already-open handle.
func newFileWriter(handle *os.File, path string, key fileKey) *fileWriter {
	w := &fileWriter{
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

// parts reports how many of the file's parts have been accounted for. The
// caller compares it to FileInfo.TotalParts to decide the file is complete.
func (w *fileWriter) parts() int { return w.partsWritten }

// holdsPart reports whether the article is counted toward the file's part
// total — accepted, resolved permanently failed, or both.
//
// It is the question handleLateDuplicate asks before reporting a redelivery
// for a finished file: an article this writer accounted for is resolved and
// is dropped, and one it never saw is owed a disposition.
func (w *fileWriter) holdsPart(artIdx int32) bool {
	if _, done := w.seenDone[artIdx]; done {
		return true
	}
	_, failed := w.seenFailed[artIdx]
	return failed
}

// admit takes an article on as a part of this file, before its bytes are
// handed to accept, and reports whether it did. False means the article is a
// duplicate of one already accepted: its first copy is written or in flight,
// so this copy's bytes are redundant and the caller releases them.
//
// The seenDone record and the count are applied together, and that pairing is
// the point of the method. resolve decides whether a RolledBack gives a part
// back by asking whether the article is in seenDone, so seenDone is the de
// facto record of "this article holds a part". While the two lived at
// separate call sites the count could be applied after a write that had
// already rolled it back, and every transient accept-time write fault
// undercounted the file by one, permanently, putting
// partsWritten >= TotalParts out of reach.
//
// Whether the admission charges a part is the writer's to decide, not the
// caller's: an article already counted as permanently failed is admitted so
// its bytes are written — they are still the file's content — but the part
// was charged when it was failed, and charging it twice would carry the file
// past TotalParts.
func (w *fileWriter) admit(artIdx int32) bool {
	if _, dup := w.seenDone[artIdx]; dup {
		return false
	}
	if !w.holdsPart(artIdx) {
		w.partsWritten++
	}
	w.seenDone[artIdx] = struct{}{}
	return true
}

// resolve is the one exit from the writer's accounting: it applies the
// transition d names for one article, and nothing else moves seenDone,
// seenFailed or partsWritten (see the field comments). Two callers reach
// it — `git grep -n 'w\.resolve(' -- 'internal/assembler/*.go' ':!*_test.go'`
// returns 2 lines: handleSuccessArticle, for the outcome of an admitted
// article's write, and handleFatalArticle, for a FetchFailed.
//
// Written, Rejected and RolledBack resolve an admission, so they require the
// article to be in seenDone and panic otherwise: an article nobody admitted
// has no part to settle, and one already resolved out (Rejected or
// RolledBack cleared its entry) would be getting a second disposition. That
// panic is an invariant violation failing loudly (R28), never a condition
// production reaches — every production caller admits first, and admit
// refuses an article already in seenDone.
//
// FetchFailed has no admission to resolve, since a fatal article never enters
// the write path, and a second copy of one is ordinary duplicate delivery
// (R12) rather than a programming error, so it is total: a duplicate changes
// nothing.
//
// The accounting each arm applies is the whole of the arm. The caller does
// not choose it and cannot apply half of it.
func (w *fileWriter) resolve(artIdx int32, d disposition) {
	_, wasDone := w.seenDone[artIdx]
	_, wasFailed := w.seenFailed[artIdx]
	if d != dispositionFetchFailed && !wasDone {
		panic(fmt.Sprintf("assembler: article %d resolved %s without being admitted, or resolved twice", artIdx, d))
	}
	switch d {
	case dispositionWritten:
		// The admission stands: seenDone kept, the part kept.
	case dispositionRejected:
		// Keeps its part: the article is resolved permanently failed through
		// OnArticleRejected and will not arrive again, so a file that stopped
		// counting it could never reach TotalParts. The part was charged by
		// admit, or earlier by a FetchFailed if this was a retry of one.
		delete(w.seenDone, artIdx)
		w.seenFailed[artIdx] = struct{}{}
	case dispositionFetchFailed:
		// A part is charged only for an article holding none: a copy already
		// written, or already failed, was counted when it arrived.
		if !wasDone && !wasFailed {
			w.partsWritten++
		}
		w.seenFailed[artIdx] = struct{}{}
	case dispositionRolledBack:
		// An article already counted as permanently FAILED keeps its count:
		// FetchFailed charged it, and a retry whose write fails must not
		// decrement what a different disposition added. Only an article
		// counted by its admission loses its count here. seenFailed is left
		// alone: a storage fault says nothing about the article (A1), and
		// recording it failed made a redelivery take the retry-of-failed
		// path, which writes the bytes but does not count them, so the file's
		// part total was permanently one short for every rolled-back article.
		delete(w.seenDone, artIdx)
		if !wasFailed {
			w.partsWritten--
		}
	default:
		panic(fmt.Sprintf("assembler: article %d resolved with %s, which is not a disposition", artIdx, d))
	}
}

// accept writes one article's bytes through writeOne.
//
// It takes ownership of data and returns it to the decoder pool on every path,
// including failure, so a caller never has to reason about who frees it.
//
// A returned error is always a *storagefault.Fault. It reports that STORAGE
// failed, never that the article did (A1, R19).
//
// It applies no accounting. The caller resolves the article RolledBack on an
// error and Written on nil; dropping the error neither stalls the job nor
// returns the article to Outstanding (its Emitted bit stays set), and the
// file goes on to complete over bytes that never landed.
func (w *fileWriter) accept(id articleID, off int64, data []byte) error {
	return w.writeOne(id, off, data)
}

// writeOne writes a single article and claims its range on success.
func (w *fileWriter) writeOne(id articleID, off int64, data []byte) error {
	telemetry.DiskWrites.Add(1)
	telemetry.DiskWriteBytes.Add(int64(len(data)))
	_, err := w.writeAt(data, off)
	if data != nil {
		defer decoder.PutBuffer(data)
	}
	if err != nil {
		telemetry.PipelineErrors.Add(telemetry.ErrClassDiskWriteError, 1)
		return storagefault.Classify("write", w.path, err)
	}
	w.owned.claim(Range{Off: off, Len: int64(len(data))}, id)
	return nil
}

// sync fsyncs the handle and reports a failure as a classified fault.
//
// It keeps no record of what the fsync covered, because a failure is answered
// for the whole file: Linux reports a writeback error to a file descriptor
// once (errseq) and marks the failed pages clean, so a later fsync on the same
// handle can return nil over bytes that never reached disk (#760), and the
// only safe reading is that none of the file's writes landed. syncAndClose's
// callers untrust the file (OnFileUntrusted), which returns every written
// article of it to Outstanding.
//
// It takes no context: its only caller is the assembler worker, which has none
// to pass, and a cancellation is not a storage fault.
func (w *fileWriter) sync() error {
	if err := w.syncFile(); err != nil {
		return storagefault.Classify("sync", w.path, err)
	}
	return nil
}

// close releases the handle.
//
// The error reports STORAGE: on network-backed mounts the close is where a
// deferred write error first surfaces, and syncAndClose reports it.
func (w *fileWriter) close() error {
	if err := w.closeFile(); err != nil {
		return storagefault.Classify("close", w.path, err)
	}
	return nil
}

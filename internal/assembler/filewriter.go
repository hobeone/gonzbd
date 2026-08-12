package assembler

import (
	"context"
	"os"

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
	// error since the last Drain. It is the ONLY evidence the barrier has, so
	// nothing may be appended here that has not come back from a successful
	// writeAt (S2).
	//
	// Drain hands the slice over and resets it. An article dropped because a
	// barrier failed after draining is re-fetched, not lost: it stays
	// Outstanding, which is the safe direction under S3. Retaining it to save
	// the re-fetch would trade a bounded cost for an unbounded slice.
	written []durability.WrittenArticle

	// seenDone and seenFailed keep duplicate handling idempotent (R12).
	// They moved here from openFile because the writer is now the only
	// component that can tell whether a duplicate's first copy has left the
	// cache — the check #358 added as wc.buffered. seenDone's value is the
	// offset the article was accepted at, which the duplicate branch needs in
	// order to ask that question: a duplicate is deduped by Message-ID and may
	// carry a different offset, so its own is the wrong one to look up.
	seenDone   map[string]int64
	seenFailed map[string]struct{}

	// writeAt is handle.WriteAt in production. Tests override it before first
	// use to inject storage faults, mirroring how diskProbe.statfs is
	// overridden. Never reassigned after the writer is in service, so the
	// single-goroutine ownership rule covers it.
	writeAt func(p []byte, off int64) (int, error)
}

// newFileWriter wraps an already-open handle.
func newFileWriter(handle *os.File, path string, key fileKey, wc *writeCache) *FileWriter {
	w := &FileWriter{
		handle:     handle,
		path:       path,
		key:        key,
		wc:         wc,
		seenDone:   make(map[string]int64),
		seenFailed: make(map[string]struct{}),
	}
	w.writeAt = handle.WriteAt
	return w
}

// noteWritten records an article whose bytes reached WriteAt without error.
//
// Every append to w.written goes through here, so there is exactly one place
// where "this article is Written" is asserted, and it is only ever reached
// from below a successful writeAt. That is the structural half of S2: the
// claim cannot be made from an accept path because no accept path can call
// this.
func (w *FileWriter) noteWritten(id articleID, off int64, n int) {
	w.written = append(w.written, durability.WrittenArticle{
		FileIdx: int32(w.key.fileIdx), //nolint:gosec // G115: file counts are far below int32
		ArtIdx:  id.artIdx,
		Offset:  off,
		Length:  int32(n), //nolint:gosec // G115: an article's decoded length is far below int32
	})
}

// writtenSoFar returns the articles reported Written since the last Drain,
// without draining. Used by tests to assert that a merely-buffered article has
// made no claim.
func (w *FileWriter) writtenSoFar() []durability.WrittenArticle { return w.written }

// fail marks an article as not-on-disk so a later run fetches it again.
//
// It moves the article out of seenDone as well, because the two answer
// different questions this file's dedup logic would otherwise conflate:
// seenDone means "accepted, counted toward the file's parts", and after a
// failed write the article is still counted but is no longer on its way to
// disk. Leaving it there would let the duplicate branch treat it as already
// handled and never retry it.
//
// There is deliberately no ack here. A failed WRITE is a storage condition,
// and A1 forbids resolving it against the article: the article simply does not
// appear in Drain's return, so it stays Outstanding and the fault travels
// separately as a *storagefault.Fault.
func (w *FileWriter) fail(id articleID) {
	if id.msgID != "" {
		delete(w.seenDone, id.msgID)
		w.seenFailed[id.msgID] = struct{}{}
	}
}

// Accept buffers or writes one article's bytes.
//
// It takes ownership of data and returns it to the decoder pool on every path,
// including failure, so a caller never has to reason about who frees it.
//
// A returned error is always a *storagefault.Fault. It reports that STORAGE
// failed, never that the article did: the article is simply absent from the
// next Drain, which leaves it Outstanding (A1, R19).
func (w *FileWriter) Accept(id articleID, off int64, data []byte) error {
	art := bufferedArticle{offset: off, data: data, id: id}
	if cached, displaced := w.wc.buffer(w.key, art); cached {
		telemetry.CacheHits.Add(1)
		// An article displaced from this offset loses its bytes; nothing else
		// will write them now. It has made no claim — it was never reported
		// Written — so failing it here only corrects the seen-sets.
		for _, d := range displaced {
			w.fail(d)
		}
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
	w.noteWritten(art.id, art.offset, len(art.data))
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
		w.noteWritten(p.id, p.offset, p.length)
	}
	return nil
}

// Drain flushes every buffered article for this file and returns the articles
// whose bytes reached WriteAt without error since the last call.
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
func (w *FileWriter) Drain(ctx context.Context) ([]durability.WrittenArticle, error) {
	if err := ctx.Err(); err != nil {
		return w.take(), storagefault.Classify("write", w.path, err)
	}
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

// take hands over the accumulated Written articles and resets the buffer.
func (w *FileWriter) take() []durability.WrittenArticle {
	out := w.written
	w.written = nil
	return out
}

// Sync fsyncs the handle. Until this returns nil, nothing a preceding Drain
// reported may be claimed (S1).
func (w *FileWriter) Sync(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return storagefault.Classify("sync", w.path, err)
	}
	if err := w.handle.Sync(); err != nil {
		return storagefault.Classify("sync", w.path, err)
	}
	return nil
}

// Stat returns the file's size and modification time as they are now. The pair
// is the S7 validity stamp a later resume checks the file against, so it must
// be read after the Sync it describes.
func (w *FileWriter) Stat() (size int64, modTimeNs int64, err error) {
	fi, err := w.handle.Stat()
	if err != nil {
		return 0, 0, storagefault.Classify("stat", w.path, err)
	}
	return fi.Size(), fi.ModTime().UnixNano(), nil
}

// Truncate trims the file to n bytes.
//
// Only ever called with a bound derived from the durable facts, never from
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
func (w *FileWriter) Close() error {
	if err := w.handle.Close(); err != nil {
		return storagefault.Classify("close", w.path, err)
	}
	return nil
}

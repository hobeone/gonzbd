package assembler

import (
	"cmp"
	"slices"

	"github.com/hobeone/gonzbd/internal/decoder"
)

// writeCache is a memory-bounded article buffer that coalesces contiguous
// articles into larger writes. It runs exclusively on the assembler's single
// worker goroutine, so it requires no locking.
//
// Every key the cache holds is believed to be a key of the assembler's
// open-file map too: each removal from that map is paired with a forget in the
// same call, with no yield to the request channel in between. A DRAIN is not
// enough on its own — drainFile deliberately retains the per-file entry to
// preserve its write cursor — so opClose, cancel and close-handles each call
// forget as well.
//
// Worker exit (drainAndCloseAll) is the one path that does not, and does not
// need to: the whole cache goes out of scope with the worker, and there is no
// subsequent request that could observe the difference.
//
// Nothing enforces the pairing, which is why the assembler's two "unknown
// file" branches still exist and still log.
//
// Those branches matter more than they read. An article's Done ack now waits
// on its write, so a buffered article the cache loses track of is one the
// queue never hears about in either direction, and no later run re-dispatches
// it. Both branches settle what they discard as failed for that reason.
//
// Design:
//   - Articles are buffered by (fileKey, offset).
//   - When a contiguous run from offset 0 (or the file's current write cursor)
//     reaches a threshold size, the run is flushed as a single WriteAt.
//   - When total memory exceeds 90% of the limit, the file with the most
//     buffered data is force-flushed regardless of contiguity.
//   - When a file completes, all its buffered articles are flushed.
type writeCache struct {
	limit int64 // max bytes to buffer; 0 disables caching
	used  int64 // current buffered bytes

	// perFile tracks buffered articles grouped by file.
	perFile map[fileKey]*fileBuf

	scratchBuf []byte // reusable buffer for coalescing contiguous runs
}

// fileBuf holds buffered articles for a single file.
type fileBuf struct {
	// articles maps byte offset → the buffered article at that offset.
	articles map[int64]bufferedArticle
	// writeCursor tracks the next expected contiguous offset for this file.
	//
	// It always starts at 0, including on a resumed download, and there is no
	// longer any way to seed it. That is deliberate: it is a coalescing hint,
	// not an authority (#311, #353). A resumed file whose early articles are
	// not re-delivered simply never forms a run from 0 and its articles are
	// written individually, which costs syscalls and never correctness. The
	// completion truncate, which used to depend on a resume seed, now derives
	// its bound from the durable facts instead.
	//
	// It advances as contiguous runs are flushed, and also on a drain —
	// drainFile moves it past everything it hands back, which can jump an
	// offset that was never buffered. See drainFile.
	writeCursor int64
	// totalBytes is the sum of len(data) for all buffered articles.
	totalBytes int64
}

// articleID identifies an article as it travels with its bytes through the
// cache, so the write that finally moves those bytes can say which articles it
// carried. The cache is the only place that knows an article's bytes are still
// in memory, and a claim made before then is one no later run can check (#355).
//
// Both fields are carried, but they no longer answer the same question equally.
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

// bufferedArticle is a flushable article with its offset, data, and identity.
type bufferedArticle struct {
	offset int64
	data   []byte
	id     articleID
}

// flushRun is a contiguous run of articles that can be written as a single WriteAt.
type flushRun struct {
	offset int64  // starting byte offset
	data   []byte // coalesced data
	// parts names every article coalesced into data, with the byte range each
	// one contributed, so the write can report them all rather than only the
	// one whose arrival triggered the flush.
	//
	// The ranges are carried rather than recomputed because the coalesced
	// buffer is flat: once the articles are merged and their originals pooled,
	// nothing else can say which bytes belonged to which article, and a
	// durability.WrittenArticle needs exactly that to be a checkable claim.
	//
	// Allocated per run, unlike data, which is a view into the cache's reused
	// scratch buffer and is valid only until the next run is built.
	parts []runPart
}

// runPart is one article's contribution to a coalesced run.
type runPart struct {
	id     articleID
	offset int64
	length int
}

// newWriteCache creates a write cache with the given memory limit.
// A limit of 0 disables caching (all articles pass through immediately).
func newWriteCache(limit int64) *writeCache {
	return &writeCache{
		limit:   limit,
		perFile: make(map[fileKey]*fileBuf),
	}
}

// enabled reports whether write coalescing is active.
func (wc *writeCache) enabled() bool {
	return wc.limit > 0
}

// buffer adds an article to the cache. cached reports whether the article was
// taken; false means caching is disabled and the caller should write
// immediately.
//
// displaced carries the identity of an article evicted from the same offset,
// and is nil in every ordinary case. The caller must settle it rather than
// dropping it: once an ack waits on a write, an article silently removed from
// the cache is an article that is never acked at all and never re-dispatched,
// which is the defect this path exists to close.
//
// This is no longer where a collision is DETECTED, and reading it as such is
// what #383 was. Membership here answers "are these bytes still unwritten",
// which is a question about caching: buildContiguousRun deletes each article
// it flushes, so in an in-order download the first article had already left
// this map before its duplicate arrived, and three paths — flushed, caching
// disabled, zero-length — reported no collision at all. FileWriter.acceptedAt
// is the detector now, consulted in Accept before this function, and it sees
// all three. What remains here is a backstop for the eviction itself, which
// only this function can observe.
//
// Folding the two identities together and letting the winner's write ack both
// would be wrong. This branch exists for a case upstream dedup should already
// have caught, so nothing constrains the two articles to the same length, and a
// shorter winner would ack the displaced article over bytes its write never
// covered.
func (wc *writeCache) buffer(key fileKey, art bufferedArticle) (cached bool, displaced []articleID) {
	if !wc.enabled() {
		return false, nil
	}
	// A zero-length article is refused so the caller writes it inline. It
	// occupies no space, so it can neither be coalesced nor advance any
	// cursor, and buffering it would wedge buildContiguousRun: that scan
	// advances by the length of the article at the cursor, so a zero-length
	// entry there never moves and the loop never terminates. The inline path
	// costs a no-op WriteAt and settles the article.
	if len(art.data) == 0 {
		return false, nil
	}
	fb, ok := wc.perFile[key]
	if !ok {
		fb = &fileBuf{articles: make(map[int64]bufferedArticle)}
		wc.perFile[key] = fb
	}
	// If this offset already exists, replace it and adjust accounting.
	//
	// Reaching this with a DIFFERENT article means Accept's acceptedAt check
	// has already failed the incumbent, and Accept skips the one it failed —
	// so a second settlement here would charge the same article's bytes to the
	// job's par2 budget twice. Reaching it with the same article is a re-accept
	// after a rollback, which is not a collision at all.
	if existing, dup := fb.articles[art.offset]; dup {
		wc.used -= int64(len(existing.data))
		fb.totalBytes -= int64(len(existing.data))
		if existing.data != nil {
			decoder.PutBuffer(existing.data)
		}
		// Reported ONLY when a different article lost the slot. The buffer is
		// superseded either way, but "displaced" is a claim about two articles
		// and the caller settles whoever it names.
		//
		// Naming the same article made it displace ITSELF: failDisplaced
		// recorded it failed, appended a faulted record, raised a warning
		// naming one article twice, and had routeFaulted resolve it
		// permanently failed — while the replacement entry written just below
		// stayed queued to be written and acked. Two terminal dispositions for
		// one article.
		//
		// Not reachable from Accept as it stands, and kept as a backstop rather
		// than as a detector. Accept calls wc.discardAt the moment its
		// acceptedAt check finds a different owner, and discardAt deletes the
		// entry — so by the time buffer runs, the collision case has no
		// incumbent left here to find. The same-article case is excluded by the
		// test below instead.
		//
		// It stays because this is the only place that knows what buffer
		// dropped, so a future path that reaches buffer without going through
		// Accept's check still reports rather than silently overwrites. That is
		// a weaker justification than "buffer owns the eviction", which is what
		// this said while discardAt already owned it.
		if !existing.id.sameArticle(art.id) {
			displaced = []articleID{existing.id}
		}
	}
	fb.articles[art.offset] = art
	size := int64(len(art.data))
	fb.totalBytes += size
	wc.used += size
	return true, displaced
}

// discardAt drops whatever is buffered at one offset, pooling its bytes.
//
// It exists because "displaced" has to MEAN something. Accept fails the
// incumbent at an offset a later article claims, on the strength of its own
// comment that the incumbent "loses its bytes; nothing else will write them
// now" — but that was only ever true as a side effect of buffer replacing the
// entry, and buffer refuses a zero-length article before it touches
// fb.articles. A zero-length arrival therefore displaced the incumbent and
// left its bytes in the cache, where the next drain wrote them and handed them
// to the barrier to ack durable — for an article routeFaulted had already
// reported permanently failed.
//
// Calling this from the displacement path makes the claim true on every path
// rather than on all but one, and it no longer matters whether the arrival
// happens to be one the cache will accept.
func (wc *writeCache) discardAt(key fileKey, off int64) {
	fb, ok := wc.perFile[key]
	if !ok {
		return
	}
	existing, dup := fb.articles[off]
	if !dup {
		return
	}
	delete(fb.articles, off)
	wc.used -= int64(len(existing.data))
	fb.totalBytes -= int64(len(existing.data))
	if existing.data != nil {
		decoder.PutBuffer(existing.data)
	}
}

// buffered reports whether an article is still sitting unwritten at this
// offset. It distinguishes an article the cache has yet to write from one it
// has already written and acked, which the two look identical from outside.
//
// Used by tests, like FileWriter.writtenSoFar and FileWriter.unconfirmed, and
// unlike those it once had a production caller in prospect: FileWriter's doc
// described handleSuccessArticle's duplicate arm consulting it. That arm never
// did, because the answer does not change what it does. Kept as the package's
// way to say "buffered, not yet written" — the distinction several tests need
// and the two-level map lookup obscures — rather than removed with the claim.
func (wc *writeCache) buffered(key fileKey, offset int64) bool {
	fb, ok := wc.perFile[key]
	if !ok {
		return false
	}
	_, ok = fb.articles[offset]
	return ok
}

// contiguousRunSize is the minimum coalesced write size before we flush
// a contiguous run. Writes below this threshold stay buffered to accumulate
// more contiguous data. 512KB is a good balance: large enough to amortize
// syscall overhead, small enough to flush frequently.
const contiguousRunSize = 512 * 1024

// flushContiguous checks if there's a contiguous run from the file's write
// cursor that exceeds the threshold. If so, it returns the coalesced data
// and advances the cursor. Returns nil if no run is ready.
func (wc *writeCache) flushContiguous(key fileKey) *flushRun {
	fb, ok := wc.perFile[key]
	if !ok {
		return nil
	}
	return wc.buildContiguousRun(fb, contiguousRunSize)
}

// buildContiguousRun scans from fb.writeCursor for a contiguous run of
// articles. If the run's total size >= minSize, it coalesces the data,
// removes the articles from the buffer, and returns the run. Returns nil
// if the run is too small.
func (wc *writeCache) buildContiguousRun(fb *fileBuf, minSize int64) *flushRun {
	if len(fb.articles) == 0 {
		return nil
	}

	// Collect offsets starting from writeCursor.
	cursor := fb.writeCursor
	var runArticles []bufferedArticle
	var runSize int64

	for {
		art, ok := fb.articles[cursor]
		if !ok {
			break
		}
		// buffer refuses zero-length articles precisely so this scan always
		// advances. Stopping rather than trusting that is cheap, and the
		// alternative is not a wrong answer but a hung worker goroutine —
		// which owns every file handle, so it takes all assembly with it.
		if len(art.data) == 0 {
			break
		}
		runArticles = append(runArticles, art)
		runSize += int64(len(art.data))
		cursor += int64(len(art.data))
	}

	if runSize < minSize {
		return nil
	}

	// Coalesce into wc.scratchBuf to eliminate heap allocations during write coalescing.
	wc.scratchBuf = wc.scratchBuf[:0]
	startOffset := fb.writeCursor
	parts := make([]runPart, 0, len(runArticles))
	for _, art := range runArticles {
		wc.scratchBuf = append(wc.scratchBuf, art.data...)
		// Copied out of art rather than read back from the map: the entry is
		// gone by the time the run reaches disk, and the identity and extent
		// are what the write needs in order to report the article Written.
		parts = append(parts, runPart{id: art.id, offset: art.offset, length: len(art.data)})
		delete(fb.articles, art.offset)
		fb.totalBytes -= int64(len(art.data))
		wc.used -= int64(len(art.data))
		if art.data != nil {
			decoder.PutBuffer(art.data)
		}
	}
	fb.writeCursor = cursor

	return &flushRun{offset: startOffset, data: wc.scratchBuf, parts: parts}
}

// pressure reports whether memory usage exceeds 90% of the limit.
func (wc *writeCache) pressure() bool {
	if wc.limit <= 0 {
		return false
	}
	return wc.used*10 > wc.limit*9
}

// forceFlushLargest returns all buffered articles for the file with the
// most buffered data (regardless of contiguity). Used to relieve memory
// pressure. Returns nil if the cache is empty.
func (wc *writeCache) forceFlushLargest() (key fileKey, articles []bufferedArticle) {
	var largest fileKey
	var largestBytes int64

	for k, fb := range wc.perFile {
		if fb.totalBytes > largestBytes {
			largest = k
			largestBytes = fb.totalBytes
		}
	}
	if largestBytes == 0 {
		return fileKey{}, nil
	}

	return wc.drainFile(largest)
}

// drainFile removes and returns all buffered articles for a file, sorted by
// offset, and advances the file's write cursor past everything it returned.
// Used for force-flush and file completion.
//
// The cursor advance is what keeps coalescing alive across a memory-pressure
// flush. Every returned article is about to be written, so the frontier really
// has moved. Before, this deleted the whole entry, and the next buffered
// article recreated it with writeCursor back at zero — an offset whose article
// had just been drained and would never be re-buffered. buildContiguousRun
// then broke there on every later call and coalescing was dead for the rest of
// the file, with no failed article involved. See #311.
//
// The cursor is set past a hole rather than up to it, deliberately: a drain
// writes what is buffered above a gap as well as below, so stopping at the gap
// would re-strand the scan at the same offset. An article that later arrives
// below the advanced cursor is not lost — it is buffered as usual and written
// by the next drain, exactly as it would be today. It simply does not
// participate in a coalesced run.
//
// The entry is retained rather than deleted so the advanced cursor survives.
// Callers that are finished with the file call forget to drop it.
func (wc *writeCache) drainFile(key fileKey) (fileKey, []bufferedArticle) {
	fb, ok := wc.perFile[key]
	if !ok {
		return key, nil
	}

	articles := make([]bufferedArticle, 0, len(fb.articles))
	for offset, art := range fb.articles {
		articles = append(articles, art)
		wc.used -= int64(len(art.data))
		if end := offset + int64(len(art.data)); end > fb.writeCursor {
			fb.writeCursor = end
		}
	}
	// Sort by offset for sequential write ordering.
	slices.SortFunc(articles, func(a, b bufferedArticle) int {
		return cmp.Compare(a.offset, b.offset)
	})

	clear(fb.articles)
	fb.totalBytes = 0
	return key, articles
}

// drainAll removes and returns all buffered articles across all files, and
// drops every per-file entry. Used on assembler shutdown, where no file will
// be written again, so there is no cursor worth preserving.
func (wc *writeCache) drainAll() map[fileKey][]bufferedArticle {
	result := make(map[fileKey][]bufferedArticle, len(wc.perFile))
	for key := range wc.perFile {
		_, arts := wc.drainFile(key)
		if len(arts) > 0 {
			result[key] = arts
		}
		// drainFile retains the entry to preserve its cursor; nothing here
		// will use it again, so drop it. Safe after a drain: the articles
		// have been handed to the caller and forget has none left to pool.
		wc.forget(key)
	}
	return result
}

// forget removes tracking for a file without returning data, pooling anything
// still buffered. Called when nothing will be written for the file again:
// after a cancel, or by the completion and shutdown paths to drop the entry
// drainFile deliberately retained.
//
// Calling this after a drain is safe only because drainFile clears the
// articles map. The articles it returned have already been pooled by the
// caller that wrote them, so an uncleared map would double-return them.
func (wc *writeCache) forget(key fileKey) {
	if fb, ok := wc.perFile[key]; ok {
		for _, art := range fb.articles {
			if art.data != nil {
				decoder.PutBuffer(art.data)
			}
		}
		wc.used -= fb.totalBytes
		delete(wc.perFile, key)
	}
}

// bytesFor returns the total buffered bytes for a specific file.
func (wc *writeCache) bytesFor(key fileKey) int64 {
	if fb, ok := wc.perFile[key]; ok {
		return fb.totalBytes
	}
	return 0
}

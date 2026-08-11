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
// open-file map too: each removal from that map is paired with a forget or a
// drain in the same call, with no yield to the request channel in between.
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
	// Initially 0 (or the resume point seeded by initCursor). It advances as
	// contiguous runs are flushed, and also on a drain — drainFile moves it
	// past everything it hands back, which can jump an offset that was never
	// buffered. See drainFile.
	writeCursor int64
	// totalBytes is the sum of len(data) for all buffered articles.
	totalBytes int64
}

// articleID names an article to the queue. It travels with the article's bytes
// through the cache so that the write which makes those bytes durable can say
// which articles it settled — the cache is the only place that knows an
// article's bytes are still in memory, and an ack sent before then is a claim
// no later run can check (#355).
//
// Both fields are carried because the assembler supports two queue callback
// shapes: the index-based one, and the Message-ID one it falls back to when
// the index form is not wired. recordPendingDone picks between them.
type articleID struct {
	msgID  string
	artIdx int32
}

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
	// ids names every article coalesced into data, so the write can settle
	// them all rather than only the one whose arrival triggered the flush.
	//
	// Allocated per run, unlike data, which is a view into the cache's reused
	// scratch buffer and is valid only until the next run is built.
	ids []articleID
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

// initCursor pre-creates the file's buffer entry with the given starting
// write cursor if one doesn't already exist. Called once when a file is first
// registered so a resumed download (whose [0,cursor) range was already written
// and won't be re-delivered) doesn't wait forever for an offset-0 article that
// never arrives. No-op when caching is disabled or the entry already exists (a
// fresh download's first buffer() call legitimately starts the cursor at 0).
func (wc *writeCache) initCursor(key fileKey, cursor int64) {
	if !wc.enabled() {
		return
	}
	if _, ok := wc.perFile[key]; ok {
		return
	}
	wc.perFile[key] = &fileBuf{articles: make(map[int64]bufferedArticle), writeCursor: cursor}
}

// cursorFor returns the file's current contiguous write frontier, or 0 if the
// file has no buffer entry — caching is disabled, or the entry was dropped by
// forget. A drain is not one of those cases: drainFile retains the entry
// precisely so its advanced cursor survives, so after a pressure flush this
// returns the advanced frontier rather than 0.
func (wc *writeCache) cursorFor(key fileKey) int64 {
	if fb, ok := wc.perFile[key]; ok {
		return fb.writeCursor
	}
	return 0
}

// buffer adds an article to the cache. cached reports whether the article was
// taken; false means caching is disabled and the caller should write
// immediately.
//
// displaced carries the identity of an article evicted from the same offset,
// and is nil in every ordinary case. The caller must settle it — as a failure,
// so it is fetched again — rather than dropping it. Returning it is not
// bookkeeping for its own sake: once an ack waits on a write, an article
// silently removed from the cache is an article that is never acked at all and
// never re-dispatched, which is the defect this whole path exists to close.
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
	// If this offset already exists (shouldn't happen in practice due to
	// upstream dedup), replace it and adjust accounting.
	if existing, dup := fb.articles[art.offset]; dup {
		wc.used -= int64(len(existing.data))
		fb.totalBytes -= int64(len(existing.data))
		if existing.data != nil {
			decoder.PutBuffer(existing.data)
		}
		displaced = []articleID{existing.id}
	}
	fb.articles[art.offset] = art
	size := int64(len(art.data))
	fb.totalBytes += size
	wc.used += size
	return true, displaced
}

// buffered reports whether an article is still sitting unwritten at this
// offset. It distinguishes an article the cache has yet to write from one it
// has already written and acked, which the two look identical from outside.
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
	ids := make([]articleID, 0, len(runArticles))
	for _, art := range runArticles {
		wc.scratchBuf = append(wc.scratchBuf, art.data...)
		// Copied out of art rather than read back from the map: the entry is
		// gone by the time the run reaches disk, and the identity is what the
		// write needs in order to ack.
		ids = append(ids, art.id)
		delete(fb.articles, art.offset)
		fb.totalBytes -= int64(len(art.data))
		wc.used -= int64(len(art.data))
		if art.data != nil {
			decoder.PutBuffer(art.data)
		}
	}
	fb.writeCursor = cursor

	return &flushRun{offset: startOffset, data: wc.scratchBuf, ids: ids}
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

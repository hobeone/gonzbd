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
	// articles maps byte offset → decoded article data.
	articles map[int64][]byte
	// writeCursor tracks the next expected contiguous offset for this file.
	// Initially 0; advances as contiguous runs are flushed.
	writeCursor int64
	// totalBytes is the sum of len(data) for all buffered articles.
	totalBytes int64
}

// bufferedArticle is a flushable article with its offset and data.
type bufferedArticle struct {
	offset int64
	data   []byte
}

// flushRun is a contiguous run of articles that can be written as a single WriteAt.
type flushRun struct {
	offset int64  // starting byte offset
	data   []byte // coalesced data
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
	wc.perFile[key] = &fileBuf{articles: make(map[int64][]byte), writeCursor: cursor}
}

// cursorFor returns the file's current contiguous write frontier, or 0 if the
// file has no buffer entry (caching disabled, or already drained).
func (wc *writeCache) cursorFor(key fileKey) int64 {
	if fb, ok := wc.perFile[key]; ok {
		return fb.writeCursor
	}
	return 0
}

// buffer adds an article to the cache. Returns true if the article was
// buffered, false if caching is disabled (caller should write immediately).
func (wc *writeCache) buffer(key fileKey, offset int64, data []byte) bool {
	if !wc.enabled() {
		return false
	}
	fb, ok := wc.perFile[key]
	if !ok {
		fb = &fileBuf{articles: make(map[int64][]byte)}
		wc.perFile[key] = fb
	}
	// If this offset already exists (shouldn't happen in practice due to
	// upstream dedup), replace it and adjust accounting.
	if existing, dup := fb.articles[offset]; dup {
		wc.used -= int64(len(existing))
		fb.totalBytes -= int64(len(existing))
		if existing != nil {
			decoder.PutBuffer(existing)
		}
	}
	fb.articles[offset] = data
	size := int64(len(data))
	fb.totalBytes += size
	wc.used += size
	return true
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
		data, ok := fb.articles[cursor]
		if !ok {
			break
		}
		runArticles = append(runArticles, bufferedArticle{offset: cursor, data: data})
		runSize += int64(len(data))
		cursor += int64(len(data))
	}

	if runSize < minSize {
		return nil
	}

	// Coalesce into wc.scratchBuf to eliminate heap allocations during write coalescing.
	wc.scratchBuf = wc.scratchBuf[:0]
	startOffset := fb.writeCursor
	for _, art := range runArticles {
		wc.scratchBuf = append(wc.scratchBuf, art.data...)
		delete(fb.articles, art.offset)
		fb.totalBytes -= int64(len(art.data))
		wc.used -= int64(len(art.data))
		if art.data != nil {
			decoder.PutBuffer(art.data)
		}
	}
	fb.writeCursor = cursor

	return &flushRun{offset: startOffset, data: wc.scratchBuf}
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
	for offset, data := range fb.articles {
		articles = append(articles, bufferedArticle{offset: offset, data: data})
		wc.used -= int64(len(data))
		if end := offset + int64(len(data)); end > fb.writeCursor {
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

// forget removes tracking for a file without returning data. Used when
// a file's cache has already been drained through contiguous flushes.
func (wc *writeCache) forget(key fileKey) {
	if fb, ok := wc.perFile[key]; ok {
		for _, data := range fb.articles {
			if data != nil {
				decoder.PutBuffer(data)
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

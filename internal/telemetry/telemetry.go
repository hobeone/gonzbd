// Package telemetry provides expvar-based pipeline performance counters.
//
// The scalar counters below are lock-free (atomic int64) and safe for
// concurrent increment from multiple goroutines. PipelineErrors is an
// expvar.Map (internally mutex-guarded rather than atomic, but still
// documented by the stdlib as safe for concurrent use). All are
// automatically exposed at /debug/vars by the expvar package.
//
// Usage in tests: call Reset() to zero all counters between test runs.
package telemetry

import "expvar"

// Pipeline counters — incremented by pipeline workers.
var (
	// ArticlesReceived counts articles dequeued from the completions channel.
	ArticlesReceived = expvar.NewInt("pipeline_articles_received")

	// ArticlesRetried counts articles returned to the dispatch pool
	// (retryable errors like connection drops, 430s).
	ArticlesRetried = expvar.NewInt("pipeline_articles_retried")

	// ArticlesFailed counts articles permanently failed (all servers
	// exhausted or unrecoverable decode error).
	ArticlesFailed = expvar.NewInt("pipeline_articles_failed")

	// ArticlesWritten counts articles successfully enqueued to the
	// assembler via WriteArticle.
	ArticlesWritten = expvar.NewInt("pipeline_articles_written")

	// PipelineErrors counts failures by classified cause (e.g.
	// "nntp_no_article", "crc_mismatch", "disk_write_error"), incremented
	// at the point of detection in the downloader and assembler packages.
	//
	// This counts attempts, not articles: connection-level failures
	// (auth, dial, transient server errors) are handled entirely within
	// the downloader's retry/penalty logic and never produce an
	// ArticleResult, so they only ever appear here, not in
	// ArticlesFailed/ArticlesRetried above. Its total should not be
	// compared 1:1 against those flat counters.
	PipelineErrors = expvar.NewMap("pipeline_errors_by_class")
)

// Assembler counters — incremented by the assembler worker goroutine.
var (
	// DiskWrites counts individual WriteAt syscalls to target files.
	DiskWrites = expvar.NewInt("assembler_disk_writes")

	// DiskWriteBytes counts total bytes written via WriteAt.
	DiskWriteBytes = expvar.NewInt("assembler_disk_write_bytes")

	// CacheHits counts articles that were buffered in the write cache
	// instead of written immediately.
	CacheHits = expvar.NewInt("assembler_cache_hits")

	// CacheFlushes counts coalesced flush operations (each flush may
	// write multiple articles' worth of data in a single WriteAt).
	CacheFlushes = expvar.NewInt("assembler_cache_flushes")

	// CacheFlushBytes counts total bytes written via coalesced flushes.
	CacheFlushBytes = expvar.NewInt("assembler_cache_flush_bytes")

	// CachePressureFlushes counts force-flushes triggered by memory
	// pressure (cache > 90% of limit).
	CachePressureFlushes = expvar.NewInt("assembler_cache_pressure_flushes")

	// FilesCompleted counts target files fully assembled (all parts
	// written, fsync'd, and closed).
	FilesCompleted = expvar.NewInt("assembler_files_completed")

	// PreallocCalls counts file pre-allocation calls (fallocate/ftruncate).
	PreallocCalls = expvar.NewInt("assembler_prealloc_calls")
)

// Reset zeros all counters. Intended for use between test runs to
// prevent cross-test interference.
func Reset() {
	ArticlesReceived.Set(0)
	ArticlesRetried.Set(0)
	ArticlesFailed.Set(0)
	ArticlesWritten.Set(0)
	DiskWrites.Set(0)
	DiskWriteBytes.Set(0)
	CacheHits.Set(0)
	CacheFlushes.Set(0)
	CacheFlushBytes.Set(0)
	CachePressureFlushes.Set(0)
	FilesCompleted.Set(0)
	PreallocCalls.Set(0)
	PipelineErrors.Init()
}

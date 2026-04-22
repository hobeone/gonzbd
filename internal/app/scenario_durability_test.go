package app_test

import (
	"testing"
)

// TestDurability_DoneMeansOnDisk verifies that an article flagged Done in
// the queue is actually on disk — i.e. the Done flag is set from the
// assembler write path, not from the downloader's NNTP-receive path.
//
// Shape (to be implemented when the skip is removed in B.6):
//  1. Instrument the assembler so its WriteArticle can be paused after the
//     pipeline hands it a body but before pwrite+fsync completes.
//  2. Download one article, pause inside the assembler, then Shutdown.
//     The crash simulated here: the process died after the downloader
//     sent its ArticleResult but before the bytes were durable.
//  3. Re-open the admin dir and assert the article is NOT marked Done —
//     i.e. the next run will re-fetch it.
//     Bonus: assert that a file whose articles all arrive as
//     WriteRequest{FatalErr} still fires OnFileComplete (issue #3).
//
// EXPECTED FAIL until B.6: handleRequest in dispatch.go calls
// queue.MarkArticleDone before the body is ever written. A crash between
// that call and the assembler's pwrite leaves the queue lying about what
// is on disk.
func TestDurability_DoneMeansOnDisk(t *testing.T) {
	t.Skip("EXPECTED FAIL until B.6: MarkArticleDone is called before the assembler writes the bytes")
}

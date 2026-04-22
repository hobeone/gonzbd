package app_test

import (
	"testing"
)

// TestReload_NoArticleLossInFlight verifies that ReloadDownloader does not
// drop articles whose ArticleResult had been emitted onto the completions
// channel but not yet consumed by the pipeline at swap time.
//
// Shape (to be implemented when the skip is removed in B.5/B.6):
//  1. Enqueue a job with N articles.
//  2. Stall the scripted server on half of them so they are dispatched but
//     pending at reload time; let the rest emit and back up the completions
//     channel.
//  3. Call ReloadDownloader with the same server config.
//  4. Release all stalled articles and wait for post-proc.
//  5. Assert all N articles appear as Done in the queue snapshot and the
//     assembled file on disk matches the expected bytes.
//
// EXPECTED FAIL until B.6 (likely flips green automatically): under the
// current "Done=downloaded" contract, dropped in-flight emissions leave
// the queue claiming progress that never reached the assembler. Once B.6
// moves MarkArticleDone to the assembler write path, a dropped result
// just means the new downloader re-dispatches the article.
func TestReload_NoArticleLossInFlight(t *testing.T) {
	t.Skip("EXPECTED FAIL until B.6 (likely auto-fix) / B.5 (verify): ReloadDownloader can drop in-flight article results")
}

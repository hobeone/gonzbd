// Package directunpack implements streaming RAR extraction during download.
//
// When enabled, each RAR set is extracted in-process by rarengine while the
// job downloads: completed volumes are handed to its reader in order as they
// arrive, and it blocks at a volume boundary until the next volume is fed.
// This overlaps download I/O with extraction I/O, reducing total pipeline
// wall-clock time. A set that is not RAR, or a RAR 1.5-4.x set rarengine
// does not decode (a compressed or encrypted member), is skipped and left to
// the post-processing unpack stage.
//
// Each set is extracted into its own .gonzbd-tmp-unpack-* staging directory
// inside the job directory (unpack.ExtractStaged). Its members are published
// into the job directory only when the whole set extracts without an error; a
// set whose extraction fails or is aborted has its staging directory removed
// and leaves nothing in the job directory for the post-processing unpack
// stage, which extracts it again, to collide with. A publish that fails part
// way is undone: what it created is removed, and a removal that fails is
// reported in the error. A set that published is
// reported in Results even when Abort follows, and the post-processing unpack
// stage skips a set it is handed as extracted. A staging directory a crash
// leaves behind is removed by unpack.CleanupStageDirs, which post-processing's
// temp sweep runs on the job directory.
package directunpack

import "github.com/hobeone/gonzbd/internal/rarheader"

// AnalyzeRarFilename parses a RAR volume filename into its set name and
// 1-based volume number. Only the base filename is considered (directory
// components are stripped).
//
// Returns ("", 0) for non-RAR files.
//
// Naming conventions:
//
//	movie.part01.rar  → ("movie", 1)
//	movie.part02.rar  → ("movie", 2)
//	movie.rar         → ("movie", 1)        // legacy main volume
//	movie.r00         → ("movie", 2)        // legacy: r00 = volume 2
//	movie.r01         → ("movie", 3)        // legacy: r01 = volume 3
func AnalyzeRarFilename(filename string) (setname string, volume int) {
	return rarheader.AnalyzeRarFilename(filename)
}

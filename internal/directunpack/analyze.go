// Package directunpack implements streaming RAR extraction during download.
//
// When enabled, completed RAR volumes are fed to an interactive `unrar -vp`
// subprocess as they arrive. The subprocess pauses at each volume boundary
// and waits for the next volume to be signaled. This overlaps download I/O
// with extraction I/O, reducing total pipeline wall-clock time.
//
// DirectUnpack is purely additive — it never modifies the existing unpack
// stage. On any error the subprocess is killed, partial extracts are cleaned
// up, and the standard post-processing unpack stage runs as if DirectUnpack
// never existed.
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

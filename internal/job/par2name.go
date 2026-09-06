package job

import "regexp"

// recoveryVolumePattern matches the .volNNN+MM.par2 / .volNNN-MM.par2
// component of a par2 recovery-volume filename anywhere in the string.
// The index par2 (e.g. "movie.par2", no .volNNN block suffix) does NOT
// match — it carries the per-file checksums and is never deferred.
//
// Decoupled from internal/par2 to simplify imports.
var recoveryVolumePattern = regexp.MustCompile(`(?i)\.vol\d+[-+]\d+\.par2(?:[^a-zA-Z0-9.]|$)`)

// IsRecoveryVolume reports whether filename names a par2 recovery volume.
func IsRecoveryVolume(filename string) bool {
	return recoveryVolumePattern.MatchString(filename)
}

// IsPar2File reports whether subject names a par2 file (index or recovery volume).
func IsPar2File(subject string) bool {
	return isPar2File(subject)
}

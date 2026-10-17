package job

import (
	"regexp"
	"strconv"
)

// recoveryVolumePattern matches the .volNNN+MM.par2 / .volNNN-MM.par2
// component of a par2 recovery-volume filename anywhere in the string.
// The index par2 (e.g. "movie.par2", no .volNNN block suffix) does NOT
// match — it carries the per-file checksums and is never deferred.
//
// In the "+" form the number is a block count, so ".vol000+00.par2" declares
// no recovery blocks and does not match: it is classified as content, which is
// always fetched and never counted as capacity. In the "-" form the second
// number is a volume number, so "-00" carries no such implication.
//
// The one capture group is the "+" form's block count; RecoveryVolumeBlocks
// reads it, so volume detection and block counting cannot disagree about
// which names are volumes.
//
// Decoupled from internal/par2 to simplify imports.
var recoveryVolumePattern = regexp.MustCompile(`(?i)\.vol\d+(?:-\d+|\+(0*[1-9]\d*))\.par2(?:[^a-zA-Z0-9.]|$)`)

// IsRecoveryVolume reports whether filename names a par2 recovery volume.
func IsRecoveryVolume(filename string) bool {
	return recoveryVolumePattern.MatchString(filename)
}

// RecoveryVolumeBlocks returns the recovery block count a volume's name
// declares, the MM of ".volNN+MM.par2". ok is false for a name that is not a
// recovery volume and for the "-NN" form, whose second number is a volume
// number rather than a count. The name is the only source of a held volume's
// count, since a held volume has not been downloaded.
func RecoveryVolumeBlocks(filename string) (blocks int, ok bool) {
	m := recoveryVolumePattern.FindStringSubmatch(filename)
	if m == nil {
		return 0, false
	}
	// The "-" form leaves the group empty, which Atoi rejects.
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// IsPar2File reports whether subject names a par2 file (index or recovery volume).
func IsPar2File(subject string) bool {
	return isPar2File(subject)
}

package par2

import (
	"os"
	"strings"
)

// IsBackupNumber reports whether suffix is a par2 backup index ("1".."999",
// non-zero-padded) as emitted by par2cmdline when renaming a damaged file to
// "<target>.1", "<target>.2", etc.
func IsBackupNumber(suffix string) bool {
	if suffix == "" || len(suffix) > 3 || suffix[0] < '1' || suffix[0] > '9' {
		return false
	}
	for i := 1; i < len(suffix); i++ {
		if suffix[i] < '0' || suffix[i] > '9' {
			return false
		}
	}
	return true
}

// IsBackupNamedFor reports whether candidateName has a "<targetName>.<N>"
// par2 backup name alongside an existing regular file targetName whose first
// 16 KiB MD5 equals wantHash (#768). It hashes targetName (not candidateName).
func IsBackupNamedFor(root *os.Root, candidateName, targetName string, wantHash [16]byte) bool {
	suffix, ok := strings.CutPrefix(candidateName, targetName+".")
	if !ok || !IsBackupNumber(suffix) {
		return false
	}
	info, err := root.Lstat(targetName)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	targetHash, err := ComputeHash16kRoot(root, targetName)
	return err == nil && targetHash == wantHash
}

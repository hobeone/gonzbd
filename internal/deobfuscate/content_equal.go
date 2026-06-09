package deobfuscate

import (
	"bytes"
	"io"
	"os"

	"github.com/hobeone/gonzbd/internal/par2"
)

// contentEqual reports whether pathA and pathB have identical content.
// knownHashOfA is the MD5-of-first-16KB already computed for pathA, avoiding
// a redundant read. Files ≤ 16384 bytes are compared entirely via this hash
// (which covers the whole file). Larger files fall back to a streaming
// chunk-by-chunk comparison.
func contentEqual(pathA, pathB string, knownHashOfA [16]byte) (bool, error) {
	statA, err := os.Stat(pathA)
	if err != nil {
		return false, err
	}
	statB, err := os.Stat(pathB)
	if err != nil {
		return false, err
	}
	if statA.Size() != statB.Size() {
		return false, nil
	}

	hashB, err := par2.ComputeHash16k(pathB)
	if err != nil {
		return false, err
	}
	if knownHashOfA != hashB {
		return false, nil
	}

	// For files that fit entirely within 16KB the hash covers all content.
	if statA.Size() <= 16384 {
		return true, nil
	}

	// Larger files: stream both and compare chunk by chunk.
	return streamEqual(pathA, pathB)
}

// streamEqual reads pathA and pathB in 32KB chunks and returns true if every
// byte is identical. Assumes callers have already verified equal sizes.
func streamEqual(pathA, pathB string) (bool, error) {
	fa, err := os.Open(pathA) //nolint:gosec // read-only open of paths validated inside deobfuscation
	if err != nil {
		return false, err
	}
	defer fa.Close() //nolint:errcheck // read-only file close error is safe to ignore

	fb, err := os.Open(pathB) //nolint:gosec // read-only open of paths validated inside deobfuscation
	if err != nil {
		return false, err
	}
	defer fb.Close() //nolint:errcheck // read-only file close error is safe to ignore

	const chunk = 32 * 1024
	bufA := make([]byte, chunk)
	bufB := make([]byte, chunk)

	for {
		nA, errA := io.ReadFull(fa, bufA)
		nB, errB := io.ReadFull(fb, bufB)
		if nA != nB || !bytes.Equal(bufA[:nA], bufB[:nB]) {
			return false, nil
		}
		done := errA == io.EOF || errA == io.ErrUnexpectedEOF
		if done {
			return errB == io.EOF || errB == io.ErrUnexpectedEOF, nil
		}
		if errA != nil {
			return false, errA
		}
		if errB != nil {
			return false, errB
		}
	}
}

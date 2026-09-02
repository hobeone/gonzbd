package postproc

import (
	"crypto/md5" //nolint:gosec // par2 records MD5; matching it is the point
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestQuickCheckStage_VerifiesAgainstThePostRenameName pins that verification
// reads the names this stage's own relocation just produced.
//
// The stage does two things in order: it relocates files to the paths par2
// names, and it then verifies them by matching those names against the par2
// index. The second step reads JobProgress.Filename, which still holds the
// PRE-rename name — this stage has no way to update it, because postproc.Job
// carries a *queue.Job snapshot rather than a *queue.Queue, and there is no
// writer behind a snapshot.
//
// Reading it straight compares a name that no longer exists on disk against
// the index, matches nothing, counts the file Unverified and sets
// QuickCheckDamaged — forcing a par2 repair over a job that is intact. The
// mapping is applied in memory instead, which is why this asserts Clean.
//
// internal/app's applyPar2Names does persist the same correction on the
// download path, but that path is conditional: on-demand par2 can be disabled,
// and a release with no deferred recovery volumes never reaches it. This stage
// runs either way, so it cannot rely on it.
func TestQuickCheckStage_VerifiesAgainstThePostRenameName(t *testing.T) {
	t.Parallel()

	_, dir := stageJob(t)

	// An OBFUSCATED delivered name, not a subdirectory relocation.
	//
	// The distinction is what makes this test discriminate at all. VerifyCRCs
	// keys the par2 index by BASENAME (verifycrc.go: "basename → entry") and
	// falls back to the basename of the delivered name, so a file relocated
	// from "shot.jpg" to "Screens/shot.jpg" still matches under its stale
	// name — the two basenames are the same string. Only a rename that
	// changes the basename can expose a missing remap, and that is precisely
	// the obfuscated case this PR extended quickcheck to handle.
	const obfuscated = "7xq6N6P340dCh9Lnih5hY3jsArfSN1"
	content := []byte("payload bytes for the rename-remap verification test")
	if err := os.WriteFile(filepath.Join(dir, obfuscated), content, 0o600); err != nil {
		t.Fatal(err)
	}

	crc := crc32.ChecksumIEEE(content)
	size := uint64(len(content))
	hash16k := md5.Sum(content) //nolint:gosec // par2 compatibility, not security

	setID := [16]byte{3, 5, 7, 9}
	fileID := [16]byte{44, 55, 66}
	par2Bytes := buildPacket(setID, typeMain, buildMainBody(size, fileID))
	par2Bytes = append(par2Bytes, buildPacket(setID, typeFileDesc,
		buildFileDescBody(fileID, [16]byte{0xBB}, hash16k, size, "data.bin"))...)
	par2Bytes = append(par2Bytes, buildPacket(setID, typeIFSC,
		buildIFSCBody(fileID, hash16k, crc))...)
	if err := os.WriteFile(filepath.Join(dir, "set.par2"), par2Bytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// The queue records the file under its delivered obfuscated name, which is
	// the ordinary state: nothing has corrected it, and this stage cannot.
	job := &Job{
		Queue:       buildQCJob(t, "remap-job", obfuscated, int64(size), crc),
		DownloadDir: dir,
	}

	stage := &QuickCheckStage{Log: slog.New(slog.DiscardHandler)}
	stage.SetEnabled(true)
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "data.bin")); err != nil {
		t.Fatalf("fixture guard: the file was not renamed, so this test proves nothing: %v", err)
	}

	if job.QuickCheck != QuickCheckClean {
		t.Errorf("QuickCheck = %s, want clean: the file is intact and now carries exactly the name par2 gives it, "+
			"so anything else sends a healthy job to par2 repair", job.QuickCheck)
	}
}

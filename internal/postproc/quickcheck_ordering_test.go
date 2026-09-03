package postproc

import (
	"crypto/md5" //nolint:gosec // par2 records MD5; matching it is the point
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestQuickCheckStage_VerifiesBeforeRenaming pins the ordering this stage now
// depends on. The name says which order, because the previous one was the
// defect.
//
// The stage assesses the directory, then relocates. Verification therefore
// compares each CRC against the name the file has WHILE it is being assessed,
// which is the name JobProgress.Filename already holds — so nothing has to be
// corrected, remapped or re-read.
//
// It used to run the other way: relocate, then verify by matching names
// against the par2 index. That invalidated exactly the names verification
// needed, and since postproc.Job carries a *queue.Job snapshot with no writer
// behind it, this stage had nowhere to record the correction. It compensated
// with a local rename map — correct, but a second enforcement point for an
// ordering internal/app enforced separately, which is #494. There is no longer
// a map to forget to apply, because there is no longer a window in which the
// names are wrong.
//
// The obfuscated name is what makes this discriminate. Verification joins on
// the BASENAME, so a file relocated from "shot.jpg" to "Screens/shot.jpg"
// joins under either name — the basenames are equal. Only a rename that
// CHANGES the basename can expose a verdict taken after the move, and an
// earlier draft of this test used the subdirectory fixture and the mutation
// SURVIVED.
func TestQuickCheckStage_VerifiesBeforeRenaming(t *testing.T) {
	t.Parallel()

	_, dir := stageJob(t)

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
		Job:         buildQCJob(t, "remap-job", obfuscated, int64(size), crc),
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
		t.Errorf("QuickCheck = %s, want clean: the file is intact and was identified under the name the queue "+
			"knows it by, so anything else means the verdict was taken after the rename moved it", job.QuickCheck)
	}
}

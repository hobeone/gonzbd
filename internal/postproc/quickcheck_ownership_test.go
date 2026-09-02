package postproc

import (
	"crypto/md5"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// A file quickcheck relocates must stay owned by the job.
//
// Job.OwnedFiles is seeded once from disk before any stage runs, and every
// stage that moves a file afterwards extends it — stage_deobfuscate,
// stage_par2names and stage_rarvolrecovery all call markRenamed. quickcheck
// did not, despite performing exactly that operation, so a relocated file left
// its old path in the set and its new path absent.
//
// The consequence is bounded but real: the ownership guards in
// extension_cleanup and sample_cleanup return early for a path they do not
// own, so the relocated file would be skipped rather than considered. Junk
// left behind, never data loss — which is why this went unnoticed.
func TestQuickCheckStage_RelocationKeepsOwnership(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)

	// A par2 set naming a file in a subdirectory, with the file delivered
	// flat, which is what makes quickcheck relocate it.
	content := []byte("payload bytes for the relocation ownership test")
	flat := filepath.Join(dir, "shot.jpg")
	if err := os.WriteFile(flat, content, 0o600); err != nil {
		t.Fatal(err)
	}

	setID := [16]byte{2, 4, 6, 8}
	fileID := [16]byte{11, 22, 33}
	hash16k := md5.Sum(content)
	size := uint64(len(content))
	par2Bytes := buildPacket(setID, typeMain, buildMainBody(size, fileID))
	par2Bytes = append(par2Bytes, buildPacket(setID, typeFileDesc,
		buildFileDescBody(fileID, [16]byte{0xAA}, hash16k, size, "Screens/shot.jpg"))...)
	par2Bytes = append(par2Bytes, buildPacket(setID, typeIFSC,
		buildIFSCBody(fileID, hash16k, crc32.ChecksumIEEE(content)))...)
	if err := os.WriteFile(filepath.Join(dir, "set.par2"), par2Bytes, 0o600); err != nil {
		t.Fatal(err)
	}

	job.OwnedFiles = map[string]struct{}{flat: {}}

	stage := &QuickCheckStage{Log: slog.New(slog.DiscardHandler)}
	stage.SetEnabled(true)
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	moved := filepath.Join(dir, "Screens", "shot.jpg")
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("fixture guard: the file was not relocated, so this test proves nothing: %v", err)
	}

	if _, owned := job.OwnedFiles[moved]; !owned {
		t.Errorf("the relocated file is not owned; OwnedFiles = %v", keysOf(job.OwnedFiles))
	}
	if _, stale := job.OwnedFiles[flat]; stale {
		t.Errorf("the pre-move path is still owned; OwnedFiles = %v", keysOf(job.OwnedFiles))
	}
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

package par2

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- ParseRepairOutput tests ---

func TestParseRepairOutput_AllFilesCorrect(t *testing.T) {
	t.Parallel()

	// Direct references for alignment check
	_ = parseOptionOrValidity
	_ = parseFinalStateOrFailure
	_ = parseRepairProgress
	_ = parsePhaseLine
	_ = parseGlobalLine

	output := `Loading "data.par2".
Loaded 8 new packets
All files are correct, repair is not required.
`
	ro := ParseRepairOutput(output)
	if !ro.Finished {
		t.Error("expected Finished=true")
	}
	if ro.Status != StatusAllFilesOK {
		t.Errorf("Status = %d, want StatusAllFilesOK", ro.Status)
	}
	if len(ro.DamagedTargets) != 0 {
		t.Errorf("DamagedTargets = %v, want empty", ro.DamagedTargets)
	}
}

func TestParseRepairOutput_RepairComplete(t *testing.T) {
	t.Parallel()
	output := `Verifying source files:
Target: "data.bin" - damaged. Found 3 of 4 data blocks.
Repair is required.
1 file(s) exist but are damaged.
Repair is possible using 1 recovery block.
Repairing: 0.0%
Repairing: 50.0%
Repairing: 100.0%
Repair complete.
Verifying repaired files:
Target: "data.bin" - found.
`
	ro := ParseRepairOutput(output)
	if !ro.Finished {
		t.Error("expected Finished=true")
	}
	if ro.Status != StatusRepairComplete {
		t.Errorf("Status = %d, want StatusRepairComplete", ro.Status)
	}
	if ro.VerifyTotal != 1 {
		t.Errorf("VerifyTotal = %d, want 1", ro.VerifyTotal)
	}
	if ro.RepairPercent != 100.0 {
		t.Errorf("RepairPercent = %f, want 100.0", ro.RepairPercent)
	}
	if len(ro.DamagedTargets) != 1 || ro.DamagedTargets[0] != "data.bin" {
		t.Errorf("DamagedTargets = %v, want [data.bin]", ro.DamagedTargets)
	}
}

func TestParseRepairOutput_NeedMoreBlocks(t *testing.T) {
	t.Parallel()
	output := `Verifying source files:
Target: "data.bin" - damaged. Found 1 of 4 data blocks.
Repair is required.
3 file(s) are missing.
Repair is not possible.
You need 5 more recovery blocks to be able to repair.
`
	ro := ParseRepairOutput(output)
	if ro.Finished {
		t.Error("expected Finished=false")
	}
	if !ro.NeedMoreBlocks {
		t.Error("expected NeedMoreBlocks=true")
	}
	if ro.BlocksNeeded != 5 {
		t.Errorf("BlocksNeeded = %d, want 5", ro.BlocksNeeded)
	}
	if ro.Status != StatusNeedMoreBlocks {
		t.Errorf("Status = %d, want StatusNeedMoreBlocks", ro.Status)
	}
}

func TestParseRepairOutput_MainPacketNotFound(t *testing.T) {
	t.Parallel()
	output := `Main packet not found
`
	ro := ParseRepairOutput(output)
	if ro.Status != StatusInvalidPar2 {
		t.Errorf("Status = %d, want StatusInvalidPar2", ro.Status)
	}
}

func TestParseRepairOutput_InvalidOption(t *testing.T) {
	t.Parallel()
	output := `Invalid option specified: -t+
`
	ro := ParseRepairOutput(output)
	if !ro.InvalidOption {
		t.Error("expected InvalidOption=true")
	}
	if ro.Status != StatusBadOption {
		t.Errorf("Status = %d, want StatusBadOption", ro.Status)
	}
}

func TestParseRepairOutput_RepairFailed(t *testing.T) {
	t.Parallel()
	output := `Verifying source files:
Repair is required.
Repair is possible.
Repairing: 50.0%
Repair Failed.
`
	ro := ParseRepairOutput(output)
	if ro.Finished {
		t.Error("expected Finished=false (Repair Failed resets it)")
	}
	if ro.Status != StatusRepairFailed {
		t.Errorf("Status = %d, want StatusRepairFailed", ro.Status)
	}
}

func TestParseRepairOutput_CannotRename(t *testing.T) {
	t.Parallel()
	output := `"file.dat.1" cannot be renamed to "file.dat"
`
	ro := ParseRepairOutput(output)
	if ro.Status != StatusRepairNotPossible {
		t.Errorf("Status = %d, want StatusRepairNotPossible", ro.Status)
	}
}

func TestParseRepairOutput_DiskFull(t *testing.T) {
	t.Parallel()
	output := `There is not enough space on the disk
`
	ro := ParseRepairOutput(output)
	if ro.Status != StatusDiskFull {
		t.Errorf("Status = %d, want StatusDiskFull", ro.Status)
	}
}

func TestParseRepairOutput_InsufficientDiskSpace(t *testing.T) {
	t.Parallel()
	output := `Insufficient disk space to write repaired file
`
	ro := ParseRepairOutput(output)
	if ro.Status != StatusDiskFull {
		t.Errorf("Status = %d, want StatusDiskFull", ro.Status)
	}
}

func TestParseRepairOutput_NoDetailsAvailable(t *testing.T) {
	t.Parallel()
	output := `No details available for recoverable file: data.bin
`
	ro := ParseRepairOutput(output)
	if ro.Status != StatusRepairNotPossible {
		t.Errorf("Status = %d, want StatusRepairNotPossible", ro.Status)
	}
}

// --- Rename and extra file tracking ---

func TestParseRepairOutput_IsMatchForRename(t *testing.T) {
	t.Parallel()
	output := `Verifying source files:
Target: "data.bin" - found.
Scanning extra files:
File: "abcdef123456.bin" - is a match for "data.bin"
`
	ro := ParseRepairOutput(output)
	if len(ro.Renames) != 1 {
		t.Fatalf("Renames len = %d, want 1", len(ro.Renames))
	}
	if got := ro.Renames["data.bin"]; got != "abcdef123456.bin" {
		t.Errorf("Renames[data.bin] = %q, want %q", got, "abcdef123456.bin")
	}
}

func TestParseRepairOutput_DataBlocksFromJoinable(t *testing.T) {
	t.Parallel()
	output := `Verifying source files:
Target: "data.bin" - found.
Scanning extra files:
File: "data.001" - found 3 of 4 data blocks from "data.bin"
`
	ro := ParseRepairOutput(output)
	if len(ro.UsedJoinables) != 1 {
		t.Fatalf("UsedJoinables len = %d, want 1", len(ro.UsedJoinables))
	}
	if ro.UsedJoinables[0] != "data.001" {
		t.Errorf("UsedJoinables[0] = %q, want %q", ro.UsedJoinables[0], "data.001")
	}
	if got := ro.Renames["data.bin"]; got != "data.001" {
		t.Errorf("Renames[data.bin] = %q, want %q", got, "data.001")
	}
}

func TestParseRepairOutput_DuplicateDataBlocks(t *testing.T) {
	t.Parallel()
	output := `Verifying source files:
Target: "extra_copy.bin" - found. It has duplicate data blocks.
`
	ro := ParseRepairOutput(output)
	if len(ro.UsedForRepair) != 1 {
		t.Fatalf("UsedForRepair len = %d, want 1", len(ro.UsedForRepair))
	}
	if ro.UsedForRepair[0] != "extra_copy.bin" {
		t.Errorf("UsedForRepair[0] = %q, want %q", ro.UsedForRepair[0], "extra_copy.bin")
	}
}

// --- Progress tracking ---

func TestParseRepairOutput_RecoverableFiles(t *testing.T) {
	t.Parallel()
	output := `There are 5 recoverable files
Verifying source files:
`
	ro := ParseRepairOutput(output)
	if ro.RecoverableFiles != 5 {
		t.Errorf("RecoverableFiles = %d, want 5", ro.RecoverableFiles)
	}
}

func TestParseRepairOutput_RepairProgress(t *testing.T) {
	t.Parallel()
	output := `Repair is required.
Repair is possible.
Repairing: 0.0%
Repairing: 33.3%
Repairing: 66.7%
Repairing: 100.0%
Repair complete.
`
	ro := ParseRepairOutput(output)
	if ro.RepairPercent != 100.0 {
		t.Errorf("RepairPercent = %f, want 100.0", ro.RepairPercent)
	}
	if !ro.Finished {
		t.Error("expected Finished=true")
	}
}

func TestParseRepairOutput_ProcessingProgress(t *testing.T) {
	t.Parallel()
	// "Processing:" is the par2 variant for join-only operations.
	output := `Repair is possible.
Processing: 50.0%
Processing: 100.0%
Repair complete.
`
	ro := ParseRepairOutput(output)
	if ro.RepairPercent != 100.0 {
		t.Errorf("RepairPercent = %f, want 100.0", ro.RepairPercent)
	}
}

func TestParseRepairOutput_VerifyingRepairedFiles(t *testing.T) {
	t.Parallel()
	output := `Repair is required.
1 file(s) exist but are damaged.
Repair is possible.
Repairing: 100.0%
Repair complete.
Verifying repaired files:
Target: "data.bin" - found.
Target: "info.txt" - found.
`
	ro := ParseRepairOutput(output)
	if ro.Phase != PhaseVerifyingRepair {
		t.Errorf("Phase = %d, want PhaseVerifyingRepair", ro.Phase)
	}
	if ro.VerifyNum != 2 {
		t.Errorf("VerifyNum = %d, want 2", ro.VerifyNum)
	}
}

func TestParseRepairOutput_MissingAndDamaged(t *testing.T) {
	t.Parallel()
	output := `Repair is required.
2 file(s) are missing.
3 file(s) exist but are damaged.
`
	ro := ParseRepairOutput(output)
	if ro.VerifyTotal != 5 {
		t.Errorf("VerifyTotal = %d, want 5 (2 missing + 3 damaged)", ro.VerifyTotal)
	}
}

// --- Progress line filtering ---

func TestParseRepairOutput_ProgressLinesFiltered(t *testing.T) {
	t.Parallel()
	output := `Loading: "data.par2"
Scanning: "data.bin"
All files are correct.
Repairing: 50.0%
`
	ro := ParseRepairOutput(output)
	// Loading, Scanning, and Repairing lines should be filtered.
	for _, line := range ro.Lines {
		for _, prefix := range progressFilterPrefixes {
			if strings.HasPrefix(line, prefix) {
				t.Errorf("progress line %q should have been filtered", line)
			}
		}
	}
	// "All files are correct" should be retained.
	found := false
	for _, line := range ro.Lines {
		if strings.Contains(line, "All files are correct") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'All files are correct' in retained Lines")
	}
}

// --- Edge cases ---

func TestParseRepairOutput_EmptyOutput(t *testing.T) {
	t.Parallel()
	ro := ParseRepairOutput("")
	if ro.Finished {
		t.Error("expected Finished=false for empty output")
	}
	if ro.Status != StatusUnknown {
		t.Errorf("Status = %d, want StatusUnknown for empty output", ro.Status)
	}
}

func TestParseRepairOutput_CouldNotWrite(t *testing.T) {
	t.Parallel()
	output := `Could not write to "data.bin" at offset 0: File exists
`
	ro := ParseRepairOutput(output)
	if !ro.Finished {
		t.Error("expected Finished=true (Could not write at offset 0 is treated as already-joined)")
	}
}

func TestParseRepairOutput_CannotSpecifyRecovery(t *testing.T) {
	t.Parallel()
	output := `Cannot specify recovery file count and redundancy at the same time
`
	ro := ParseRepairOutput(output)
	if !ro.InvalidOption {
		t.Error("expected InvalidOption=true")
	}
	if ro.Status != StatusBadOption {
		t.Errorf("Status = %d, want StatusBadOption", ro.Status)
	}
}

// --- parseStatus new case: StatusRepairComplete ---

func TestParseStatus_RepairComplete(t *testing.T) {
	t.Parallel()
	got := parseStatus("Repairing: 100.0%\nRepair complete.\nVerifying repaired files:\n")
	if got != StatusRepairComplete {
		t.Errorf("got %d, want StatusRepairComplete", got)
	}
}

// Verify parseStatus still gives failure priority over "Repair complete".
func TestParseStatus_RepairFailedOverComplete(t *testing.T) {
	t.Parallel()
	got := parseStatus("Repair complete.\nRepair Failed.\n")
	if got != StatusRepairFailed {
		t.Errorf("got %d, want StatusRepairFailed (failure should take priority)", got)
	}
}

// --- Full SABnzbd-style output ---

func TestParseRepairOutput_FullSABnzbdStyleOutput(t *testing.T) {
	t.Parallel()
	// Simulates the full output of a par2 repair run with obfuscated files.
	output := `Loading "movie.par2".
Loaded 24 new packets
There are 3 recoverable files
Verifying source files:
Target: "movie.mkv" - found.
Target: "movie.nfo" - found.
Target: "movie.srt" - missing.
Scanning extra files:
File: "abc123.srt" - is a match for "movie.srt"
Repair is required.
1 file(s) are missing.
Repair is possible using 1 recovery block.
Repairing: 0.0%
Repairing: 50.0%
Repairing: 100.0%
Repair complete.
Verifying repaired files:
Target: "movie.srt" - found.
`
	ro := ParseRepairOutput(output)

	if !ro.Finished {
		t.Error("expected Finished=true")
	}
	if ro.Status != StatusRepairComplete {
		t.Errorf("Status = %d, want StatusRepairComplete", ro.Status)
	}
	if ro.RecoverableFiles != 3 {
		t.Errorf("RecoverableFiles = %d, want 3", ro.RecoverableFiles)
	}
	if got := ro.Renames["movie.srt"]; got != "abc123.srt" {
		t.Errorf("Renames[movie.srt] = %q, want %q", got, "abc123.srt")
	}
	if len(ro.DamagedTargets) != 1 || ro.DamagedTargets[0] != "movie.srt" {
		t.Errorf("DamagedTargets = %v, want [movie.srt]", ro.DamagedTargets)
	}
	if ro.RepairPercent != 100.0 {
		t.Errorf("RepairPercent = %f, want 100.0", ro.RepairPercent)
	}
	if ro.Phase != PhaseVerifyingRepair {
		t.Errorf("Phase = %d, want PhaseVerifyingRepair", ro.Phase)
	}
}

func TestParseRepairOutput_CRDelimitedProgressLines(t *testing.T) {
	t.Parallel()
	// Real par2cmdline (1.1.1 and turbo) emits Target: and File: lines on the
	// same \n-delimited line as preceding \r-terminated "Scanning: NN.N%"
	// progress lines.
	output := "Verifying source files:\n" +
		"Opening: \"a.rar\"\n" +
		"Scanning: 10.0%\rScanning: 94.2%\rTarget: \"a.rar\" - damaged. Found 12 of 13 data blocks.\n" +
		"Scanning extra files:\n" +
		"Opening: \"obf.bin\"\n" +
		"Scanning: 50.0%\rScanning: 99.9%\rFile: \"obf.bin\" - is a match for \"b.rar\".\n" +
		"Repair is required.\n" +
		"1 file(s) exist but are damaged.\n" +
		"Repair is possible.\n" +
		"Repairing: 10.0%\rRepairing: 100.0%\rRepair complete.\n"

	ro := ParseRepairOutput(output)
	if !ro.Finished || ro.Status != StatusRepairComplete {
		t.Errorf("Finished=%v Status=%v, want true/%v", ro.Finished, ro.Status, StatusRepairComplete)
	}
	if len(ro.DamagedTargets) != 1 || ro.DamagedTargets[0] != "a.rar" {
		t.Errorf("DamagedTargets = %v, want [a.rar]", ro.DamagedTargets)
	}
	if got := ro.Renames["b.rar"]; got != "obf.bin" {
		t.Errorf("Renames[b.rar] = %q, want %q", got, "obf.bin")
	}
}

func TestIsBackupOfVerifiedTarget(t *testing.T) {
	t.Parallel()

	if !IsBackupNumber("1") || !IsBackupNumber("999") || IsBackupNumber("") || IsBackupNumber("01") || IsBackupNumber("1000") || IsBackupNumber("1a") {
		t.Error("IsBackupNumber classification failed")
	}

	dir := t.TempDir()
	targetData := []byte("verified-target-bytes")
	if err := os.WriteFile(filepath.Join(dir, "movie.rar"), targetData, 0o600); err != nil {
		t.Fatalf("WriteFile movie.rar: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "movie.rar.1"), targetData, 0o600); err != nil {
		t.Fatalf("WriteFile movie.rar.1: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subdir.rar"), 0o750); err != nil {
		t.Fatalf("MkdirAll subdir.rar: %v", err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	wantHash, err := ComputeHash16kRoot(root, "movie.rar")
	if err != nil {
		t.Fatalf("ComputeHash16kRoot: %v", err)
	}

	if !IsBackupOfVerifiedTarget(root, "movie.rar.1", "movie.rar", wantHash) {
		t.Error("IsBackupOfVerifiedTarget(movie.rar.1, movie.rar, wantHash) = false, want true")
	}
	if IsBackupOfVerifiedTarget(root, "movie.rar.01", "movie.rar", wantHash) {
		t.Error("IsBackupOfVerifiedTarget accepted zero-padded .01 suffix")
	}
	if IsBackupOfVerifiedTarget(root, "other.rar.1", "movie.rar", wantHash) {
		t.Error("IsBackupOfVerifiedTarget accepted mismatched target prefix")
	}
	if IsBackupOfVerifiedTarget(root, "subdir.rar.1", "subdir.rar", wantHash) {
		t.Error("IsBackupOfVerifiedTarget accepted directory target")
	}
	if IsBackupOfVerifiedTarget(root, "missing.rar.1", "missing.rar", wantHash) {
		t.Error("IsBackupOfVerifiedTarget accepted missing target")
	}
	var wrongHash [16]byte
	if IsBackupOfVerifiedTarget(root, "movie.rar.1", "movie.rar", wrongHash) {
		t.Error("IsBackupOfVerifiedTarget accepted mismatched target 16 KiB hash")
	}
}

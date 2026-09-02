package par2

import (
	"crypto/md5" //nolint:gosec // md5 used for PAR2 spec packet integrity
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyCRCs_NoSets(t *testing.T) {
	t.Parallel()

	// No par2 sets — all files are NotInPar2 (benign).
	files := []AssembledFile{
		{FileName: "movie.mkv", CRC32: 0x12345678},
		{FileName: "sample.avi", CRC32: 0xDEADBEEF},
	}

	result := VerifyCRCs(files, nil, slog.Default())

	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0 (no par2 sets)", result.Checked)
	}
	// With no par2 sets, VerifyCRCs returns early (Skipped=len(files))
	// via the "no par2 file descriptions found" path.
	// Since it returns before the loop, NotInPar2 is not incremented
	// — all counts stay zero. That's fine: no manifest means nothing
	// to verify.
}

func TestVerifyCRCs_NoCRC_NoPar2Sets(t *testing.T) {
	t.Parallel()

	// Files with CRC32=0, no par2 sets.
	files := []AssembledFile{
		{FileName: "movie.mkv", CRC32: 0},
		{FileName: "sample.avi", CRC32: 0},
	}

	result := VerifyCRCs(files, nil, slog.Default())

	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0", result.Checked)
	}
}

func TestVerifyCRCs_EmptyFiles(t *testing.T) {
	t.Parallel()

	result := VerifyCRCs(nil, nil, slog.Default())

	if result.Checked != 0 || result.Matched != 0 || result.Mismatched != 0 {
		t.Errorf("unexpected result for empty input: %+v", result)
	}
}

func TestCRCResult_Fields(t *testing.T) {
	t.Parallel()

	cr := CRCResult{
		FileName:     "test.dat",
		AssembledCRC: 0x12345678,
		Par2CRC:      0x12345678,
		Match:        true,
		Par2FileName: "Subdir/test.dat",
	}

	if !cr.Match {
		t.Error("expected Match=true for equal CRCs")
	}
	if cr.FileName != "test.dat" {
		t.Errorf("FileName = %q, want %q", cr.FileName, "test.dat")
	}
}

// P23: VerifyCRCs with real par2 fixture files.
func TestVerifyCRCs_WithRealPar2Fixture(t *testing.T) {
	t.Parallel()

	par2Path := "../../test/fixtures/par2/data.par2"
	if _, err := os.Stat(par2Path); err != nil {
		t.Skipf("par2 fixture not available: %v", err)
	}

	sets, err := FindPar2Files("../../test/fixtures/par2")
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}
	if len(sets) == 0 {
		t.Skip("no par2 sets found in fixture")
	}

	t.Run("matching CRC", func(t *testing.T) {
		t.Parallel()
		// data.bin has CRC32 = 0x1068AFA6
		files := []AssembledFile{
			{FileName: "data.bin", CRC32: 0x1068AFA6},
		}
		result := VerifyCRCs(files, sets, slog.Default())
		if result.Checked != 1 {
			t.Errorf("Checked = %d, want 1", result.Checked)
		}
		if result.Matched != 1 {
			t.Errorf("Matched = %d, want 1", result.Matched)
		}
		if result.Mismatched != 0 {
			t.Errorf("Mismatched = %d, want 0", result.Mismatched)
		}
		if result.NoCRC != 0 {
			t.Errorf("NoCRC = %d, want 0", result.NoCRC)
		}
		if result.Unverified != 0 {
			t.Errorf("Unverified = %d, want 0", result.Unverified)
		}
	})

	t.Run("mismatched CRC", func(t *testing.T) {
		t.Parallel()
		// Wrong CRC for data.bin
		files := []AssembledFile{
			{FileName: "data.bin", CRC32: 0xDEADBEEF},
		}
		result := VerifyCRCs(files, sets, slog.Default())
		if result.Checked != 1 {
			t.Errorf("Checked = %d, want 1", result.Checked)
		}
		if result.Matched != 0 {
			t.Errorf("Matched = %d, want 0", result.Matched)
		}
		if result.Mismatched != 1 {
			t.Errorf("Mismatched = %d, want 1", result.Mismatched)
		}
	})

	t.Run("file not in manifest", func(t *testing.T) {
		t.Parallel()
		files := []AssembledFile{
			{FileName: "unknown.mkv", CRC32: 0x12345678},
		}
		result := VerifyCRCs(files, sets, slog.Default())
		if result.Checked != 0 {
			t.Errorf("Checked = %d, want 0 (file not in manifest)", result.Checked)
		}
		// unknown.mkv → NotInPar2, data.bin par2 entry → Unverified
		if result.NotInPar2 != 1 {
			t.Errorf("NotInPar2 = %d, want 1", result.NotInPar2)
		}
		if result.Unverified != 1 {
			t.Errorf("Unverified = %d, want 1 (data.bin par2 entry unconsumed)", result.Unverified)
		}
		if len(result.UnverifiedFiles) != 1 || result.UnverifiedFiles[0] != "data.bin" {
			t.Errorf("UnverifiedFiles = %v, want [data.bin]", result.UnverifiedFiles)
		}
	})

	t.Run("UnverifiedFiles is sorted regardless of manifest order", func(t *testing.T) {
		t.Parallel()

		// par2Index is a map, so its iteration order is randomized per run.
		// Build a manifest with three unconsumed entries (no assembled files
		// at all, so none can be matched by name or CRC+size) and register
		// them in a deliberately non-alphabetical order to catch any
		// regression to raw map-iteration order.
		setID := [16]byte{9, 9, 9}
		names := []string{"zebra.txt", "apple.txt", "mango.txt"}
		pkts := make([][]byte, 0, 2*len(names))
		fileIDs := make([][16]byte, 0, len(names))
		for i, name := range names {
			fileID := [16]byte{byte(i + 1)}
			fileIDs = append(fileIDs, fileID)
			content := []byte("content for " + name)
			hash16k := md5.Sum(content) //nolint:gosec // test fixture only
			pkts = append(pkts,
				buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, hash16k, hash16k, uint64(len(content)), name)),
				buildPacket(setID, typeIFSC, buildIFSCBody(fileID, []ifscSlice{{md5Hash: hash16k, crc32: crc32.ChecksumIEEE(content)}})),
			)
		}
		mainPkt := buildPacket(setID, typeMain, buildMainBody(16, fileIDs...))

		var parContent []byte
		parContent = append(parContent, mainPkt...)
		for _, p := range pkts {
			parContent = append(parContent, p...)
		}

		tmpDir := t.TempDir()
		parPath := filepath.Join(tmpDir, "multi.par2")
		if err := os.WriteFile(parPath, parContent, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		multiSets, err := FindPar2Files(tmpDir)
		if err != nil {
			t.Fatalf("FindPar2Files: %v", err)
		}
		if len(multiSets) == 0 {
			t.Fatal("expected at least one par2 set")
		}

		result := VerifyCRCs(nil, multiSets, slog.Default())

		if result.Unverified != 3 {
			t.Fatalf("Unverified = %d, want 3", result.Unverified)
		}
		want := []string{"apple.txt", "mango.txt", "zebra.txt"}
		if len(result.UnverifiedFiles) != len(want) {
			t.Fatalf("UnverifiedFiles = %v, want %v", result.UnverifiedFiles, want)
		}
		for i, name := range want {
			if result.UnverifiedFiles[i] != name {
				t.Errorf("UnverifiedFiles[%d] = %q, want %q (order must be deterministic)", i, result.UnverifiedFiles[i], name)
			}
		}
	})

	t.Run("par2-tracked file with no CRC", func(t *testing.T) {
		t.Parallel()
		// data.bin is in par2 manifest but has CRC32=0 (simulates failed download)
		files := []AssembledFile{
			{FileName: "data.bin", CRC32: 0},
		}
		result := VerifyCRCs(files, sets, slog.Default())
		if result.Checked != 0 {
			t.Errorf("Checked = %d, want 0", result.Checked)
		}
		// data.bin is par2-tracked but has no CRC → NoCRC=1
		if result.NoCRC != 1 {
			t.Errorf("NoCRC = %d, want 1", result.NoCRC)
		}
		// The par2 entry was consumed (not double-counted as Unverified)
		if result.Unverified != 0 {
			t.Errorf("Unverified = %d, want 0 (par2 entry was consumed)", result.Unverified)
		}
		if len(result.NoCRCFiles) != 1 || result.NoCRCFiles[0] != "data.bin" {
			t.Errorf("NoCRCFiles = %v, want [data.bin]", result.NoCRCFiles)
		}
	})
}

// TestVerifyCRCs_CRCSizeFallback verifies that the CRC+Size fallback pass
// matches obfuscated filenames when the basename doesn't match par2 entries.

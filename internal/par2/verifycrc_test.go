package par2

import (
	"log/slog"
	"os"
	"testing"
)

func TestVerifyCRCs_NoSets(t *testing.T) {
	t.Parallel()

	// Direct references for alignment check
	_ = matchBasename
	_ = matchFlattened
	_ = matchCRCSize

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
func TestVerifyCRCs_CRCSizeFallback(t *testing.T) {
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

	// Get the real FileDesc to know the exact CRC32 and FileSize.
	descs, err := ParseFileDescriptions(par2Path)
	if err != nil {
		t.Fatalf("ParseFileDescriptions: %v", err)
	}
	if len(descs) == 0 {
		t.Skip("no file descriptions in par2 fixture")
	}
	fd := descs[0] // data.bin

	t.Run("obfuscated name matched by CRC+size", func(t *testing.T) {
		t.Parallel()
		// Simulate an obfuscated NZB subject that doesn't match par2's "data.bin".
		files := []AssembledFile{
			{
				FileName: "abcdef1234567890.bin", // obfuscated
				CRC32:    fd.FileCRC32,
				FileSize: int64(fd.FileSize), //nolint:gosec // test
			},
		}
		result := VerifyCRCs(files, sets, slog.Default())

		// The name-based pass finds nothing; the CRC+size fallback should match.
		if result.Matched != 1 {
			t.Errorf("Matched = %d, want 1 (CRC+size fallback should have matched)", result.Matched)
		}
		if result.Checked != 1 {
			t.Errorf("Checked = %d, want 1", result.Checked)
		}
		if result.Unverified != 0 {
			t.Errorf("Unverified = %d, want 0 (par2 entry should be consumed by fallback)", result.Unverified)
		}
		if result.Mismatched != 0 {
			t.Errorf("Mismatched = %d, want 0", result.Mismatched)
		}
	})

	t.Run("obfuscated name wrong CRC", func(t *testing.T) {
		t.Parallel()
		// Same size but wrong CRC — should NOT match.
		files := []AssembledFile{
			{
				FileName: "obfuscated.bin",
				CRC32:    0xDEADBEEF,
				FileSize: int64(fd.FileSize), //nolint:gosec // test
			},
		}
		result := VerifyCRCs(files, sets, slog.Default())

		if result.Matched != 0 {
			t.Errorf("Matched = %d, want 0 (wrong CRC)", result.Matched)
		}
		if result.Unverified != 1 {
			t.Errorf("Unverified = %d, want 1 (par2 entry should remain unmatched)", result.Unverified)
		}
	})

	t.Run("obfuscated name wrong size", func(t *testing.T) {
		t.Parallel()
		// Right CRC but wrong size — should NOT match.
		files := []AssembledFile{
			{
				FileName: "obfuscated.bin",
				CRC32:    fd.FileCRC32,
				FileSize: 99999, // wrong size
			},
		}
		result := VerifyCRCs(files, sets, slog.Default())

		if result.Matched != 0 {
			t.Errorf("Matched = %d, want 0 (wrong size)", result.Matched)
		}
		if result.Unverified != 1 {
			t.Errorf("Unverified = %d, want 1 (par2 entry should remain unmatched)", result.Unverified)
		}
	})

	t.Run("obfuscated name no FileSize on assembled file", func(t *testing.T) {
		t.Parallel()
		// CRC matches but FileSize=0 on assembled file — index key won't match
		// because par2 FileSize > 0.
		files := []AssembledFile{
			{
				FileName: "obfuscated.bin",
				CRC32:    fd.FileCRC32,
				FileSize: 0,
			},
		}
		result := VerifyCRCs(files, sets, slog.Default())

		if result.Matched != 0 {
			t.Errorf("Matched = %d, want 0 (zero FileSize should not match)", result.Matched)
		}
	})

	t.Run("name match takes priority over CRC+size", func(t *testing.T) {
		t.Parallel()
		// "data.bin" matches by name — the CRC+size fallback should NOT be used,
		// and the name-based match's CRC should be checked.
		files := []AssembledFile{
			{
				FileName: "data.bin", // matches par2 by name
				CRC32:    fd.FileCRC32,
				FileSize: int64(fd.FileSize), //nolint:gosec // test
			},
		}
		result := VerifyCRCs(files, sets, slog.Default())

		if result.Matched != 1 {
			t.Errorf("Matched = %d, want 1", result.Matched)
		}
		// Should be exactly 1 check (by name), not 2 (name + fallback).
		if result.Checked != 1 {
			t.Errorf("Checked = %d, want 1 (name match should prevent fallback)", result.Checked)
		}
	})
}

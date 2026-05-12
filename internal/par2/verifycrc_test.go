package par2

import (
	"log/slog"
	"os"
	"testing"
)

func TestVerifyCRCs_AllMatch(t *testing.T) {
	t.Parallel()

	// Create a mock par2 file with known CRC32 values.
	// We'll use ParseFileDescriptions which needs a real par2 file,
	// so we test VerifyCRCs with pre-built sets instead.
	// Since VerifyCRCs calls ParseFileDescriptions internally, we
	// need real par2 files. For unit testing, we'll test the matching
	// logic by constructing scenarios.

	// For now, test with an empty set — all files should be skipped.
	files := []AssembledFile{
		{FileName: "movie.mkv", CRC32: 0x12345678},
		{FileName: "sample.avi", CRC32: 0xDEADBEEF},
	}

	result := VerifyCRCs(files, nil, slog.Default())

	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0 (no par2 sets)", result.Checked)
	}
	if result.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2", result.Skipped)
	}
}

func TestVerifyCRCs_NoCRC(t *testing.T) {
	t.Parallel()

	// Files with CRC32=0 should be skipped.
	files := []AssembledFile{
		{FileName: "movie.mkv", CRC32: 0},
		{FileName: "sample.avi", CRC32: 0},
	}

	result := VerifyCRCs(files, nil, slog.Default())

	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0", result.Checked)
	}
	// All skipped because no par2 sets and no CRCs.
	if result.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2", result.Skipped)
	}
}

func TestVerifyCRCs_EmptyFiles(t *testing.T) {
	t.Parallel()

	result := VerifyCRCs(nil, nil, slog.Default())

	if result.Checked != 0 || result.Matched != 0 || result.Mismatched != 0 || result.Skipped != 0 {
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
		if result.Skipped != 1 {
			t.Errorf("Skipped = %d, want 1", result.Skipped)
		}
	})
}

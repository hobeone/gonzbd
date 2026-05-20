package par2

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// ---------- parseStatus ----------

func TestParseStatus_AllFilesOK(t *testing.T) {
	t.Parallel()
	got := parseStatus("Loaded 8 new packets\nAll files are correct, repair is not required.\n")
	if got != StatusAllFilesOK {
		t.Errorf("got %d, want StatusAllFilesOK", got)
	}
}

func TestParseStatus_RepairRequired(t *testing.T) {
	t.Parallel()
	got := parseStatus("Repair is required\n2 file(s) are missing.\n")
	if got != StatusRepairRequired {
		t.Errorf("got %d, want StatusRepairRequired", got)
	}
}

func TestParseStatus_RepairPossible(t *testing.T) {
	t.Parallel()
	got := parseStatus("Repair is possible. 2 recovery blocks needed.\nRepair is possible using 2 recovery blocks.\n")
	if got != StatusRepairPossible {
		t.Errorf("got %d, want StatusRepairPossible", got)
	}
}

func TestParseStatus_RepairNotPossible(t *testing.T) {
	t.Parallel()
	got := parseStatus("Repair is not possible.\n")
	if got != StatusRepairNotPossible {
		t.Errorf("got %d, want StatusRepairNotPossible", got)
	}
}

func TestParseStatus_InvalidPar2_MainPacket(t *testing.T) {
	t.Parallel()
	got := parseStatus("Main packet not found\n")
	if got != StatusInvalidPar2 {
		t.Errorf("got %d, want StatusInvalidPar2", got)
	}
}

func TestParseStatus_InvalidPar2_RecoveryFile(t *testing.T) {
	t.Parallel()
	got := parseStatus("The recovery file does not exist\n")
	if got != StatusInvalidPar2 {
		t.Errorf("got %d, want StatusInvalidPar2", got)
	}
}

func TestParseStatus_Unknown(t *testing.T) {
	t.Parallel()
	got := parseStatus("some unknown output\n")
	if got != StatusUnknown {
		t.Errorf("got %d, want StatusUnknown", got)
	}
}

func TestParseStatus_Empty(t *testing.T) {
	t.Parallel()
	got := parseStatus("")
	if got != StatusUnknown {
		t.Errorf("got %d, want StatusUnknown", got)
	}
}

// TestParseStatus_FailurePriorityOverSuccess verifies that when par2 output
// contains both "All files are correct" and a failure message, the failure
// takes priority. This can happen when par2 checks multiple files and some
// pass while the overall result is failure.
func TestParseStatus_FailurePriorityOverSuccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   Status
	}{
		{
			name:   "need more blocks + all ok",
			output: "All files are correct\nRepair is not possible.\nYou need 5 more recovery blocks.\n",
			want:   StatusNeedMoreBlocks,
		},
		{
			name:   "repair required + all ok",
			output: "All files are correct\nRepair is required\n2 file(s) are missing.\n",
			want:   StatusRepairRequired,
		},
		{
			name:   "invalid par2 + all ok",
			output: "All files are correct\nMain packet not found\n",
			want:   StatusInvalidPar2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseStatus(tc.output)
			if got != tc.want {
				t.Errorf("parseStatus() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseStatus_NeedMoreBlocks(t *testing.T) {
	t.Parallel()
	got := parseStatus("Repair is not possible.\nYou need 5 more recovery blocks.\n")
	if got != StatusNeedMoreBlocks {
		t.Errorf("got %d, want StatusNeedMoreBlocks", got)
	}
}

func TestParseStatus_DiskFull(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output string
	}{
		{"not enough space", "There is not enough space on the disk\n"},
		{"insufficient", "Insufficient disk space to write repaired file\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseStatus(tc.output)
			if got != StatusDiskFull {
				t.Errorf("parseStatus(%q) = %d, want StatusDiskFull", tc.output, got)
			}
		})
	}
}

func TestParseStatus_CannotRename(t *testing.T) {
	t.Parallel()
	got := parseStatus("\"file.dat.1\" cannot be renamed to \"file.dat\"\n")
	if got != StatusRepairNotPossible {
		t.Errorf("got %d, want StatusRepairNotPossible", got)
	}
}

func TestParseStatus_RepairFailed(t *testing.T) {
	t.Parallel()
	got := parseStatus("Verifying repaired files:\n\nRepair Failed.\n")
	if got != StatusRepairFailed {
		t.Errorf("got %d, want StatusRepairFailed", got)
	}
}

func TestParseStatus_BadOption_InvalidOption(t *testing.T) {
	t.Parallel()
	got := parseStatus("Invalid option specified: -t+\n")
	if got != StatusBadOption {
		t.Errorf("got %d, want StatusBadOption", got)
	}
}

func TestParseStatus_BadOption_InvalidThread(t *testing.T) {
	t.Parallel()
	got := parseStatus("Invalid thread option: -t+\n")
	if got != StatusBadOption {
		t.Errorf("got %d, want StatusBadOption", got)
	}
}

func TestParseStatus_NoDetailsAvailable(t *testing.T) {
	t.Parallel()
	got := parseStatus("No details available for recoverable file: data.bin\n")
	if got != StatusRepairNotPossible {
		t.Errorf("got %d, want StatusRepairNotPossible", got)
	}
}

func TestNeedMoreBlocksRE(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output string
		want   int
	}{
		{"5 blocks", "You need 5 more recovery blocks to be able to repair.\n", 5},
		{"1 block", "You need 1 more recovery block to be able to repair.\n", 1},
		{"25 blocks", "You need 25 more recovery blocks.\n", 25},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := needMoreBlocksRE.FindStringSubmatch(tc.output)
			if len(m) != 2 {
				t.Fatalf("no match for %q", tc.output)
			}
			got, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("atoi: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d blocks, want %d", got, tc.want)
			}
		})
	}
}

// ---------- setName ----------

func TestSetName_MainFile(t *testing.T) {
	t.Parallel()
	got := setName("movie.par2")
	if got != "movie" {
		t.Errorf("setName(%q) = %q, want %q", "movie.par2", got, "movie")
	}
}

func TestSetName_VolumeFile(t *testing.T) {
	t.Parallel()
	got := setName("movie.vol000+01.par2")
	if got != "movie" {
		t.Errorf("setName(%q) = %q, want %q", "movie.vol000+01.par2", got, "movie")
	}
}

func TestSetName_LargeVolume(t *testing.T) {
	t.Parallel()
	got := setName("data.vol015+16.par2")
	if got != "data" {
		t.Errorf("setName(%q) = %q, want %q", "data.vol015+16.par2", got, "data")
	}
}

func TestSetName_NotPar2(t *testing.T) {
	t.Parallel()
	got := setName("movie.rar")
	if got != "movie.rar" {
		t.Errorf("setName(%q) = %q, want %q", "movie.rar", got, "movie.rar")
	}
}

func TestSetName_CaseInsensitive(t *testing.T) {
	t.Parallel()
	got := setName("movie.PAR2")
	if got != "movie" {
		t.Errorf("setName(%q) = %q, want %q", "movie.PAR2", got, "movie")
	}
}

// ---------- isVolume ----------

func TestIsVolume_True(t *testing.T) {
	t.Parallel()
	if !isVolume("movie.vol000+01.par2") {
		t.Error("expected true for volume file")
	}
}

func TestIsVolume_False(t *testing.T) {
	t.Parallel()
	if isVolume("movie.par2") {
		t.Error("expected false for main par2 file")
	}
}

// ---------- FindPar2Files ----------

func TestFindPar2Files_GroupsCorrectly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create a set with main + 2 volumes.
	for _, name := range []string{
		"movie.par2",
		"movie.vol000+01.par2",
		"movie.vol001+02.par2",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("par2"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	sets, err := FindPar2Files(dir)
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("expected 1 set, got %d", len(sets))
	}
	if sets[0].Name != "movie" {
		t.Errorf("Name = %q, want %q", sets[0].Name, "movie")
	}
	if sets[0].MainFile == "" {
		t.Error("MainFile should be set")
	}
	if len(sets[0].ExtraFiles) != 2 {
		t.Errorf("ExtraFiles = %d, want 2", len(sets[0].ExtraFiles))
	}
}

func TestFindPar2Files_MultipleSets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "alpha.par2"), []byte("p"), 0o644)
	os.WriteFile(filepath.Join(dir, "beta.par2"), []byte("p"), 0o644)
	os.WriteFile(filepath.Join(dir, "beta.vol000+01.par2"), []byte("p"), 0o644)

	sets, err := FindPar2Files(dir)
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}
	if len(sets) != 2 {
		t.Fatalf("expected 2 sets, got %d", len(sets))
	}
	// Sorted by name.
	if sets[0].Name != "alpha" || sets[1].Name != "beta" {
		t.Errorf("names = [%q, %q], want [alpha, beta]", sets[0].Name, sets[1].Name)
	}
}

func TestFindPar2Files_NoPar2(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hi"), 0o644)

	sets, err := FindPar2Files(dir)
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}
	if len(sets) != 0 {
		t.Errorf("expected 0 sets, got %d", len(sets))
	}
}

func TestFindPar2Files_NonexistentDir(t *testing.T) {
	t.Parallel()
	_, err := FindPar2Files("/nonexistent/dir")
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

func TestFindPar2Files_SkipsDirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "fake.par2"), 0o755) // dir named .par2

	sets, err := FindPar2Files(dir)
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}
	if len(sets) != 0 {
		t.Errorf("expected 0 sets (directories should be skipped), got %d", len(sets))
	}
}

func TestFindPar2Files_VolumeOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Only volume files, no main .par2 file.
	os.WriteFile(filepath.Join(dir, "data.vol000+01.par2"), []byte("p"), 0o644)
	os.WriteFile(filepath.Join(dir, "data.vol001+02.par2"), []byte("p"), 0o644)

	sets, err := FindPar2Files(dir)
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("expected 1 set, got %d", len(sets))
	}
	if sets[0].MainFile != "" {
		t.Errorf("MainFile = %q, want empty for volume-only set", sets[0].MainFile)
	}
	if len(sets[0].ExtraFiles) != 2 {
		t.Errorf("ExtraFiles = %d, want 2", len(sets[0].ExtraFiles))
	}
}

// L15: Verify FindPar2Files matches .PAR2 and .Par2 extensions (case-insensitive).
func TestFindPar2Files_CaseInsensitive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Mixed case: main file is .PAR2, volume file is .Par2.
	for _, name := range []string{
		"album.PAR2",
		"album.vol000+01.Par2",
		"album.vol001+02.par2",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("par2"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	sets, err := FindPar2Files(dir)
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("expected 1 set for mixed-case .par2 files, got %d", len(sets))
	}
	if sets[0].Name != "album" {
		t.Errorf("Name = %q, want %q", sets[0].Name, "album")
	}
	// Should have the main .PAR2 file and 2 volume files.
	if sets[0].MainFile == "" {
		t.Error("MainFile should be set for .PAR2 main file")
	}
	if len(sets[0].ExtraFiles) != 2 {
		t.Errorf("ExtraFiles = %d, want 2", len(sets[0].ExtraFiles))
	}
}

// L15 follow-up: Ensure all-uppercase .PAR2 extension is recognized by setName.
func TestSetName_UppercasePAR2(t *testing.T) {
	t.Parallel()
	got := setName("data.PAR2")
	if got != "data" {
		t.Errorf("setName(%q) = %q, want %q", "data.PAR2", got, "data")
	}
}

func TestSetName_MixedCaseVolume(t *testing.T) {
	t.Parallel()
	got := setName("data.Vol015+16.Par2")
	if got != "data" {
		t.Errorf("setName(%q) = %q, want %q", "data.Vol015+16.Par2", got, "data")
	}
}

func TestTeeHandler_FormatForUI(t *testing.T) {
	t.Parallel()

	h := &teeHandler{}

	tests := []struct {
		name    string
		level   slog.Level
		message string
		attrs   []slog.Attr
		wantOk  bool
	}{
		{
			name:    "info level logs are allowed",
			level:   slog.LevelInfo,
			message: "Some info log",
			wantOk:  true,
		},
		{
			name:    "warn level logs are allowed",
			level:   slog.LevelWarn,
			message: "Some warn log",
			wantOk:  true,
		},
		{
			name:    "error level logs are allowed",
			level:   slog.LevelError,
			message: "Some error log",
			wantOk:  true,
		},
		{
			name:    "debug level logs are filtered out",
			level:   slog.LevelDebug,
			message: "Some debug log",
			wantOk:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := slog.NewRecord(time.Now(), tc.level, tc.message, 0)
			r.AddAttrs(tc.attrs...)
			_, ok := h.formatForUI(r)
			if ok != tc.wantOk {
				t.Errorf("formatForUI() ok = %t, want %t", ok, tc.wantOk)
			}
		})
	}
}

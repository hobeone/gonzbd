package par2

import (
	"os"
	"path/filepath"
	"testing"
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
	got := parseStatus("Repair is not possible.\nYou need 5 more recovery blocks.\n")
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

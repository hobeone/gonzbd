package postproc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

func TestVerifiedSets_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	vs := NewVerifiedSets(dir)
	vs.MarkVerified("movie", true)
	vs.MarkVerified("subs", false)

	if !vs.IsVerified("movie") {
		t.Error("movie should be verified")
	}
	if vs.IsVerified("subs") {
		t.Error("subs should NOT be verified (marked false)")
	}
	if vs.IsVerified("unknown") {
		t.Error("unknown set should not be verified")
	}

	// Reload from disk.
	vs2 := NewVerifiedSets(dir)
	if !vs2.IsVerified("movie") {
		t.Error("after reload: movie should be verified")
	}
	if vs2.IsVerified("subs") {
		t.Error("after reload: subs should NOT be verified")
	}
}

func TestVerifiedSets_AllVerified(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Empty → false.
	vs := NewVerifiedSets(dir)
	if vs.AllVerified() {
		t.Error("empty set should not be AllVerified")
	}

	// Partial → false.
	vs.MarkVerified("a", true)
	vs.MarkVerified("b", false)
	if vs.AllVerified() {
		t.Error("partial set should not be AllVerified")
	}

	// All true → true.
	vs.MarkVerified("b", true)
	if !vs.AllVerified() {
		t.Error("all-true set should be AllVerified")
	}
}

func TestVerifiedSets_Persistence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	vs := NewVerifiedSets(dir)
	vs.MarkVerified("set1", true)
	vs.MarkVerified("set2", true)

	// Create a new instance pointing to the same directory.
	vs2 := NewVerifiedSets(dir)
	if !vs2.IsVerified("set1") {
		t.Error("set1 should be verified after reload")
	}
	if !vs2.IsVerified("set2") {
		t.Error("set2 should be verified after reload")
	}
	if !vs2.AllVerified() {
		t.Error("all sets should be verified after reload")
	}
}

func TestVerifiedSets_FileLocation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	vs := NewVerifiedSets(dir)
	vs.MarkVerified("test", true)

	// Verify the file is in the expected location.
	expectedPath := filepath.Join(dir, constants.JobAdminDirName, constants.VerifiedFileName)
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("verified file not found at expected path %s: %v", expectedPath, err)
	}
}

func TestVerifiedSets_CorruptFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write garbage to the verified file location.
	adminDir := filepath.Join(dir, constants.JobAdminDirName)
	if err := os.MkdirAll(adminDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(adminDir, constants.VerifiedFileName)
	if err := os.WriteFile(path, []byte("not json"), 0o640); err != nil {
		t.Fatal(err)
	}

	// Should recover gracefully with empty state.
	vs := NewVerifiedSets(dir)
	if vs.AllVerified() {
		t.Error("corrupt file should result in empty (not AllVerified) state")
	}
	if vs.IsVerified("anything") {
		t.Error("corrupt file should result in no verified sets")
	}
}

func TestVerifiedSets_LoadSave_Direct(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	vs := NewVerifiedSets(dir)
	vs.sets["direct_test"] = true

	if err := vs.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	vs2 := &VerifiedSets{
		sets: make(map[string]bool),
		path: vs.path,
	}
	vs2.load()
	if !vs2.sets["direct_test"] {
		t.Error("expected load to populate direct_test")
	}
}

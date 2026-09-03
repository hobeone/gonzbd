package postproc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExtensionCleanup_RemovesMatchingFiles(t *testing.T) {
	dir := t.TempDir()

	// Create files with various extensions.
	for _, name := range []string{
		"movie.mkv",
		"info.nfo",
		"readme.txt",
		"subs.srt",
		"release.sfv",
		"release.srr",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	stage := NewExtensionCleanupStage([]string{"nfo", "txt", "sfv", "srr"})
	job := &Job{Job: newQueueJob(t, "test", 0), DownloadDir: dir}

	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	// movie.mkv and subs.srt should survive.
	for _, want := range []string{"movie.mkv", "subs.srt"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("expected %s to survive, got %v", want, err)
		}
	}
	// The cleanup targets should be gone.
	for _, gone := range []string{"info.nfo", "readme.txt", "release.sfv", "release.srr"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted", gone)
		}
	}
}

func TestExtensionCleanup_CaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "info.NFO"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "readme.TXT"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("x"), 0o644)

	stage := NewExtensionCleanupStage([]string{"nfo", "txt"})
	job := &Job{Job: newQueueJob(t, "test", 0), DownloadDir: dir}

	stage.Run(context.Background(), job)

	if _, err := os.Stat(filepath.Join(dir, "movie.mkv")); err != nil {
		t.Error("movie.mkv should survive")
	}
	if _, err := os.Stat(filepath.Join(dir, "info.NFO")); !os.IsNotExist(err) {
		t.Error("info.NFO should be deleted (case insensitive)")
	}
	if _, err := os.Stat(filepath.Join(dir, "readme.TXT")); !os.IsNotExist(err) {
		t.Error("readme.TXT should be deleted (case insensitive)")
	}
}

func TestExtensionCleanup_RecursiveWalk(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "subs")
	os.MkdirAll(subdir, 0o755)

	os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "info.nfo"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(subdir, "notes.nfo"), []byte("x"), 0o644)

	stage := NewExtensionCleanupStage([]string{"nfo"})
	job := &Job{Job: newQueueJob(t, "test", 0), DownloadDir: dir}

	stage.Run(context.Background(), job)

	// Both NFOs should be gone.
	if _, err := os.Stat(filepath.Join(dir, "info.nfo")); !os.IsNotExist(err) {
		t.Error("root info.nfo should be deleted")
	}
	if _, err := os.Stat(filepath.Join(subdir, "notes.nfo")); !os.IsNotExist(err) {
		t.Error("subdir notes.nfo should be deleted")
	}
	// The now-empty subdir should also be removed.
	if _, err := os.Stat(subdir); !os.IsNotExist(err) {
		t.Error("empty subdir should be cleaned up")
	}
	// movie.mkv stays.
	if _, err := os.Stat(filepath.Join(dir, "movie.mkv")); err != nil {
		t.Error("movie.mkv should survive")
	}
}

func TestExtensionCleanup_SkipNZB(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "extra.nzb"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "info.nfo"), []byte("x"), 0o644)

	// NZB is in the cleanup list, but SkipNZB is true (default).
	stage := NewExtensionCleanupStage([]string{"nzb", "nfo"})
	job := &Job{Job: newQueueJob(t, "test", 0), DownloadDir: dir}

	stage.Run(context.Background(), job)

	// NZB should survive.
	if _, err := os.Stat(filepath.Join(dir, "extra.nzb")); err != nil {
		t.Error("extra.nzb should survive (SkipNZB=true)")
	}
	// NFO should be gone.
	if _, err := os.Stat(filepath.Join(dir, "info.nfo")); !os.IsNotExist(err) {
		t.Error("info.nfo should be deleted")
	}
}

func TestExtensionCleanup_EmptyList(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "info.nfo"), []byte("x"), 0o644)

	stage := NewExtensionCleanupStage([]string{})
	job := &Job{Job: newQueueJob(t, "test", 0), DownloadDir: dir}

	stage.Run(context.Background(), job)

	// Nothing should be deleted.
	if _, err := os.Stat(filepath.Join(dir, "info.nfo")); err != nil {
		t.Error("info.nfo should survive with empty cleanup list")
	}
}

func TestExtensionCleanup_NormalizesInput(t *testing.T) {
	// Test that dots and spaces are stripped from the input.
	stage := NewExtensionCleanupStage([]string{".NFO", " txt ", "..sfv"})
	want := map[string]bool{"nfo": true, "txt": true, "sfv": true}
	if len(stage.Extensions) != len(want) {
		t.Fatalf("Extensions = %v, want 3 entries", stage.Extensions)
	}
	for _, ext := range stage.Extensions {
		if !want[ext] {
			t.Errorf("unexpected extension %q", ext)
		}
	}
}

func TestExtensionCleanup_Name(t *testing.T) {
	stage := NewExtensionCleanupStage(nil)
	if stage.Name() != "extension_cleanup" {
		t.Errorf("Name() = %q, want %q", stage.Name(), "extension_cleanup")
	}
}

func TestCleanupEmptyDirs(t *testing.T) {
	root := t.TempDir()

	emptyDir := filepath.Join(root, "empty_dir")
	nonEmptyDir := filepath.Join(root, "non_empty_dir")
	nestedEmpty := filepath.Join(root, "nested_empty")
	nestedEmptySub := filepath.Join(nestedEmpty, "subdir")

	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nonEmptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nestedEmptySub, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(nonEmptyDir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rootH, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rootH.Close()

	cleanupEmptyDirs(rootH)

	if _, err := os.Stat(emptyDir); !os.IsNotExist(err) {
		t.Errorf("expected empty_dir to be removed")
	}
	if _, err := os.Stat(nestedEmptySub); !os.IsNotExist(err) {
		t.Errorf("expected nested_empty/subdir to be removed")
	}
	if _, err := os.Stat(nestedEmpty); !os.IsNotExist(err) {
		t.Errorf("expected nested_empty to be removed")
	}
	if _, err := os.Stat(nonEmptyDir); err != nil {
		t.Errorf("expected non_empty_dir to survive")
	}
	if _, err := os.Stat(filepath.Join(nonEmptyDir, "file.txt")); err != nil {
		t.Errorf("expected non_empty_dir/file.txt to survive")
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("expected root dir to survive")
	}
}

// TestExtensionCleanup_RestrictsToOwnedFiles simulates the upstream SABnzbd
// bug (#3462, commit 5b3cf86f6): cleanup_list() used to glob-delete any file
// in the working directory matching a cleanup extension, regardless of
// whether it belonged to the current job. A stray file matching a cleanup
// extension but absent from job.OwnedFiles must survive, while an owned
// file with the same extension is still removed.
func TestExtensionCleanup_RestrictsToOwnedFiles(t *testing.T) {
	dir := t.TempDir()

	ownedPath := filepath.Join(dir, "job.nfo")
	strayPath := filepath.Join(dir, "unrelated.nfo")
	if err := os.WriteFile(ownedPath, []byte("owned"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strayPath, []byte("stray"), 0o644); err != nil {
		t.Fatal(err)
	}

	stage := NewExtensionCleanupStage([]string{"nfo"})
	job := &Job{
		Job:         newQueueJob(t, "test", 0),
		DownloadDir: dir,
		OwnedFiles: map[string]struct{}{
			ownedPath: {},
		},
	}

	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(ownedPath); !os.IsNotExist(err) {
		t.Errorf("expected owned file %s to be deleted", ownedPath)
	}
	if _, err := os.Stat(strayPath); err != nil {
		t.Errorf("expected unrelated file %s to survive, got %v", strayPath, err)
	}
}

func TestExtensionCleanup_ZipSlip_Symlink(t *testing.T) {
	dlDir := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("VICTIM"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Plant symlink inside dlDir pointing to outside directory
	if err := os.Symlink(outside, filepath.Join(dlDir, "link")); err != nil {
		t.Fatal(err)
	}

	// Create a job representing the run
	job := &Job{
		DownloadDir:   dlDir,
		ConsumedFiles: make(map[string]struct{}),
	}

	stage := NewExtensionCleanupStage([]string{"txt"})
	// Run stage. It should walk dlDir but root.Remove("link/victim.txt")
	// should fail/refuse to follow the symlink because of os.Root.
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("stage run returned error: %v", err)
	}

	// Victim must survive
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("failed to read victim: %v", err)
	}
	if string(data) != "VICTIM" {
		t.Errorf("victim content modified: %s", string(data))
	}
}

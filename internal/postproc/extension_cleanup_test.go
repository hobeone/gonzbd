package postproc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
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
	job := &Job{Queue: &queue.Job{ID: "test"}, DownloadDir: dir}

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
	job := &Job{Queue: &queue.Job{ID: "test"}, DownloadDir: dir}

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
	job := &Job{Queue: &queue.Job{ID: "test"}, DownloadDir: dir}

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
	job := &Job{Queue: &queue.Job{ID: "test"}, DownloadDir: dir}

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
	job := &Job{Queue: &queue.Job{ID: "test"}, DownloadDir: dir}

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

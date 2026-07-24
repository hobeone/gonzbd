package postproc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsSample(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		// SABnzbd-compatible matches.
		{"movie-sample.mkv", true},
		{"Movie.Sample.mkv", true},
		{"sample.mp4", true},
		{"SAMPLE.MP4", true},
		{"show.S01E01.proof.avi", true},
		{"release_sample.iso", true},
		{"release.proof.iso", true},
		{"narcos-sample-tfx-25-04-26.mp4", true},

		// Non-matches: word "sample"/"proof" only matches at the start
		// or after a non-word boundary, so substrings inside other
		// words don't count.
		{"examplefile.mp4", false}, // "ample" embedded in "example"
		{"approof.txt", false},     // "proof" embedded in "approof"
		{"main.mkv", false},
		{"Transfixed.1080p.MP4", false},
		{"par2recovery.par2", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSample(tt.name); got != tt.want {
				t.Errorf("IsSample(%q) = %v; want %v", tt.name, got, tt.want)
			}
		})
	}
}

// makeTestJob builds a postproc.Job pointing at a fresh temp directory.
func makeTestJob(t *testing.T, name string) *Job {
	t.Helper()
	dir := t.TempDir()
	qjob := newQueueJob(t, "job-"+name, 0)
	qjob.Name = name
	return &Job{
		Queue:       qjob,
		DownloadDir: dir,
	}
}

// touchFile creates an empty file at path, creating parent dirs.
func touchFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// listFiles returns the basenames of all regular files under dir.
func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, d.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	return out
}

func TestSampleCleanup_DeletesMatchingFiles(t *testing.T) {
	t.Parallel()
	job := makeTestJob(t, "release")

	touchFile(t, filepath.Join(job.DownloadDir, "release.mkv"))
	touchFile(t, filepath.Join(job.DownloadDir, "release-sample.mkv"))
	touchFile(t, filepath.Join(job.DownloadDir, "release.proof.avi"))
	touchFile(t, filepath.Join(job.DownloadDir, "subtitles", "release.srt"))

	stage := NewSampleCleanupStage()
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := listFiles(t, job.DownloadDir)
	want := map[string]bool{"release.mkv": true, "release.srt": true}
	for _, f := range got {
		if !want[f] {
			t.Errorf("unexpected surviving file %q", f)
		}
		delete(want, f)
	}
	for f := range want {
		t.Errorf("expected %q to survive but it did not", f)
	}
}

// TestSampleCleanup_FalsePositiveGuard mirrors SABnzbd's behaviour:
// when every file in the workdir matches the sample pattern, refuse to
// delete anything (the whole release was probably distributed as a
// "sample" archive on purpose).
func TestSampleCleanup_FalsePositiveGuard(t *testing.T) {
	t.Parallel()
	job := makeTestJob(t, "all-samples")

	touchFile(t, filepath.Join(job.DownloadDir, "sample.mkv"))
	touchFile(t, filepath.Join(job.DownloadDir, "another-sample.mp4"))
	touchFile(t, filepath.Join(job.DownloadDir, "release.proof.avi"))

	stage := NewSampleCleanupStage()
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := len(listFiles(t, job.DownloadDir)); got != 3 {
		t.Errorf("file count = %d; want 3 (false-positive guard should keep everything)", got)
	}
}

func TestSampleCleanup_NoMatchesIsNoop(t *testing.T) {
	t.Parallel()
	job := makeTestJob(t, "no-samples")

	touchFile(t, filepath.Join(job.DownloadDir, "release.mkv"))
	touchFile(t, filepath.Join(job.DownloadDir, "subs.srt"))

	stage := NewSampleCleanupStage()
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := len(listFiles(t, job.DownloadDir)); got != 2 {
		t.Errorf("file count = %d; want 2 (nothing matched)", got)
	}
}

// TestSampleCleanup_UserScenario is the bug-report fixture: an NZB
// landed with one obvious sample plus the main feature. Expect the
// sample gone, the rest intact.
func TestSampleCleanup_UserScenario(t *testing.T) {
	t.Parallel()
	job := makeTestJob(t, "transfixed")

	files := []string{
		"narcos-sample-tfx-25-04-26-ariel-demure-getting-his-attention-1080p.mp4",
		"Transfixed.25.04.26.Ariel.Demure.Getting.His.Attention.XXX.1080p.1.MP4",
		"Transfixed.25.04.26.Ariel.Demure.Getting.His.Attention.XXX.1080p.2.MP4",
		"Transfixed.25.04.26.Ariel.Demure.Getting.His.Attention.XXX.1080p.MP4",
	}
	for _, f := range files {
		touchFile(t, filepath.Join(job.DownloadDir, f))
	}

	stage := NewSampleCleanupStage()
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := listFiles(t, job.DownloadDir)
	for _, f := range got {
		if f == files[0] {
			t.Errorf("sample %q was not deleted", f)
		}
	}
	if len(got) != 3 {
		t.Errorf("file count = %d; want 3 (sample deleted, three Transfixed files retained)", len(got))
	}
}

// TestSampleCleanup_MissingDirIsTolerated guards against the stage
// failing when DownloadDir doesn't exist (e.g. a previous stage moved
// it). Best-effort cleanup; never fail the pipeline here.
func TestSampleCleanup_MissingDirIsTolerated(t *testing.T) {
	t.Parallel()
	job := makeTestJob(t, "missing")
	if err := os.RemoveAll(job.DownloadDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	stage := NewSampleCleanupStage()
	if err := stage.Run(context.Background(), job); err != nil {
		t.Errorf("Run returned error for missing dir: %v (should be tolerated)", err)
	}
}

func TestSampleCleanup_RemoveFailure(t *testing.T) {
	t.Parallel()
	job := makeTestJob(t, "remove-failure")

	touchFile(t, filepath.Join(job.DownloadDir, "release.mkv"))
	sampleFile := filepath.Join(job.DownloadDir, "release-sample.mkv")
	touchFile(t, sampleFile)

	// Make download directory non-writable, so we can't remove files from it.
	if err := os.Chmod(job.DownloadDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Chmod(job.DownloadDir, 0o755)
	}()

	stage := NewSampleCleanupStage()
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify that we recorded the removal failure.
	foundFailureMsg := false
	for _, line := range job.OutputLines {
		if strings.Contains(line, "remove ") && strings.Contains(line, "release-sample.mkv") {
			foundFailureMsg = true
			break
		}
	}
	if !foundFailureMsg {
		t.Errorf("expected failure message in OutputLines, got: %v", job.OutputLines)
	}
}

// TestSampleCleanup_RestrictsToOwnedFiles simulates the upstream SABnzbd
// bug (#3462, commit 5b3cf86f6): sample/proof cleanup used to delete any
// matching file in the working directory regardless of job ownership. A
// stray sample file absent from job.OwnedFiles must survive, while an
// owned sample file is still removed.
func TestSampleCleanup_RestrictsToOwnedFiles(t *testing.T) {
	t.Parallel()
	job := makeTestJob(t, "owned-restriction")

	ownedPath := filepath.Join(job.DownloadDir, "release-sample.mkv")
	strayPath := filepath.Join(job.DownloadDir, "other-sample.mkv")
	touchFile(t, filepath.Join(job.DownloadDir, "release.mkv"))
	touchFile(t, ownedPath)
	touchFile(t, strayPath)

	job.OwnedFiles = map[string]struct{}{
		filepath.Join(job.DownloadDir, "release.mkv"): {},
		ownedPath: {},
	}

	stage := NewSampleCleanupStage()
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(ownedPath); !os.IsNotExist(err) {
		t.Errorf("expected owned sample %s to be deleted", ownedPath)
	}
	if _, err := os.Stat(strayPath); err != nil {
		t.Errorf("expected unrelated file %s to survive, got %v", strayPath, err)
	}
}

func TestSampleCleanup_ZipSlip_Symlink(t *testing.T) {
	dlDir := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim-sample.txt")
	if err := os.WriteFile(victim, []byte("VICTIM"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Plant symlink inside dlDir pointing to outside directory
	if err := os.Symlink(outside, filepath.Join(dlDir, "link")); err != nil {
		t.Fatal(err)
	}

	// Create a job representing the run. Place another dummy file to avoid false-positive guard.
	if err := os.WriteFile(filepath.Join(dlDir, "movie.mkv"), []byte("MOVIE"), 0o644); err != nil {
		t.Fatal(err)
	}

	job := &Job{
		DownloadDir:   dlDir,
		ConsumedFiles: make(map[string]struct{}),
	}

	stage := NewSampleCleanupStage()
	stage.SetEnabled(true)
	// Run stage. It should walk dlDir but root.Remove("link/victim-sample.txt")
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

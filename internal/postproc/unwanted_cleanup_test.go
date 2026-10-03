package postproc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/unwanted"
)

// unwantedFixture lays out a job directory: a payload, an executable at the
// top level, a script in a subdirectory that holds nothing else, and a file
// no rule names.
func unwantedFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range []string{"movie.mkv", "Setup.EXE", "extras/run.bat", "notes.nfo"} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func rulesOf(t *testing.T, action unwanted.Action) func() (unwanted.Rules, error) {
	t.Helper()
	r, err := unwanted.NewRules(action, unwanted.ModeBlacklist, []string{"exe", "bat"})
	if err != nil {
		t.Fatal(err)
	}
	return func() (unwanted.Rules, error) { return r, nil }
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func TestUnwantedCleanup_RemovesUnwantedFiles(t *testing.T) {
	dir := unwantedFixture(t)
	job := &Job{Job: newQueueJob(t, "test", 0), DownloadDir: dir}
	if err := NewUnwantedCleanupStage(rulesOf(t, unwanted.ActionPause)).Run(context.Background(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, gone := range []string{"Setup.EXE", "extras/run.bat"} {
		if exists(t, filepath.Join(dir, gone)) {
			t.Errorf("%s survived", gone)
		}
	}
	for _, kept := range []string{"movie.mkv", "notes.nfo"} {
		if !exists(t, filepath.Join(dir, kept)) {
			t.Errorf("%s was removed", kept)
		}
	}
	if exists(t, filepath.Join(dir, "extras")) {
		t.Error("the directory the removal emptied was left behind")
	}
	if !slices.ContainsFunc(job.OutputLines, func(l string) bool {
		return l == "Removed 2 files with unwanted extensions"
	}) {
		t.Errorf("no summary line in the stage log: %q", job.OutputLines)
	}
}

// TestUnwantedCleanup_RemovesNothing covers each reason the stage leaves the
// job's files alone.
func TestUnwantedCleanup_RemovesNothing(t *testing.T) {
	cases := []struct {
		name   string
		action unwanted.Action
		mut    func(*Job)
	}{
		{"approved job", unwanted.ActionPause, func(j *Job) { j.Unwanted = unwanted.StateApproved }},
		{"action off", unwanted.ActionOff, func(*Job) {}},
		{"failed job", unwanted.ActionFail, func(j *Job) { j.UnpackError = true }},
		{"job with a fail message", unwanted.ActionFail, func(j *Job) { j.FailMsg = "beyond repair" }},
		{"not the job's own", unwanted.ActionPause, func(j *Job) {
			j.OwnedFiles = map[string]struct{}{filepath.Join(j.DownloadDir, "movie.mkv"): {}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := unwantedFixture(t)
			job := &Job{Job: newQueueJob(t, "test", 0), DownloadDir: dir}
			c.mut(job)
			if err := NewUnwantedCleanupStage(rulesOf(t, c.action)).Run(context.Background(), job); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !exists(t, filepath.Join(dir, "Setup.EXE")) || !exists(t, filepath.Join(dir, "extras/run.bat")) {
				t.Error("an unwanted file was removed")
			}
		})
	}
}

// TestUnwantedCleanup_BlockedJobIsCleaned pins that only approval spares a
// job: one the check blocked and the user never approved is cleaned like any
// other. (Such a job reaches post-processing only by failing, which the stage
// skips for its own reason, so this is the stage's contract rather than a
// production path.)
func TestUnwantedCleanup_BlockedJobIsCleaned(t *testing.T) {
	dir := unwantedFixture(t)
	job := &Job{Job: newQueueJob(t, "test", 0), DownloadDir: dir, Unwanted: unwanted.StateBlocked}
	if err := NewUnwantedCleanupStage(rulesOf(t, unwanted.ActionPause)).Run(context.Background(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exists(t, filepath.Join(dir, "Setup.EXE")) {
		t.Error("Setup.EXE survived")
	}
}

// TestUnwantedCleanup_FailsClosed pins that rules which cannot be read fail
// the job, so finalize does not deliver files nothing checked.
func TestUnwantedCleanup_FailsClosed(t *testing.T) {
	dir := unwantedFixture(t)
	job := &Job{Job: newQueueJob(t, "test", 0), DownloadDir: dir}
	boom := errors.New("boom")
	err := NewUnwantedCleanupStage(func() (unwanted.Rules, error) { return unwanted.Rules{}, boom }).Run(context.Background(), job)
	if !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want the rules error", err)
	}
	if !strings.Contains(job.FailMsg, "unwanted-extension check could not run") {
		t.Errorf("FailMsg = %q, want the job failed", job.FailMsg)
	}
}

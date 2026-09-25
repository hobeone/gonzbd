package app

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// twoFileJob builds a job with two files of one and two articles, so a test can
// tell an article count derived from the manifest's FileRange apart from a file
// index or a file count.
func twoFileJob(t *testing.T) *job.Job {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "one.bin", Bytes: 10, Articles: []nzb.Article{
			{ID: "a1@t", Bytes: 10, Number: 1},
		}},
		{Subject: "two.bin", Bytes: 20, Articles: []nzb.Article{
			{ID: "b1@t", Bytes: 10, Number: 1},
			{ID: "b2@t", Bytes: 10, Number: 2},
		}},
	}}
	j, _, err := BuildIngestJob(cfg, parsed, "retained.nzb",
		types.FetchOptions{NzbName: "retained"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	return j
}

// TestRetainedProgressFor_RendersOneRowPerManifestFile pins what a retry
// actually resumes from: the article count comes from the manifest's FileRange,
// and the rest from the job's progress.
func TestRetainedProgressFor_RendersOneRowPerManifestFile(t *testing.T) {
	t.Parallel()
	j := twoFileJob(t)
	if err := j.SetFileFilename(0, "one.bin"); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	if err := j.MarkFileComplete(0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}

	got := retainedProgressFor(j, t.TempDir(), slog.New(slog.DiscardHandler))

	if len(got) != 2 {
		t.Fatalf("got %d rows, want one per manifest file (2)", len(got))
	}
	if got[0].FileIndex != 0 || got[1].FileIndex != 1 {
		t.Errorf("file indexes = %d,%d, want 0,1", got[0].FileIndex, got[1].FileIndex)
	}
	if got[0].ArticleCount != 1 || got[1].ArticleCount != 2 {
		t.Errorf("article counts = %d,%d, want 1,2 — these come from the manifest's "+
			"FileRange and are what retainedMatchesManifest checks a retry against",
			got[0].ArticleCount, got[1].ArticleCount)
	}
	if !got[0].Complete {
		t.Error("file 0 is marked complete on the job but not in its retained progress; a " +
			"retry would re-fetch a file that is already on disk")
	}
	if got[1].Complete {
		t.Error("file 1 is not complete on the job but its retained progress says it is")
	}
	if got[0].Filename != "one.bin" {
		t.Errorf("filename = %q, want %q", got[0].Filename, "one.bin")
	}
}

// TestRetainedProgressFor_ReadsAnEvictedManifestFromDisk covers the case the
// finalizer actually meets: by the time a failed job finalizes its content tier
// has usually been evicted, so the article counts have to come off disk.
func TestRetainedProgressFor_ReadsAnEvictedManifestFromDisk(t *testing.T) {
	t.Parallel()
	j := twoFileJob(t)
	mdir := t.TempDir()
	writeManifestFixture(t, mdir, j)

	j.Evict()
	if _, err := j.Manifest(); err == nil {
		t.Fatal("the job is still resident, so this test would not exercise the disk read")
	}

	got := retainedProgressFor(j, mdir, slog.New(slog.DiscardHandler))

	if len(got) != 2 {
		t.Fatalf("got %d rows for an evicted job, want 2 from the manifest on disk", len(got))
	}
	if got[1].ArticleCount != 2 {
		t.Errorf("file 1 article count = %d, want 2", got[1].ArticleCount)
	}
}

// TestRetainedProgressFor_ReportsNothingWithNoManifestAnywhere pins the refusal.
// Rows with the wrong article count are worse than none: retainedMatchesManifest
// rejects the whole overlay, so a guessed count silently costs the retry every
// file's progress rather than one file's.
func TestRetainedProgressFor_ReportsNothingWithNoManifestAnywhere(t *testing.T) {
	t.Parallel()
	j := twoFileJob(t)
	j.Evict()

	got := retainedProgressFor(j, t.TempDir(), slog.New(slog.DiscardHandler))

	if got != nil {
		t.Errorf("got %d rows with no manifest in memory or on disk, want none", len(got))
	}
}

// writeManifestFixture puts j's manifest where openManifestIn will find it.
func writeManifestFixture(t *testing.T, mdir string, j *job.Job) {
	t.Helper()
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal manifest: %v", err)
	}
	if err := os.MkdirAll(mdir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := fsutil.WriteGzAtomicBytes(filepath.Join(mdir, j.ID()+".json.gz"), data); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

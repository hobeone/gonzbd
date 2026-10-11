package app

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/durability"
)

// TestRenameJob_RefusedAfterAFailedFirstArticle pins that a job whose only
// resolved article is a permanent failure cannot be renamed. The failure path
// registers the file and the assembler creates it under DownloadDir/<name>, so
// a rename at that point would split the job across two directories (#825).
func TestRenameJob_RefusedAfterAFailedFirstArticle(t *testing.T) {
	t.Parallel()
	a := newTestApplication(t)
	disp, j := helperJob(t, a, "old", 2, 2)
	p := helperPipeline(t, disp)
	p.downloadDir = a.config.Snapshot().General.DownloadDir
	p.assembler = assembler.New(assembler.Options{FileInfo: p.resolveFileInfo}, nil)
	if err := p.assembler.Start(t.Context()); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}

	p.handleResult(t.Context(), &downloader.ArticleResult{
		Job:       j,
		FileIdx:   0,
		ArtIdx:    0,
		MessageID: "old-f0-a0@x",
		Err:       downloader.ErrNoServersLeft,
		Subject:   "file0.rar",
	})
	// Stop drains the queued request, so the file exists once it returns.
	if err := p.assembler.Stop(); err != nil {
		t.Fatalf("assembler.Stop: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(p.downloadDir, "old", "file0.rar")); err != nil {
		t.Fatalf("the failed article did not create its file under the old name: %v", err)
	}

	if got, err := a.RenameJob(j.ID(), "new"); !errors.Is(err, dispatch.ErrJobStarted) {
		t.Fatalf("RenameJob = (%q, %v), want dispatch.ErrJobStarted", got, err)
	}
	if got := nameOf(t, a, j.ID()); got != "old" {
		t.Errorf("name after a refused rename = %q, want %q", got, "old")
	}
}

// TestInstallVerification_FilenameWithoutRowsMarksBegun pins the restart side
// of #825: a file whose only resolved articles failed has a recorded filename
// and no written rows. Verification leaves it alone and the install restores
// the filename, so the hydrated job still reports DownloadBegun and
// Dispatcher.SetName, which hydrates first, still refuses the rename.
func TestInstallVerification_FilenameWithoutRowsMarksBegun(t *testing.T) {
	t.Parallel()
	a := newTestApplication(t)
	_, j := helperJob(t, a, "restored", 2, 2)
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if j.DownloadBegun() {
		t.Fatal("a job with no recorded filename reports DownloadBegun")
	}
	files := []durability.FileRow{{FileIndex: 0, Filename: "file0.rar"}, {FileIndex: 1}}
	dir := t.TempDir()
	res, err := verifyJobFiles(t.Context(), m, files, nil,
		func(name string) jobFile { return jobFile{Dir: dir, Name: name} }, false)
	if err != nil {
		t.Fatalf("verifyJobFiles: %v", err)
	}
	installVerification(j, files, nil, res, true, slog.New(slog.DiscardHandler), nil)
	if !j.DownloadBegun() {
		t.Error("a restored file with a recorded filename and no rows does not report DownloadBegun")
	}
}

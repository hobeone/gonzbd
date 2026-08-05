package app

import (
	"bytes"
	"compress/gzip"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// readGzFile decompresses the gzip file at path, failing the test on error.
func readGzFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip.NewReader %s: %v", path, err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(gr); err != nil {
		t.Fatalf("gzip read %s: %v", path, err)
	}
	if err := gr.Close(); err != nil {
		t.Fatalf("gzip close %s: %v", path, err)
	}
	return buf.Bytes()
}

// ---------- waitBounded ----------

// TestWaitBounded_ReturnsWaitError pins the fast path: when wait returns
// before the budget elapses, waitBounded returns exactly wait's error
// (including nil) without waiting for the timer.
func TestWaitBounded_ReturnsWaitError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom")
	start := time.Now()
	err := waitBounded("step", time.Second, func() error { return wantErr }, slog.New(slog.DiscardHandler))
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Errorf("waitBounded took %v, want it to return as soon as wait did, well under the 1s budget", elapsed)
	}
}

// TestWaitBounded_ReturnsNilOnSuccess pins that a nil wait error passes
// through unchanged rather than being swallowed or converted.
func TestWaitBounded_ReturnsNilOnSuccess(t *testing.T) {
	t.Parallel()
	err := waitBounded("step", time.Second, func() error { return nil }, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

// TestWaitBounded_TimesOutWhenWaitBlocks pins the budget boundary: a wait
// that never returns must not hang waitBounded forever — it gets a step
// timeout error once d elapses, and the goroutine running wait is abandoned
// rather than joined.
func TestWaitBounded_TimesOutWhenWaitBlocks(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	defer close(block) // let the abandoned goroutine exit so it doesn't leak past the test

	const budget = 20 * time.Millisecond
	start := time.Now()
	err := waitBounded("slow-step", budget, func() error {
		<-block
		return nil
	}, slog.New(slog.DiscardHandler))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("err = nil, want a step timeout error")
	}
	if elapsed < budget {
		t.Errorf("waitBounded returned after %v, want at least the %v budget", elapsed, budget)
	}
	const wantSubstr = "slow-step"
	if got := err.Error(); !containsAll(got, wantSubstr, "timed out") {
		t.Errorf("err = %q, want it to name the step (%q) and say it timed out", got, wantSubstr)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !bytes.Contains([]byte(s), []byte(sub)) {
			return false
		}
	}
	return true
}

// ---------- hasFailedArticle ----------

// buildFailArticleJob builds a job whose single file has two articles, and
// returns the manifest/progress pair alongside a helper to fail an article
// by 0-based position within the file.
func buildFailArticleJob(t *testing.T) (*queue.Queue, *queue.Job) {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject: "multi.bin",
		Bytes:   200,
		Articles: []nzb.Article{
			{ID: "first@t", Bytes: 100, Number: 1},
			{ID: "second@t", Bytes: 100, Number: 2},
		},
	}}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "fail-article-job"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	q := queue.New()
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return q, job
}

// TestHasFailedArticle_TrueWhenAnyArticleInRangeFailed pins the "any"
// semantics: a file is reported as having a failed article as soon as one
// article within its [lo,hi) range has exhausted retries, even with a
// second article in the same file still pending.
func TestHasFailedArticle_TrueWhenAnyArticleInRangeFailed(t *testing.T) {
	q, job := buildFailArticleJob(t)
	if _, err := q.MarkArticlesFailed(job.ID, []string{"second@t"}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}

	m, p := mustManifest(t, job), job.Progress()
	if !hasFailedArticle(m, p, 0) {
		t.Error("hasFailedArticle = false, want true: file 0 has a failed article")
	}
}

// TestHasFailedArticle_FalseWhenNoArticleFailed pins the negative case: a
// file whose articles are all still pending (or done) reports no failure.
func TestHasFailedArticle_FalseWhenNoArticleFailed(t *testing.T) {
	_, job := buildFailArticleJob(t)
	m, p := mustManifest(t, job), job.Progress()
	if hasFailedArticle(m, p, 0) {
		t.Error("hasFailedArticle = true, want false: no article has failed")
	}
}

// TestHasFailedArticle_RespectsFileBoundary pins that a failed article in
// one file does not leak into hasFailedArticle's verdict for a different
// file — the [lo,hi) range from FileRange must actually scope the check.
func TestHasFailedArticle_RespectsFileBoundary(t *testing.T) {
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "a.bin", Bytes: 100, Articles: []nzb.Article{{ID: "a@t", Bytes: 100, Number: 1}}},
		{Subject: "b.bin", Bytes: 100, Articles: []nzb.Article{{ID: "b@t", Bytes: 100, Number: 1}}},
	}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "boundary-job"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	q := queue.New()
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := q.MarkArticlesFailed(job.ID, []string{"a@t"}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}

	m, p := mustManifest(t, job), job.Progress()
	if !hasFailedArticle(m, p, 0) {
		t.Error("hasFailedArticle(fileIdx=0) = false, want true: its article failed")
	}
	if hasFailedArticle(m, p, 1) {
		t.Error("hasFailedArticle(fileIdx=1) = true, want false: file 1's article never failed")
	}
}

// ---------- writeNZBBackup ----------

// TestWriteNZBBackup_WritesGzippedFileUsingBasename pins the happy path: the
// returned name is the submitted filename's basename plus ".gz", and the
// file lands at nzbDir/<name> as valid gzip.
func TestWriteNZBBackup_WritesGzippedFileUsingBasename(t *testing.T) {
	nzbDir := t.TempDir()
	raw := []byte("<nzb>hello</nzb>")

	name, err := writeNZBBackup(nzbDir, "sub/dir/Show.S01E01.nzb", raw)
	if err != nil {
		t.Fatalf("writeNZBBackup: %v", err)
	}
	if name != "Show.S01E01.nzb.gz" {
		t.Errorf("name = %q, want %q (basename + .gz, directory stripped)", name, "Show.S01E01.nzb.gz")
	}

	got := readGzFile(t, filepath.Join(nzbDir, name))
	if !bytes.Equal(got, raw) {
		t.Errorf("round-tripped content = %q, want %q", got, raw)
	}
}

// TestWriteNZBBackup_CollisionGetsUniqueSuffix pins that writing a backup
// under a name that already exists does not overwrite it — it takes a
// ".1"-style suffix instead, matching queue.UniqueName's job-name collision
// behaviour, so the original NZB (needed to retry the older job) survives.
func TestWriteNZBBackup_CollisionGetsUniqueSuffix(t *testing.T) {
	nzbDir := t.TempDir()
	first := []byte("<nzb>first</nzb>")
	second := []byte("<nzb>second</nzb>")

	firstName, err := writeNZBBackup(nzbDir, "Show.S01E01.nzb", first)
	if err != nil {
		t.Fatalf("writeNZBBackup (first): %v", err)
	}
	secondName, err := writeNZBBackup(nzbDir, "Show.S01E01.nzb", second)
	if err != nil {
		t.Fatalf("writeNZBBackup (second): %v", err)
	}

	if secondName == firstName {
		t.Fatalf("second write reused name %q; the first backup would have been overwritten", firstName)
	}

	gotFirst := readGzFile(t, filepath.Join(nzbDir, firstName))
	if !bytes.Equal(gotFirst, first) {
		t.Error("first backup's content was overwritten by the second write")
	}
	gotSecond := readGzFile(t, filepath.Join(nzbDir, secondName))
	if !bytes.Equal(gotSecond, second) {
		t.Error("second backup content did not round-trip")
	}
}

// TestWriteNZBBackup_PropagatesWriteError pins that a destination directory
// which does not exist (and so cannot receive the atomic temp file) surfaces
// as an error rather than being swallowed.
func TestWriteNZBBackup_PropagatesWriteError(t *testing.T) {
	missingDir := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := os.Stat(missingDir); !os.IsNotExist(err) {
		t.Fatalf("fixture guard: %s unexpectedly exists", missingDir)
	}

	_, err := writeNZBBackup(missingDir, "job.nzb", []byte("data"))
	if err == nil {
		t.Fatal("writeNZBBackup succeeded against a nonexistent directory, want an error")
	}
}

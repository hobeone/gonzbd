package postproc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// artIdxForID resolves a message ID to its global article index using only
// exported Manifest accessors.
func artIdxForID(t *testing.T, m *job.Manifest, msgID string) int32 {
	t.Helper()
	for i := range m.NumArticles() {
		if m.ArticleID(i) == msgID {
			return int32(i) //nolint:gosec // G115: article counts are far below int32
		}
	}
	t.Fatalf("artIdxForID: no article with message ID %s", msgID)
	return 0
}

// dupcomment:ok four packages each need their own copy of this helper;
// Manifest.fileIndexForArticle is unexported outside package job.
//
// fileIdxForArticle returns the manifest file index owning global article
// index i, using only exported Manifest accessors.
func fileIdxForArticle(m *job.Manifest, i int) (int, bool) {
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		if i >= lo && i < hi {
			return fi, true
		}
	}
	return 0, false
}

// ackFailedIDs marks articles failed keyed by message ID.
func ackFailedIDs(t *testing.T, j *job.Job, m *job.Manifest, msgIDs []string) {
	t.Helper()
	for _, id := range msgIDs {
		idx := int(artIdxForID(t, m, id))
		if err := j.MarkArticleFailed(idx); err != nil {
			t.Fatalf("MarkArticleFailed: %v", err)
		}
	}
}

func TestCompletionPct(t *testing.T) {
	tests := []struct {
		name             string
		downloaded, want int
		total            int
	}{
		{name: "fully downloaded", downloaded: 200, total: 200, want: 100},
		{name: "over-downloaded clamps to 100", downloaded: 250, total: 200, want: 100},
		{name: "half", downloaded: 100, total: 200, want: 50},
		{name: "floors, never rounds up to 100", downloaded: 999, total: 1000, want: 99},
		{name: "nothing", downloaded: 0, total: 200, want: 0},
		{name: "zero total", downloaded: 0, total: 0, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := completionPct(int64(tc.downloaded), int64(tc.total))
			if got != tc.want {
				t.Errorf("completionPct(%d, %d) = %d, want %d", tc.downloaded, tc.total, got, tc.want)
			}
		})
	}
}

// artSpec describes one article's outcome for buildQueueJob.
type artSpec struct {
	bytes  int
	done   bool
	failed bool
}

// fileSpec describes one file for buildQueueJob. bytes is only needed when
// articles is empty (e.g. a deferred par2 recovery volume with no dispatched
// articles); otherwise the file's byte count is the sum of its articles.
type fileSpec struct {
	subject  string
	bytes    int64
	articles []artSpec
}

// buildTestJob builds a real *job.Job and drives it to reach the described
// per-article done/failed state.
func buildTestJob(t *testing.T, onDemandPar2 bool, specs []fileSpec) *job.Job {
	t.Helper()
	files := make([]job.JobFile, 0, len(specs))
	for fi, f := range specs {
		isPar2 := job.IsPar2File(f.subject)
		isRecovery := isPar2 && job.IsRecoveryVolume(f.subject)
		jf := job.JobFile{
			Subject:        f.subject,
			Bytes:          f.bytes,
			IsPar2Recovery: isRecovery,
			Deferred:       isRecovery && onDemandPar2,
		}
		for ai, a := range f.articles {
			jf.Articles = append(jf.Articles, job.JobArticle{
				ID:     fmt.Sprintf("f%da%d@t", fi, ai),
				Bytes:  a.bytes,
				Number: ai + 1,
			})
			jf.Bytes += int64(a.bytes)
		}
		files = append(files, jf)
	}
	job.SortJobFiles(files)

	j := job.New("test-id", "test-name", job.Policy{})
	m := job.NewManifest(files)
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	for fi, jf := range files {
		if jf.Deferred {
			if err := j.SetFileFetchPolicy(fi, job.FetchIfNeeded); err != nil {
				t.Fatalf("SetFileFetchPolicy: %v", err)
			}
		}
	}

	var runs []durability.Run
	var failedIDs []string
	for fi, f := range specs {
		for ai, a := range f.articles {
			id := fmt.Sprintf("f%da%d@t", fi, ai)
			switch {
			case a.failed:
				failedIDs = append(failedIDs, id)
			case a.done:
				globalIdx := int(artIdxForID(t, m, id))
				mfi, ok := fileIdxForArticle(m, globalIdx)
				if !ok {
					t.Fatalf("buildTestJob: article %s (global idx %d) not owned by any manifest file", id, globalIdx)
				}
				runs = append(runs, durability.Run{
					FileIdx:     int32(mfi),       //nolint:gosec // G115: file counts are far below int32
					FirstArtIdx: int32(globalIdx), //nolint:gosec // G115: article counts are far below int32
					LastArtIdx:  int32(globalIdx), //nolint:gosec // G115: article counts are far below int32
					Length:      int64(m.ArticleBytes(globalIdx)),
				})
			}
		}
	}
	if len(failedIDs) > 0 {
		ackFailedIDs(t, j, m, failedIDs)
	}
	if len(runs) > 0 {
		if err := j.SeedFromRuns(runs); err != nil {
			t.Fatalf("SeedFromRuns: %v", err)
		}
	}
	return j
}

func TestBuildFileCompletionLines(t *testing.T) {
	t.Run("flags the file that owns the failed bytes", func(t *testing.T) {
		// movie.mkv: both articles succeeded -> 100%.
		// extra.rar: one of two articles failed -> 50% (100 B of 200 B).
		// This mirrors the user's scenario: the per-file ⚠ line pinpoints
		// which file is short, and its missing bytes are the job's failed
		// bytes (at finalize nothing is still pending).
		qjob := buildTestJob(t, false, []fileSpec{
			{subject: "movie.mkv", articles: []artSpec{{bytes: 100, done: true}, {bytes: 100, done: true}}},
			{subject: "extra.rar", articles: []artSpec{{bytes: 100, done: true}, {bytes: 100, failed: true}}},
		})
		job := &Job{Job: qjob}

		lines := buildFileCompletionLines(job)
		joined := strings.Join(lines, "\n")

		if !strings.Contains(joined, "File completion (1 of 2 incomplete):") {
			t.Errorf("missing/incorrect header; got:\n%s", joined)
		}
		if !strings.Contains(joined, "✓ movie.mkv — 100%") {
			t.Errorf("complete file not marked done; got:\n%s", joined)
		}
		if !strings.Contains(joined, "⚠ extra.rar — 50%") {
			t.Errorf("incomplete file not flagged at 50%%; got:\n%s", joined)
		}
		if !strings.Contains(joined, "received") {
			t.Errorf("incomplete line should report bytes received; got:\n%s", joined)
		}
	})

	t.Run("deferred par2 files are marked as not downloaded and omit from incomplete", func(t *testing.T) {
		qjob := buildTestJob(t, true, []fileSpec{
			{subject: "movie.mkv", articles: []artSpec{{bytes: 100, done: true}}},
			{subject: "extra.vol000+01.par2", bytes: 1000},
		})
		job := &Job{Job: qjob}
		lines := buildFileCompletionLines(job)
		joined := strings.Join(lines, "\n")

		if !strings.Contains(joined, "File completion (2 files, all complete):") {
			t.Errorf("expected all-complete header since deferred is not counted as incomplete; got:\n%s", joined)
		}
		if !strings.Contains(joined, "  - extra.vol000+01.par2 — not downloaded") {
			t.Errorf("expected deferred file to be listed as not downloaded; got:\n%s", joined)
		}
		if strings.Contains(joined, "⚠") {
			t.Errorf("no file should be flagged as incomplete; got:\n%s", joined)
		}
	})

	t.Run("all complete", func(t *testing.T) {
		qjob := buildTestJob(t, false, []fileSpec{
			{subject: "a.bin", articles: []artSpec{{bytes: 50, done: true}}},
			{subject: "b.bin", articles: []artSpec{{bytes: 50, done: true}}},
		})
		job := &Job{Job: qjob}
		lines := buildFileCompletionLines(job)
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "File completion (2 files, all complete):") {
			t.Errorf("expected all-complete header; got:\n%s", joined)
		}
		if strings.Contains(joined, "⚠") {
			t.Errorf("no file should be flagged; got:\n%s", joined)
		}
	})

	t.Run("no files yields no lines", func(t *testing.T) {
		qjob := buildTestJob(t, false, nil)
		job := &Job{Job: qjob}
		if lines := buildFileCompletionLines(job); lines != nil {
			t.Errorf("expected nil for empty file list, got %v", lines)
		}
	})
}

func TestBuildDownloadFileList_Par2Summary(t *testing.T) {
	t.Run("clean: deferred volumes skipped", func(t *testing.T) {
		dir := t.TempDir()
		qjob := buildTestJob(t, true, []fileSpec{
			{subject: "release.rar", articles: []artSpec{{bytes: 500, done: true}}},
			{subject: "x.vol000+01.par2", bytes: 500},
		})
		job := &Job{DownloadDir: dir, Job: qjob}
		got := strings.Join(buildDownloadFileList(job), "\n")
		if !strings.Contains(got, "✓ Par2: verified clean") {
			t.Errorf("expected clean par2 line; got:\n%s", got)
		}
		if !strings.Contains(got, "skipped") {
			t.Errorf("expected 'skipped' in par2 line; got:\n%s", got)
		}
		if !strings.Contains(got, "saved 500 B by not downloading par2 files") {
			t.Errorf("expected saved bytes message; got:\n%s", got)
		}
	})

	t.Run("fetched: repair was needed", func(t *testing.T) {
		dir := t.TempDir()
		qjob := buildTestJob(t, true, []fileSpec{
			{subject: "release.rar", articles: []artSpec{{bytes: 500, done: true}}},
			{subject: "x.vol000+01.par2", bytes: 500},
		})
		if err := qjob.UndeferRecoveryVolumes([]int{1}); err != nil {
			t.Fatalf("UndeferRecoveryVolumes: %v", err)
		}
		qjob.SetPar2ReleaseReason("repair needed")
		job := &Job{DownloadDir: dir, Job: qjob}
		got := strings.Join(buildDownloadFileList(job), "\n")
		if !strings.Contains(got, "⚠ Par2: fetched") {
			t.Errorf("expected fetched par2 line; got:\n%s", got)
		}
		if !strings.Contains(got, "(reason: repair needed)") {
			t.Errorf("expected reason; got:\n%s", got)
		}
	})

	t.Run("unknown: verdict reached but nothing was verified", func(t *testing.T) {
		dir := t.TempDir()
		qjob := buildTestJob(t, true, []fileSpec{
			{subject: "release.rar", articles: []artSpec{{bytes: 500, done: true}}},
			{subject: "x.vol000+01.par2", bytes: 500},
		})
		qjob.SetPar2ReleaseReason("no delivered file matched any par2 entry")
		job := &Job{DownloadDir: dir, Job: qjob}
		got := strings.Join(buildDownloadFileList(job), "\n")
		if strings.Contains(got, "verified clean") {
			t.Errorf("must not claim verified clean when nothing was verified; got:\n%s", got)
		}
		if !strings.Contains(got, "no delivered file matched any par2 entry") {
			t.Errorf("expected the release reason in the output; got:\n%s", got)
		}
	})

	t.Run("recovered: an already-undeferred volume must not read as could-not-verify", func(t *testing.T) {
		// Two recovery volumes, only one undeferred: Par2Recovered() reads
		// true (undeferRecovery sets it on any change) while the other
		// volume is still held, so heldVols > 0 too. That combination is
		// what pins the new case's `!p.Par2Recovered()` conjunct — drop it
		// and heldVols>0 && HasPar2Verdict() alone starts matching here,
		// which it must not: a job that already has a repair verdict is not
		// the "could not verify" state that case exists to report.
		//
		// The plain `case heldVols > 0:` fallback (unconditional on
		// Par2Recovered) intercepts this fixture ahead of `case
		// recoveryVols > 0 && p.Par2Recovered():`, so the correct-code
		// output actually observed here is "verified clean", not "fetched
		// ... for repair" — that ordering is pre-existing (unconditional on
		// UndeferRecoveryVolumes always releasing every deferred index at
		// once, so filelist.go never sees this state from app.go) and out
		// of this task's scope. What this test pins is narrower and
		// sufficient: the new case must not fire and claim "could not
		// verify" for a job a verdict already released volumes for.
		dir := t.TempDir()
		qjob := buildTestJob(t, true, []fileSpec{
			{subject: "release.rar", articles: []artSpec{{bytes: 500, done: true}}},
			{subject: "x.vol000+01.par2", bytes: 500},
			{subject: "x.vol001+01.par2", bytes: 500},
		})
		if err := qjob.UndeferRecoveryVolumes([]int{1}); err != nil {
			t.Fatalf("UndeferRecoveryVolumes: %v", err)
		}
		qjob.SetPar2ReleaseReason("repair needed")
		if !qjob.Progress().Par2Recovered() {
			t.Fatal("fixture guard: Par2Recovered() must be true")
		}
		job := &Job{DownloadDir: dir, Job: qjob}
		got := strings.Join(buildDownloadFileList(job), "\n")
		if strings.Contains(got, "could not verify") {
			t.Errorf("a job whose verdict already released volumes must not read as could-not-verify; got:\n%s", got)
		}
	})

	t.Run("off/normal: no on-demand, no summary line", func(t *testing.T) {
		dir := t.TempDir()
		qjob := buildTestJob(t, false, []fileSpec{
			{subject: "release.rar", articles: []artSpec{{bytes: 500, done: true}}},
			{subject: "x.vol000+01.par2", bytes: 500},
		})
		job := &Job{DownloadDir: dir, Job: qjob}
		got := strings.Join(buildDownloadFileList(job), "\n")
		if strings.Contains(got, "Par2:") {
			t.Errorf("expected no Par2 summary line; got:\n%s", got)
		}
	})
}

// TestBuildDownloadFileListIncludesCompletion verifies the completion section
// is added alongside (not replacing) the on-disk file listing.
func TestBuildDownloadFileListIncludesCompletion(t *testing.T) {
	dir := t.TempDir()

	// Create a file to test the file size warning / info checks
	if err := os.WriteFile(filepath.Join(dir, "file1.txt"), make([]byte, 1234), 0o644); err != nil {
		t.Fatal(err)
	}

	qjob := buildTestJob(t, false, []fileSpec{
		{subject: "short.rar", articles: []artSpec{{bytes: 100, done: true}, {bytes: 100, failed: true}}},
	})
	if err := qjob.RecordDownload("news.server.com", 123); err != nil {
		t.Fatalf("RecordDownload: %v", err)
	}
	job := &Job{DownloadDir: dir, Job: qjob}
	lines := buildDownloadFileList(job)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "File completion (1 of 1 incomplete):") {
		t.Errorf("completion section missing; got:\n%s", joined)
	}
	if !strings.Contains(joined, "⚠ short.rar — 50%") {
		t.Errorf("per-file percentage missing; got:\n%s", joined)
	}
	// The original on-disk listing must still be present (additive change).
	if !strings.Contains(joined, "Files in download directory") {
		t.Errorf("on-disk listing was removed; got:\n%s", joined)
	}
	// Assert failed bytes warning is logged
	if !strings.Contains(joined, "failed (50.0%)") {
		t.Errorf("expected failed bytes warning; got:\n%s", joined)
	}
	// Assert server stats are logged
	if !strings.Contains(joined, "Servers: news.server.com: 123 B") {
		t.Errorf("expected server stats; got:\n%s", joined)
	}
	// Assert file size is formatted correctly (not 0 B)
	if !strings.Contains(joined, "file1.txt (1.2 KiB)") {
		t.Errorf("expected file1.txt size to be 1.2 KiB; got:\n%s", joined)
	}
}

// TestBuildDownloadFileListRecursesSubdirectories verifies that files nested
// in subdirectories of the download dir are listed (not just the bare
// "dirname/" entry), with each nesting level indented two more spaces, and
// that the header count reflects files only (not directories).
func TestBuildDownloadFileListRecursesSubdirectories(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file1.txt"), make([]byte, 1234), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "nested.txt"), make([]byte, 1234), 0o644); err != nil {
		t.Fatal(err)
	}

	qjob := buildTestJob(t, false, nil)
	job := &Job{DownloadDir: dir, Job: qjob}
	joined := strings.Join(buildDownloadFileList(job), "\n")

	if !strings.Contains(joined, "Files in download directory (2):") {
		t.Errorf("expected header counting files only (not dirs); got:\n%s", joined)
	}
	if !strings.Contains(joined, "  📁 subdir/") {
		t.Errorf("expected subdirectory entry; got:\n%s", joined)
	}
	if !strings.Contains(joined, "    nested.txt (1.2 KiB)") {
		t.Errorf("expected nested file indented two more spaces under its directory; got:\n%s", joined)
	}
}

func TestBuildFinalFileListDirect(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Directory is missing / nonexistent
	jobMissing := &Job{FinalDir: "/nonexistent/path/ever"}
	lines := buildFinalFileList(jobMissing)
	if lines != nil {
		t.Errorf("expected nil for nonexistent directory, got %v", lines)
	}

	// 2. Directory exists and contains files/folders
	subDir := filepath.Join(tmpDir, "folder1")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	filePath := filepath.Join(tmpDir, "file1.txt")
	content := []byte(strings.Repeat("A", 1234)) // 1234 bytes
	if err := os.WriteFile(filePath, content, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	job := &Job{FinalDir: tmpDir}
	lines = buildFinalFileList(job)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "Final files (2):") {
		t.Errorf("expected header 'Final files (2):', got:\n%s", joined)
	}
	if !strings.Contains(joined, "file1.txt (1.2 KiB)") {
		t.Errorf("expected file1.txt (1.2 KiB) in output, got:\n%s", joined)
	}
	if !strings.Contains(joined, "📁 folder1/") {
		t.Errorf("expected folder1/ directory in output, got:\n%s", joined)
	}
	if !strings.Contains(joined, "Total: 1.2 KiB") {
		t.Errorf("expected Total: 1.2 KiB in output, got:\n%s", joined)
	}
}

func TestBuildDirTree(t *testing.T) {
	t.Run("nonexistent directory returns error", func(t *testing.T) {
		lines, count, err := buildDirTree("/nonexistent/path/ever", "  ")
		if err == nil {
			t.Errorf("expected error for nonexistent directory, got nil (lines: %v, count: %d)", lines, count)
		}
	})

	t.Run("directory with files and subdirectories", func(t *testing.T) {
		dir := t.TempDir()
		subDir := filepath.Join(dir, "subdir")
		if err := os.Mkdir(subDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "file1.txt"), make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(subDir, "nested.txt"), make([]byte, 200), 0o644); err != nil {
			t.Fatal(err)
		}

		lines, count, err := buildDirTree(dir, "  ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count != 2 {
			t.Errorf("expected count=2, got %d", count)
		}
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "  file1.txt (100 B)") {
			t.Errorf("missing file1.txt line; got:\n%s", joined)
		}
		if !strings.Contains(joined, "  📁 subdir/") {
			t.Errorf("missing subdir line; got:\n%s", joined)
		}
		if !strings.Contains(joined, "    nested.txt (200 B)") {
			t.Errorf("missing nested.txt line; got:\n%s", joined)
		}
	})

	t.Run("unreadable subdirectory inline error", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("skipping permission test when running as root")
		}
		dir := t.TempDir()
		noPermDir := filepath.Join(dir, "noperm")
		if err := os.Mkdir(noPermDir, 0o000); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		defer os.Chmod(noPermDir, 0o755)

		lines, count, err := buildDirTree(dir, "  ")
		if err != nil {
			t.Fatalf("unexpected top-level error: %v", err)
		}
		if count != 0 {
			t.Errorf("expected count=0, got %d", count)
		}
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "Error reading dir:") {
			t.Errorf("expected inline error message; got:\n%s", joined)
		}
	})
}

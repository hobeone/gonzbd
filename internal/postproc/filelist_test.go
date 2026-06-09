package postproc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
)

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

// art is a small helper for building articles with a byte count and outcome.
func art(bytes int, done, failed bool) queue.JobArticle {
	return queue.JobArticle{Bytes: bytes, Done: done, Failed: failed}
}

func TestBuildFileCompletionLines(t *testing.T) {
	t.Run("flags the file that owns the failed bytes", func(t *testing.T) {
		// movie.mkv: both articles succeeded -> 100%.
		// extra.rar: one of two articles failed -> 50% (100 B of 200 B).
		// This mirrors the user's scenario: the per-file ⚠ line pinpoints
		// which file is short, and its missing bytes are the job's failed
		// bytes (at finalize nothing is still pending).
		job := &Job{Queue: &queue.Job{
			Files: []queue.JobFile{
				{Subject: "movie.mkv", Articles: []queue.JobArticle{
					art(100, true, false), art(100, true, false),
				}},
				{Subject: "extra.rar", Articles: []queue.JobArticle{
					art(100, true, false), art(100, false, true),
				}},
			},
		}}

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
		job := &Job{Queue: &queue.Job{
			Files: []queue.JobFile{
				{Subject: "movie.mkv", Articles: []queue.JobArticle{art(100, true, false)}},
				{Subject: "extra.par2", IsPar2Recovery: true, Deferred: true, Bytes: 1000},
			},
		}}
		lines := buildFileCompletionLines(job)
		joined := strings.Join(lines, "\n")

		if !strings.Contains(joined, "File completion (2 files, all complete):") {
			t.Errorf("expected all-complete header since deferred is not counted as incomplete; got:\n%s", joined)
		}
		if !strings.Contains(joined, "  - extra.par2 — not downloaded") {
			t.Errorf("expected deferred file to be listed as not downloaded; got:\n%s", joined)
		}
		if strings.Contains(joined, "⚠") {
			t.Errorf("no file should be flagged as incomplete; got:\n%s", joined)
		}
	})

	t.Run("all complete", func(t *testing.T) {
		job := &Job{Queue: &queue.Job{
			Files: []queue.JobFile{
				{Subject: "a.bin", Articles: []queue.JobArticle{art(50, true, false)}},
				{Subject: "b.bin", Articles: []queue.JobArticle{art(50, true, false)}},
			},
		}}
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
		job := &Job{Queue: &queue.Job{}}
		if lines := buildFileCompletionLines(job); lines != nil {
			t.Errorf("expected nil for empty file list, got %v", lines)
		}
	})
}

func TestBuildDownloadFileList_Par2Summary(t *testing.T) {
	t.Run("clean: deferred volumes skipped", func(t *testing.T) {
		dir := t.TempDir()
		job := &Job{
			DownloadDir: dir,
			Queue: &queue.Job{
				TotalBytes: 1000,
				Files: []queue.JobFile{
					{Subject: "release.rar", Articles: []queue.JobArticle{art(500, true, false)}},
					{Subject: "x.vol000+01.par2", IsPar2Recovery: true, Deferred: true, Bytes: 500},
				},
			},
		}
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
		job := &Job{
			DownloadDir: dir,
			Queue: &queue.Job{
				TotalBytes:        1000,
				Par2Recovered:     true,
				Par2ReleaseReason: "repair needed",
				Files: []queue.JobFile{
					{Subject: "release.rar", Articles: []queue.JobArticle{art(500, true, false)}},
					{Subject: "x.vol000+01.par2", IsPar2Recovery: true, Deferred: false, Bytes: 500},
				},
			},
		}
		got := strings.Join(buildDownloadFileList(job), "\n")
		if !strings.Contains(got, "⚠ Par2: fetched") {
			t.Errorf("expected fetched par2 line; got:\n%s", got)
		}
		if !strings.Contains(got, "(reason: repair needed)") {
			t.Errorf("expected reason; got:\n%s", got)
		}
	})

	t.Run("off/normal: no on-demand, no summary line", func(t *testing.T) {
		dir := t.TempDir()
		job := &Job{
			DownloadDir: dir,
			Queue: &queue.Job{
				TotalBytes:    1000,
				Par2Recovered: false,
				Files: []queue.JobFile{
					{Subject: "release.rar", Articles: []queue.JobArticle{art(500, true, false)}},
					{Subject: "x.vol000+01.par2", IsPar2Recovery: true, Deferred: false, Bytes: 500},
				},
			},
		}
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

	job := &Job{
		DownloadDir: dir,
		Queue: &queue.Job{
			TotalBytes:  200,
			FailedBytes: 100,
			ServerStats: map[string]int64{
				"news.server.com": 123,
			},
			Files: []queue.JobFile{
				{Subject: "short.rar", Articles: []queue.JobArticle{
					art(100, true, false), art(100, false, true),
				}},
			},
		},
	}
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

package postproc

import (
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
	})

	t.Run("fetched: repair was needed", func(t *testing.T) {
		dir := t.TempDir()
		job := &Job{
			DownloadDir: dir,
			Queue: &queue.Job{
				TotalBytes:    1000,
				Par2Recovered: true,
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
	job := &Job{
		DownloadDir: dir,
		Queue: &queue.Job{
			TotalBytes:  200,
			FailedBytes: 100,
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
}

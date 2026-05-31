package postproc

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/humanfmt"
)

// buildDownloadFileList creates the lines for the synthetic "download"
// StageLogEntry. It scans job.DownloadDir (non-recursively) and produces
// a human-readable listing that includes:
//   - per-file name and size
//   - download duration and total size
//   - article completion stats from the queue
func buildDownloadFileList(job *Job) []string {
	var lines []string

	// On-demand par2 stats: count skipped recovery volumes and bytes saved.
	var heldVols, recoveryVols int
	var heldBytes int64
	for i := range job.Queue.Files {
		f := &job.Queue.Files[i]
		if !f.IsPar2Recovery {
			continue
		}
		recoveryVols++
		if f.Deferred {
			heldVols++
			heldBytes += f.Bytes
		}
	}

	// Download duration header.
	var dlDuration time.Duration
	if !job.Queue.DownloadStarted.IsZero() && !job.Queue.DownloadFinished.IsZero() {
		dlDuration = job.Queue.DownloadFinished.Sub(job.Queue.DownloadStarted)
	}

	if heldBytes > 0 {
		lines = append(lines, fmt.Sprintf("Downloaded %s (saved %s on-demand) in %s",
			humanfmt.BytesSI(job.Queue.TotalBytes-heldBytes),
			humanfmt.BytesSI(heldBytes),
			humanfmt.Duration(dlDuration)))
	} else {
		lines = append(lines, fmt.Sprintf("Downloaded %s in %s",
			humanfmt.BytesSI(job.Queue.TotalBytes),
			humanfmt.Duration(dlDuration)))
	}

	// Failed/remaining bytes summary.
	if job.Queue.FailedBytes > 0 {
		lines = append(lines, fmt.Sprintf("⚠ %s failed (%.1f%%)",
			humanfmt.BytesSI(job.Queue.FailedBytes),
			float64(job.Queue.FailedBytes)/float64(job.Queue.TotalBytes)*100))
	}

	switch {
	case heldVols > 0:
		// Still deferred at finalize => never downloaded => verified clean.
		lines = append(lines, fmt.Sprintf("✓ Par2: verified clean from index — %d recovery volume(s) skipped (saved %s)",
			heldVols, humanfmt.BytesSI(heldBytes)))
	case recoveryVols > 0 && job.Queue.Par2Recovered:
		// Volumes were un-deferred and fetched because repair was needed.
		reasonStr := ""
		if job.Queue.Par2ReleaseReason != "" {
			reasonStr = fmt.Sprintf(" (reason: %s)", job.Queue.Par2ReleaseReason)
		}
		lines = append(lines, fmt.Sprintf("⚠ Par2: fetched %d recovery volume(s) for repair%s", recoveryVols, reasonStr))
	}

	// Per-file download completion. Lets the user see exactly which files
	// came up short and reconcile them against the failed-bytes total above
	// (at post-processing time nothing is still pending, so any file below
	// 100% is short because some of its articles failed). Percentages are
	// derived from article byte sums rather than JobFile.Bytes so they are
	// internally consistent: a file reads 100% only when every article
	// downloaded successfully.
	lines = append(lines, buildFileCompletionLines(job)...)

	// Enumerate files in the download directory.
	entries, err := os.ReadDir(job.DownloadDir)
	if err != nil {
		lines = append(lines, fmt.Sprintf("Error reading download dir: %v", err))
		return lines
	}

	lines = append(lines, fmt.Sprintf("Files in download directory (%d):", len(entries)))
	for _, e := range entries {
		info, _ := e.Info()
		var sz int64
		if info != nil {
			sz = info.Size()
		}
		prefix := "  "
		if e.IsDir() {
			prefix = "  📁 "
			lines = append(lines, fmt.Sprintf("%s%s/", prefix, e.Name()))
		} else {
			lines = append(lines, fmt.Sprintf("%s%s (%s)", prefix, e.Name(), humanfmt.BytesSI(sz)))
		}
	}

	// Server stats, if available.
	if len(job.Queue.ServerStats) > 0 {
		var parts []string
		for srv, bytes := range job.Queue.ServerStats {
			parts = append(parts, fmt.Sprintf("%s: %s", srv, humanfmt.BytesSI(bytes)))
		}
		lines = append(lines, "Servers: "+strings.Join(parts, ", "))
	}

	return lines
}

// buildFileCompletionLines produces one line per job file showing its
// download completeness, plus a header summarising how many files are
// incomplete. Complete files are marked "✓ … 100%"; short files are marked
// "⚠ … N% (X of Y received)" so it is clear which files failed to download
// in full. A short file is not necessarily a broken file — a later par2
// repair stage may still recover it; this section reports download
// completeness only.
func buildFileCompletionLines(job *Job) []string {
	files := job.Queue.Files
	if len(files) == 0 {
		return nil
	}

	incomplete := 0
	fileLines := make([]string, 0, len(files))
	for fi := range files {
		f := &files[fi]
		var downloaded, total int64
		anyFailed := false
		for ai := range f.Articles {
			a := &f.Articles[ai]
			total += int64(a.Bytes)
			switch {
			case a.Failed:
				anyFailed = true
			case a.Done:
				downloaded += int64(a.Bytes)
			}
		}
		pct := completionPct(downloaded, total)
		if pct >= 100 && !anyFailed {
			fileLines = append(fileLines, fmt.Sprintf("  ✓ %s — 100%% (%s)",
				f.Subject, humanfmt.BytesSI(total)))
			continue
		}
		incomplete++
		fileLines = append(fileLines, fmt.Sprintf("  ⚠ %s — %d%% (%s of %s received)",
			f.Subject, pct, humanfmt.BytesSI(downloaded), humanfmt.BytesSI(total)))
	}

	var header string
	if incomplete > 0 {
		header = fmt.Sprintf("File completion (%d of %d incomplete):", incomplete, len(files))
	} else {
		header = fmt.Sprintf("File completion (%d files, all complete):", len(files))
	}
	return append([]string{header}, fileLines...)
}

// completionPct returns the integer percentage (0-100) of total bytes that
// downloaded successfully. It floors the result so a file missing any data
// never rounds up to 100, and returns 0 when total is non-positive.
func completionPct(downloaded, total int64) int {
	if total <= 0 {
		return 0
	}
	if downloaded >= total {
		return 100
	}
	return int(float64(downloaded) / float64(total) * 100)
}

// buildFinalFileList creates a file listing of the job's final directory
// for the summary stage. This shows the end state after all processing.
func buildFinalFileList(job *Job) []string {
	dir := job.FinalDir
	if dir == "" {
		dir = job.DownloadDir
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var lines []string
	var totalSize int64
	lines = append(lines, fmt.Sprintf("Final files (%d):", len(entries)))
	for _, e := range entries {
		info, _ := e.Info()
		var sz int64
		if info != nil {
			sz = info.Size()
		}
		totalSize += sz
		if e.IsDir() {
			lines = append(lines, fmt.Sprintf("  📁 %s/", e.Name()))
		} else {
			lines = append(lines, fmt.Sprintf("  %s (%s)", e.Name(), humanfmt.BytesSI(sz)))
		}
	}
	lines = append(lines, fmt.Sprintf("Total: %s", humanfmt.BytesSI(totalSize)))

	return lines
}

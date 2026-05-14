package postproc

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// buildDownloadFileList creates the lines for the synthetic "download"
// StageLogEntry. It scans job.DownloadDir (non-recursively) and produces
// a human-readable listing that includes:
//   - per-file name and size
//   - download duration and total size
//   - article completion stats from the queue
func buildDownloadFileList(job *Job) []string {
	var lines []string

	// Download duration header.
	var dlDuration time.Duration
	if !job.Queue.DownloadStarted.IsZero() && !job.Queue.DownloadFinished.IsZero() {
		dlDuration = job.Queue.DownloadFinished.Sub(job.Queue.DownloadStarted)
	}
	lines = append(lines, fmt.Sprintf("Downloaded %s in %s",
		formatBytesSI(job.Queue.TotalBytes),
		formatDurationHuman(dlDuration)))

	// Failed/remaining bytes summary.
	if job.Queue.FailedBytes > 0 {
		lines = append(lines, fmt.Sprintf("⚠ %s failed (%.1f%%)",
			formatBytesSI(job.Queue.FailedBytes),
			float64(job.Queue.FailedBytes)/float64(job.Queue.TotalBytes)*100))
	}

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
			lines = append(lines, fmt.Sprintf("%s%s (%s)", prefix, e.Name(), formatBytesSI(sz)))
		}
	}

	// Server stats, if available.
	if len(job.Queue.ServerStats) > 0 {
		var parts []string
		for srv, bytes := range job.Queue.ServerStats {
			parts = append(parts, fmt.Sprintf("%s: %s", srv, formatBytesSI(bytes)))
		}
		lines = append(lines, "Servers: "+strings.Join(parts, ", "))
	}

	return lines
}

// formatBytesSI formats bytes into a human-readable string using SI units.
func formatBytesSI(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMG"[exp])
}

// formatDurationHuman produces a human-readable duration string.
func formatDurationHuman(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m < 60 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := m / 60
	m = m % 60
	return fmt.Sprintf("%dh %dm", h, m)
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
			lines = append(lines, fmt.Sprintf("  %s (%s)", e.Name(), formatBytesSI(sz)))
		}
	}
	lines = append(lines, fmt.Sprintf("Total: %s", formatBytesSI(totalSize)))

	return lines
}

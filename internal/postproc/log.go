package postproc

import (
	"context"
	"fmt"
	"log/slog"
)

// logf writes a formatted message to both the structured logger and
// the job's OutputLines buffer (which feeds StageLog → history → UI).
// Messages at Info level and above are persisted to history; Debug
// messages appear only in the daemon log.
func logf(log *slog.Logger, job *Job, level slog.Level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Log(context.Background(), level, msg)
	// Only persist Info+ to history; Debug stays in daemon log only.
	if level >= slog.LevelInfo {
		job.OutputLines = append(job.OutputLines, msg)
	}
}

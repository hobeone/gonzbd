package queue

import "github.com/hobeone/gonzbd/internal/job"

// isRecoveryVolume reports whether filename names a par2 recovery volume.
func isRecoveryVolume(filename string) bool {
	return job.IsRecoveryVolume(filename)
}

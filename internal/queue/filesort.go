package queue

import "github.com/hobeone/gonzbd/internal/job"

// sortJobFiles reorders files so that RAR volumes appear first, sorted by
// (set name, volume number). Non-RAR files retain their relative document
// order. The sort is stable.
func sortJobFiles(files []JobFile) {
	job.SortJobFiles(files)
}

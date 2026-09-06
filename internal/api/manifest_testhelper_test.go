package api

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// mustManifest returns j's manifest, failing the test if it is unavailable.
func mustManifest(t *testing.T, j *job.Job) *job.Manifest {
	t.Helper()
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest() for job %s: %v", j.ID(), err)
	}
	return m
}

func makeJobManifest(t *testing.T, subjects []string, fileBytes []int64, artBytes [][]int64, artIDs [][]string) *job.Manifest {
	t.Helper()
	jfiles := make([]job.JobFile, 0, len(subjects))
	for fi, subj := range subjects {
		var arts []job.JobArticle
		for ai, b := range artBytes[fi] {
			arts = append(arts, job.JobArticle{
				ID:    artIDs[fi][ai],
				Bytes: int(b),
			})
		}
		jfiles = append(jfiles, job.JobFile{
			Subject:  subj,
			Bytes:    fileBytes[fi],
			Articles: arts,
		})
	}
	return job.NewManifest(jfiles)
}

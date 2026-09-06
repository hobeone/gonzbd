package app

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

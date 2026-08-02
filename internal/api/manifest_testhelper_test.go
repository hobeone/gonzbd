package api

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
)

// mustManifest returns j's manifest, failing the test if it is unavailable.
// See the queue package's copy for why tests use this rather than checking
// the error at every site.
func mustManifest(t *testing.T, j *queue.Job) *queue.Manifest {
	t.Helper()
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest() for job %s: %v", j.ID, err)
	}
	return m
}

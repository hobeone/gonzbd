package queue_test

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
)

// mustManifest returns j's manifest, failing the test if it is unavailable.
// See the queue package's internal copy for why tests use this rather than
// checking the error at every site.
func mustManifest(t *testing.T, j *queue.Job) *queue.Manifest {
	t.Helper()
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest() for job %s: %v", j.ID, err)
	}
	return m
}

// dupcomment:ok the queue and queue_test packages each need their own
// copy; Go cannot share an unexported helper across that boundary.
//
// manifestResident reports whether j currently has a manifest, without
// caring why not. Fixture guards assert on residency itself, where the
// difference between "evicted" and "unreadable" is not the point — and
// where mustManifest would abort the very case being set up.
func manifestResident(j *queue.Job) bool {
	m, err := j.Manifest()
	return err == nil && m != nil
}

package queue

import "testing"

// mustManifest returns j's manifest, failing the test if it is unavailable.
//
// Job.Manifest is fallible so that production code cannot ignore an absent
// manifest. Tests that have deliberately arranged a resident job are not the
// audience for that error, and threading a check through every one of them
// would bury the handful of tests where absence is the property under test.
// Those call Manifest directly and assert on the error.
func mustManifest(t *testing.T, j *Job) *Manifest {
	t.Helper()
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest() for job %s: %v", j.ID, err)
	}
	return m
}

// dupcomment:ok the queue and queue_test packages each need their own copy;
// Go cannot share an unexported helper across that boundary.
//
// manifestResident reports whether j currently has a manifest, without
// caring why not. Fixture guards assert on residency itself, where the
// difference between "evicted" and "unreadable" is not the point — and
// where mustManifest would abort the very case being set up.
func manifestResident(j *Job) bool {
	m, err := j.Manifest()
	return err == nil && m != nil
}

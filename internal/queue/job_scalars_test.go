package queue

import "testing"

// The five manifest scalars must be readable from a job whose manifest has
// been evicted. That is the entire point of promoting them: a reporting
// path must never need a manifest.
func TestJobScalarsSurviveEviction(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "scalars", 2, 3) // 2 files, 3 articles each
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	wantBytes := job.TotalBytes()
	if wantBytes == 0 {
		t.Fatal("fixture is useless: TotalBytes is zero while resident")
	}

	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	q.mu.RLock()
	resident := q.byID[job.ID].manifest != nil
	q.mu.RUnlock()
	if resident {
		t.Fatal("fixture guard: job still resident after Pause, nothing is being tested")
	}

	if got := job.TotalBytes(); got != wantBytes {
		t.Errorf("TotalBytes after eviction = %d, want %d", got, wantBytes)
	}
	if got := job.NumFiles(); got != 2 {
		t.Errorf("NumFiles after eviction = %d, want 2", got)
	}
	if got := job.NumArticles(); got != 6 {
		t.Errorf("NumArticles after eviction = %d, want 6", got)
	}
}

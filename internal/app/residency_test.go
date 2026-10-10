package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/job/jobtest"
)

func writeTestManifest(t *testing.T, path string, _ *job.Job) {
	t.Helper()
	content := `{"files":[{"subject":"test.rar","bytes":100,"articles":[{"id":"m1","bytes":100,"number":1}]}]}`
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestAppResidency_HydrateThenEvict pins the contract dispatch.Residency
// states: Hydrate makes the manifest available and may block on disk; Evict
// takes it away. The dispatcher decides WHEN and delegates WHAT to here.
func TestAppResidency_HydrateThenEvict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j := job.New("abc123", "test", job.PolicyFromPP(3))
	writeTestManifest(t, filepath.Join(dir, "abc123.json.gz"), j)

	r := newAppResidency(func(id string) (*job.Job, bool) {
		if id == "abc123" {
			return j, true
		}
		return nil, false
	}, dir, nil, nil)

	if j.Resident() {
		t.Fatal("precondition: job must start non-resident")
	}
	if err := r.Hydrate(context.Background(), "abc123"); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if !j.Resident() {
		t.Fatal("Hydrate must leave the job resident")
	}

	r.Evict("abc123")
	if j.Resident() {
		t.Fatal("Evict must leave the job non-resident")
	}

	// Subtest: Re-hydration preserves progress rather than re-zeroing counters.
	if err := r.Hydrate(context.Background(), "abc123"); err != nil {
		t.Fatalf("second Hydrate: %v", err)
	}
	jobtest.MarkArticleWritten(t, j, 0)
	if !j.Progress().ArticleDone(0) {
		t.Fatal("precondition: article 0 must be marked done")
	}

	r.Evict("abc123")
	if j.Resident() {
		t.Fatal("Evict must leave the job non-resident")
	}

	if err := r.Hydrate(context.Background(), "abc123"); err != nil {
		t.Fatalf("re-Hydrate: %v", err)
	}
	if !j.Resident() {
		t.Fatal("re-Hydrate must leave the job resident")
	}
	if !j.Progress().ArticleDone(0) {
		t.Fatal("re-hydration re-zeroed progress counters instead of preserving them via RestoreContent")
	}
}

// TestAppResidency_RehydrationKeepsAFailureRecordedWhileEvicted pins the
// hydration half of recording an evicted job's failure: Hydrate restores onto
// the job's own progress record, so the failed bit survives and is charged.
func TestAppResidency_RehydrationKeepsAFailureRecordedWhileEvicted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j := job.New("abc123", "test", job.PolicyFromPP(3))
	writeTestManifest(t, filepath.Join(dir, "abc123.json.gz"), j)
	r := newAppResidency(func(id string) (*job.Job, bool) {
		if id == "abc123" {
			return j, true
		}
		return nil, false
	}, dir, nil, nil)

	if err := r.Hydrate(context.Background(), "abc123"); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if err := j.MarkArticleEmitted(0); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	r.Evict("abc123")
	if err := j.MarkArticleFailed(0); err != nil {
		t.Fatalf("MarkArticleFailed while evicted: %v", err)
	}

	if err := r.Hydrate(context.Background(), "abc123"); err != nil {
		t.Fatalf("re-Hydrate: %v", err)
	}
	if p := j.Progress(); !p.ArticleFailed(0) || p.ArticleEmitted(0) {
		t.Errorf("article 0 after re-hydration: failed=%v emitted=%v, want failed and not emitted",
			p.ArticleFailed(0), p.ArticleEmitted(0))
	}
	if got := j.FailedBytes(); got != 100 {
		t.Errorf("FailedBytes after re-hydration = %d, want 100", got)
	}
}

// TestAppResidency_HydrateUnknownJobErrors pins that a missing job is an error
// rather than a silent no-op: the dispatcher logs Residency failures
// (see logResidencyError — `git grep -n 'func (d \*Dispatcher) logResidencyError' internal/dispatch/`)
// and a silent success would strand a job
// at Fetching with nothing to fetch from.
func TestAppResidency_HydrateUnknownJobErrors(t *testing.T) {
	t.Parallel()
	r := newAppResidency(func(string) (*job.Job, bool) { return nil, false }, t.TempDir(), nil, nil)
	if err := r.Hydrate(context.Background(), "nope"); err == nil {
		t.Fatal("Hydrate of an unknown job must error")
	}
}

// TestAppResidency_HydrateRefusesAProgressRecordOfAnotherShape pins that a
// re-hydration whose manifest does not describe the job's own progress record
// fails rather than recomputing the record against the wrong file ranges.
func TestAppResidency_HydrateRefusesAProgressRecordOfAnotherShape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j := job.New("abc123", "test", job.PolicyFromPP(3))
	if err := j.AttachContent(job.NewManifest([]job.JobFile{{Subject: "x.rar", Bytes: 200, Articles: []job.JobArticle{
		{ID: "a1", Bytes: 100, Number: 1}, {ID: "a2", Bytes: 100, Number: 2},
	}}})); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	j.Evict()
	writeTestManifest(t, filepath.Join(dir, "abc123.json.gz"), j) // one article
	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, dir, nil, nil)

	if err := r.Hydrate(context.Background(), "abc123"); err == nil {
		t.Fatal("Hydrate attached a one-article manifest to a two-article progress record")
	}
	if j.Resident() {
		t.Error("the job is resident after a refused hydration")
	}
}

// TestAppResidency_HydrateWaitsForAHydrationInFlight pins the coalescing
// branch: a second Hydrate for a job already being hydrated waits for the
// first, is done if that one attached, and otherwise hydrates the job itself
// rather than reporting the other's failure as its own.
func TestAppResidency_HydrateWaitsForAHydrationInFlight(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	newJob := func() *job.Job {
		j := job.New("abc123", "test", job.PolicyFromPP(3))
		writeTestManifest(t, filepath.Join(dir, "abc123.json.gz"), j)
		return j
	}
	// inFlight plays a hydration of j in progress. ready is its channel; the
	// returned function ends the hydration as Hydrate's own defer does, so a
	// waiter that looks again finds none in flight.
	inFlight := func(j *job.Job) (*appResidency, chan struct{}, func()) {
		ready := make(chan struct{})
		r := &appResidency{
			lookup:    func(string) (*job.Job, bool) { return j, true },
			dir:       dir,
			hydrating: map[string]chan struct{}{"abc123": ready},
		}
		return r, ready, func() {
			r.mu.Lock()
			delete(r.hydrating, "abc123")
			close(ready)
			r.mu.Unlock()
		}
	}

	t.Run("the first succeeded", func(t *testing.T) {
		j := newJob()
		r, _, finish := inFlight(j)
		done := make(chan error, 1)
		go func() { done <- r.Hydrate(context.Background(), "abc123") }()
		m := job.NewManifest([]job.JobFile{{Subject: "test.rar", Bytes: 100, Articles: []job.JobArticle{{ID: "m1", Bytes: 100, Number: 1}}}})
		if err := j.AttachContent(m); err != nil {
			t.Fatalf("AttachContent: %v", err)
		}
		finish()
		if err := <-done; err != nil {
			t.Errorf("Hydrate after a successful hydration in flight: %v", err)
		}
	})

	t.Run("the first failed", func(t *testing.T) {
		j := newJob()
		r, ready, finish := inFlight(j)
		done := make(chan error, 1)
		go func() { done <- r.Hydrate(context.Background(), "abc123") }()
		// Production only closes ready; this send completes only once the
		// waiter is receiving on it, so the waiter has certainly waited —
		// a waiter that found no hydration in flight would hydrate the job
		// itself and pass whatever the waited path does.
		ready <- struct{}{}
		finish()
		if err := <-done; err != nil {
			t.Errorf("Hydrate = %v after the hydration it waited on left the job non-resident; the waiter hydrates it itself", err)
		}
		if !j.Resident() {
			t.Error("the job is not resident: the waiter did not hydrate it after the hydration it waited on failed")
		}
	})

	t.Run("the caller gives up", func(t *testing.T) {
		r, _, _ := inFlight(newJob())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := r.Hydrate(ctx, "abc123"); !errors.Is(err, context.Canceled) {
			t.Errorf("Hydrate with a cancelled context = %v, want context.Canceled", err)
		}
	})

	t.Run("a zero-value residency hydrates", func(t *testing.T) {
		j := newJob()
		r := &appResidency{lookup: func(string) (*job.Job, bool) { return j, true }, dir: dir}
		if err := r.Hydrate(context.Background(), "abc123"); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if !j.Resident() {
			t.Error("the job is not resident after Hydrate")
		}
	})
}

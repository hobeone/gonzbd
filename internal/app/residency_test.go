package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/job/jobtest"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// attachTestFiles gives j the one-file, one-article table writeTestManifest's
// manifest is built over. A job that already has a file table hydrates from
// its manifest alone, which is the shape these tests exercise with no record
// store.
func attachTestFiles(t *testing.T, j *job.Job) {
	t.Helper()
	if err := j.AttachFiles(job.NewFileTable([]job.FileSpec{{Subject: "test.rar", Bytes: 100, ArticleCount: 1}}), nil); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
}

func writeTestManifest(t *testing.T, path string, _ *job.Job) {
	t.Helper()
	content := `{"articles":[{"id":"m1","bytes":100,"number":1}]}`
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
	attachTestFiles(t, j)
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
		t.Fatal("re-hydration re-zeroed progress counters instead of preserving them via AttachArticles")
	}
}

// TestAppResidency_RehydrationKeepsAFailureRecordedWhileEvicted pins the
// hydration half of recording an evicted job's failure: Hydrate restores onto
// the job's own progress record, so the failed bit survives and is charged.
func TestAppResidency_RehydrationKeepsAFailureRecordedWhileEvicted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j := job.New("abc123", "test", job.PolicyFromPP(3))
	attachTestFiles(t, j)
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
	if err := j.MarkArticleFailed(0, job.WholeArticle); err != nil {
		t.Fatalf("MarkArticleFailed while evicted: %v", err)
	}

	if err := r.Hydrate(context.Background(), "abc123"); err != nil {
		t.Fatalf("re-Hydrate: %v", err)
	}
	if p := j.Progress(); !p.ArticleFailed(0) || !p.ArticleDone(0) {
		t.Errorf("article 0 after re-hydration: failed=%v done=%v, want both",
			p.ArticleFailed(0), p.ArticleDone(0))
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
		attachTestFiles(t, j)
		writeTestManifest(t, filepath.Join(dir, "abc123.json.gz"), j)
		return j
	}
	// inFlight plays a hydration of j in progress, holding its claim; the
	// returned function ends the hydration as Hydrate's own defer does, so a
	// waiter that looks again finds none in flight.
	inFlight := func(j *job.Job) (*appResidency, func()) {
		r := &appResidency{
			lookup:  func(string) (*job.Job, bool) { return j, true },
			dir:     dir,
			cancels: map[string]context.CancelFunc{"abc123": func() {}},
		}
		r.hydrating.TryClaim("abc123")
		return r, func() {
			r.mu.Lock()
			delete(r.cancels, "abc123")
			r.hydrating.Release("abc123")
			r.mu.Unlock()
		}
	}

	t.Run("the first succeeded", func(t *testing.T) {
		j := newJob()
		r, finish := inFlight(j)
		done := make(chan error, 1)
		go func() { done <- r.Hydrate(context.Background(), "abc123") }()
		m, err := job.NewManifestFromArticles(j.Files(), []job.JobArticle{{ID: "m1", Bytes: 100, Number: 1}})
		if err != nil {
			t.Fatalf("NewManifestFromArticles: %v", err)
		}
		if err := j.AttachArticles(m); err != nil {
			t.Fatalf("AttachArticles: %v", err)
		}
		finish()
		if err := <-done; err != nil {
			t.Errorf("Hydrate after a successful hydration in flight: %v", err)
		}
	})

	t.Run("the first failed", func(t *testing.T) {
		j := newJob()
		r, finish := inFlight(j)
		done := make(chan error, 1)
		go func() { done <- r.Hydrate(context.Background(), "abc123") }()
		// The waiter must be waiting before the hydration ends: one that
		// found no hydration in flight would hydrate the job itself and pass
		// whatever the waited path does.
		awaitHydrateWaiting(t, r)
		finish()
		if err := <-done; err != nil {
			t.Errorf("Hydrate = %v after the hydration it waited on left the job non-resident; the waiter hydrates it itself", err)
		}
		if !j.Resident() {
			t.Error("the job is not resident: the waiter did not hydrate it after the hydration it waited on failed")
		}
	})

	t.Run("the caller gives up", func(t *testing.T) {
		r, _ := inFlight(newJob())
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

// TestAppResidency_FaultAfterCancellationIsAContextError pins that a
// verification error returned after the hydration's context ended is reported
// as a context error even when it does not carry one itself, and parks
// nothing: Evict cancels a tick's hydration while the tick's context is live,
// and the tick settles a job Failed on a hydration error it cannot identify.
func TestAppResidency_FaultAfterCancellationIsAContextError(t *testing.T) {
	t.Parallel()
	j := job.New("abc123", "test", job.PolicyFromPP(3))
	parked := false
	r := &appResidency{parked: func(*job.Job, *storagefault.Fault) { parked = true }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	readErr := errors.New("read: file already closed")
	err := r.fault(ctx, j, storagefault.Classify("verify", "", readErr), readErr)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("fault after cancellation = %v, want it to wrap context.Canceled", err)
	}
	if errors.Is(err, dispatch.ErrResidencyFault) {
		t.Errorf("fault after cancellation = %v, want no residency fault", err)
	}
	if parked {
		t.Error("fault after cancellation parked the job")
	}
}

// awaitHydrateWaiting returns once r's own Hydrate is blocked in the latch
// Wait, and fails the test if it is not within five seconds. The match is on
// one goroutine's stack holding both frames, with r as Hydrate's printed
// receiver (exact, or marked ? when the traceback is unsure of it), so a
// parallel test's Hydrate waiting on another residency does not satisfy it.
// The dump is grown until runtime.Stack no longer fills the buffer, so the
// waiter cannot be cut off the end of it.
func awaitHydrateWaiting(t *testing.T, r *appResidency) {
	t.Helper()
	recv := fmt.Sprintf("(*appResidency).Hydrate(%p", r)
	buf := make([]byte, 1<<16)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		for g := range strings.SplitSeq(string(buf[:n]), "\n\n") {
			if strings.Contains(g, "keyedlatch.(*Latch).Wait(") &&
				(strings.Contains(g, recv+",") || strings.Contains(g, recv+"?,")) {
				return
			}
		}
		runtime.Gosched()
	}
	n := runtime.Stack(buf, true)
	t.Fatalf("r's Hydrate never waited on the latch; goroutines:\n%s", buf[:n])
}

// TestAppResidency_HydrateLeavesNoCancelBehind pins that Hydrate's deferred
// release drops the hydration's cancel along with its claim, on the return
// that attached and on one whose manifest read failed. A cancel left behind
// would sit in r.cancels for the life of the process, one per job ever
// hydrated.
func TestAppResidency_HydrateLeavesNoCancelBehind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	attached := job.New("abc123", "test", job.PolicyFromPP(3))
	attachTestFiles(t, attached)
	writeTestManifest(t, filepath.Join(dir, "abc123.json.gz"), attached)
	missing := job.New("def456", "test", job.PolicyFromPP(3)) // no manifest written
	attachTestFiles(t, missing)
	jobs := map[string]*job.Job{"abc123": attached, "def456": missing}
	r := newAppResidency(func(id string) (*job.Job, bool) {
		j, found := jobs[id]
		return j, found
	}, dir, nil, nil)

	for _, c := range []struct {
		id      string
		wantErr bool
	}{{"abc123", false}, {"def456", true}} {
		err := r.Hydrate(context.Background(), c.id)
		if (err != nil) != c.wantErr {
			t.Fatalf("Hydrate(%s) = %v, want an error: %v", c.id, err, c.wantErr)
		}
		r.mu.Lock()
		n, held := len(r.cancels), r.hydrating.Held(c.id)
		r.mu.Unlock()
		if n != 0 || held {
			t.Errorf("after Hydrate(%s): %d cancels left and claim held = %v; want 0 and false", c.id, n, held)
		}
	}
}

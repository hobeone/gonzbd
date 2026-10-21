package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestAppResidency_EvictEndsAHydrationBlockedInItsManifestRead pins that
// Evict cancels a hydration in flight rather than waiting on its reads (#811).
// The manifest is a FIFO holding half a gzip stream, so the hydration's read
// blocks mid-decode, as one on a stalled remote mount does; a removal's Evict
// must still return within a bound, and the hydration must report the
// cancellation and attach nothing.
func TestAppResidency_EvictEndsAHydrationBlockedInItsManifestRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := blockingManifest(t, dir, "abc123")

	j := job.New("abc123", "test", job.PolicyFromPP(3))
	attachTestFiles(t, j)
	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, dir, nil, nil)
	hydrated := make(chan error, 1)
	go func() { hydrated <- r.Hydrate(context.Background(), "abc123") }()
	waitDrained(t, w)

	evicted := make(chan struct{})
	go func() {
		r.Evict("abc123")
		close(evicted)
	}()
	select {
	case <-evicted:
	case <-time.After(5 * time.Second):
		t.Fatal("Evict did not return while the hydration's manifest read was blocked")
	}
	select {
	case err := <-hydrated:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Hydrate = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Hydrate did not return after Evict")
	}
	if j.Resident() {
		t.Error("the job is resident after its hydration was cancelled")
	}
}

// TestAppResidency_EvictCancelsTheHydrationThatReplacesTheOneItCancelled pins
// Evict's loop: a hydration that takes the place of the one Evict cancelled,
// before Evict looks again, is cancelled and waited for too, so none is in
// flight when Evict returns.
//
// The first hydration is a stand-in. Evict calls its cancel holding r.mu, and
// in that same critical section the cancel registers the second and ends the
// first, so the second is certainly in place before Evict looks again — the
// order a waiting Hydrate that wins r.mu first produces. The second is blocked
// in a real manifest read of a FIFO and ends only when its context is
// cancelled.
func TestAppResidency_EvictCancelsTheHydrationThatReplacesTheOneItCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := blockingManifest(t, dir, "abc123")
	j := job.New("abc123", "test", job.PolicyFromPP(3))
	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, dir, nil, nil)

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ready2 := make(chan struct{})
	second := make(chan error, 1)
	go func() {
		_, err := r.readArticles(ctx2, "abc123")
		second <- err
		r.mu.Lock()
		delete(r.cancels, "abc123")
		r.hydrating.Release("abc123")
		close(ready2)
		r.mu.Unlock()
	}()
	waitDrained(t, w)

	r.mu.Lock()
	r.hydrating.TryClaim("abc123")
	r.cancels["abc123"] = func() {
		// Evict holds r.mu here: end the first claim and take the second.
		r.hydrating.Release("abc123")
		r.hydrating.TryClaim("abc123")
		r.cancels["abc123"] = cancel2
	}
	r.mu.Unlock()

	evicted := make(chan struct{})
	go func() {
		r.Evict("abc123")
		close(evicted)
	}()
	select {
	case <-evicted:
	case <-time.After(5 * time.Second):
		t.Fatal("Evict did not return while the second hydration's manifest read was blocked")
	}
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the second hydration's read = %v, want context.Canceled", err)
		}
	default:
		t.Fatal("Evict returned while the hydration that replaced the one it cancelled was still in flight")
	}
	select {
	case <-ready2:
	default:
		t.Error("Evict returned before the second hydration ended")
	}
}

// A first hydration cancelled while it reads the manifest must leave the
// restored job unverified and not resident, so the next hydration verifies it.
func TestHydrate_CancelledDuringArticleReadAttachesNothing(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	orig, restored, a2 := restartedUnverified(t, env, "cancelread", 3, 2)
	mdir := manifestDir(env.admin)
	if err := os.Remove(filepath.Join(mdir, orig.ID()+".json.gz")); err != nil {
		t.Fatalf("remove the manifest: %v", err)
	}
	w := blockingManifest(t, mdir, orig.ID())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	hydrated := make(chan error, 1)
	go func() { hydrated <- a2.residency.Hydrate(ctx, restored.ID()) }()
	waitDrained(t, w)
	cancel()

	select {
	case err := <-hydrated:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Hydrate = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Hydrate did not return after its context was cancelled")
	}
	if restored.Resident() || restored.AllVerified() {
		t.Error("a hydration cancelled during the article read left the job resident or verified")
	}
}

// blockingManifest makes id's manifest in dir a FIFO holding half a gzip
// stream, so a read of it blocks mid-decode as one on a stalled remote mount
// does. It returns the FIFO's writer, opened O_RDWR so the open does not wait
// for a reader and a reader sees no EOF while it is open; closing it at
// cleanup ends any read left behind.
func blockingManifest(t *testing.T, dir, id string) *os.File {
	t.Helper()
	path := filepath.Join(dir, id+".json.gz")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	w, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open fifo: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(`{"articles":[{"id":"m1","bytes":100,"number":1}]}`)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if _, err := w.Write(buf.Bytes()[:buf.Len()/2]); err != nil {
		t.Fatalf("write fifo: %v", err)
	}
	return w
}

// waitDrained waits until the FIFO behind w is empty: its reader has read all
// it holds and is blocked waiting for more.
func waitDrained(t *testing.T, w *os.File) {
	t.Helper()
	fd := int(w.Fd())
	deadline := time.Now().Add(10 * time.Second)
	for {
		n, err := unix.IoctlGetInt(fd, unix.TIOCINQ)
		if err != nil {
			t.Fatalf("TIOCINQ: %v", err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the hydration never read the manifest")
		}
		time.Sleep(time.Millisecond)
	}
}

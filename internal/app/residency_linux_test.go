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
	path := filepath.Join(dir, "abc123.json.gz")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	// O_RDWR so the open does not wait for a reader, and the reader sees no
	// EOF while w is open. Closing it at cleanup ends the abandoned read.
	w, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open fifo: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(`{"files":[{"subject":"test.rar","bytes":100,"articles":[{"id":"m1","bytes":100,"number":1}]}]}`)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if _, err := w.Write(buf.Bytes()[:buf.Len()/2]); err != nil {
		t.Fatalf("write fifo: %v", err)
	}

	j := job.New("abc123", "test", job.PolicyFromPP(3))
	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, dir, nil, nil)
	hydrated := make(chan error, 1)
	go func() { hydrated <- r.Hydrate(context.Background(), "abc123") }()

	// Once the pipe is empty the hydration has read all it holds and is
	// blocked waiting for more.
	fd := int(w.Fd())
	deadline := time.Now().Add(10 * time.Second)
	for {
		n, err := unix.IoctlGetInt(fd, unix.TIOCINQ)
		if err != nil {
			t.Fatalf("TIOCINQ: %v", err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hydration never read the manifest")
		}
		time.Sleep(time.Millisecond)
	}

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

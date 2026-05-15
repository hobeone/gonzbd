package directunpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitFS_OpenPreRegistered(t *testing.T) {
	ctx := context.Background()
	wfs := NewWaitFS(ctx)

	// Create a real file for Open to return.
	dir := t.TempDir()
	vol := filepath.Join(dir, "movie.part01.rar")
	if err := os.WriteFile(vol, []byte("rar-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-register the volume.
	wfs.AddVolume("movie.part01.rar", vol)

	// Open should return immediately.
	f, err := wfs.Open(vol)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if info.Name() != "movie.part01.rar" {
		t.Errorf("expected movie.part01.rar, got %s", info.Name())
	}
}

func TestWaitFS_OpenAbsolutePath(t *testing.T) {
	ctx := context.Background()
	wfs := NewWaitFS(ctx)

	dir := t.TempDir()
	vol := filepath.Join(dir, "movie.part02.rar")
	if err := os.WriteFile(vol, []byte("rar-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	wfs.AddVolume("movie.part02.rar", vol)

	// Open with an absolute path — WaitFS should resolve by basename.
	f, err := wfs.Open("/some/other/path/movie.part02.rar")
	if err != nil {
		t.Fatalf("Open() with absolute path error: %v", err)
	}
	f.Close()
}

func TestWaitFS_OpenBlocksUntilAddVolume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wfs := NewWaitFS(ctx)

	dir := t.TempDir()
	vol := filepath.Join(dir, "movie.part02.rar")
	if err := os.WriteFile(vol, []byte("rar-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start Open in a goroutine — it should block.
	type result struct {
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := wfs.Open("/downloads/movie.part02.rar")
		if f != nil {
			f.Close()
		}
		ch <- result{err: err}
	}()

	// Ensure Open hasn't returned yet.
	select {
	case <-ch:
		t.Fatal("Open() returned before AddVolume()")
	case <-time.After(50 * time.Millisecond):
		// Good — still blocking.
	}

	// Now add the volume — should unblock Open.
	wfs.AddVolume("movie.part02.rar", vol)

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("Open() returned error after AddVolume: %v", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open() did not unblock after AddVolume()")
	}
}

func TestWaitFS_OpenContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	wfs := NewWaitFS(ctx)

	// Start Open for a volume that will never be added.
	ch := make(chan error, 1)
	go func() {
		f, err := wfs.Open("/downloads/movie.part99.rar")
		if f != nil {
			f.Close()
		}
		ch <- err
	}()

	// Let it block briefly.
	time.Sleep(50 * time.Millisecond)

	// Cancel the context — should unblock Open.
	cancel()

	select {
	case err := <-ch:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open() did not unblock after context cancel")
	}
}

func TestWaitFS_CloseUnblocksWaiters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wfs := NewWaitFS(ctx)

	// Start Open for a volume that will never be added.
	ch := make(chan error, 1)
	go func() {
		f, err := wfs.Open("/downloads/movie.part99.rar")
		if f != nil {
			f.Close()
		}
		ch <- err
	}()

	// Let it block briefly.
	time.Sleep(50 * time.Millisecond)

	// Close the WaitFS and cancel context.
	wfs.Close()
	cancel()

	select {
	case err := <-ch:
		if err == nil {
			t.Fatal("expected error from Open() after Close()")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open() did not unblock after Close()")
	}
}

func TestWaitFS_MultipleAddVolumes(t *testing.T) {
	ctx := context.Background()
	wfs := NewWaitFS(ctx)

	dir := t.TempDir()
	for i := 1; i <= 3; i++ {
		name := filepath.Join(dir, fmtPart(i))
		if err := os.WriteFile(name, []byte("rar"), 0o644); err != nil {
			t.Fatal(err)
		}
		wfs.AddVolume(fmtPart(i), name)
	}

	// All three should open immediately.
	for i := 1; i <= 3; i++ {
		f, err := wfs.Open(filepath.Join("/some/abs/path", fmtPart(i)))
		if err != nil {
			t.Fatalf("Open(%s) error: %v", fmtPart(i), err)
		}
		f.Close()
	}
}

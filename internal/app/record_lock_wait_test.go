package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestRecorder_WriterLockWaitEndsWithTheWaitersCtx: a flush whose store does
// not answer holds wmu with no deadline. A waiter for wmu, flush or apply,
// returns its own ctx's error while the holder is still blocked, and an apply
// that gave up purged nothing. handleFileUntrusted on the assembler's single
// worker is such a waiter, under untrustTimeout.
func TestRecorder_WriterLockWaitEndsWithTheWaitersCtx(t *testing.T) {
	for name, wait := range map[string]func(ctx context.Context, r *recorder, j *job.Job) error{
		"apply": func(ctx context.Context, r *recorder, j *job.Job) error {
			return r.apply(ctx, j, []durability.FileVerdict{{FileIdx: 0, DeleteAll: true, ClearComplete: true}})
		},
		"flush": func(ctx context.Context, r *recorder, _ *job.Job) error {
			return r.flush(ctx)
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := newModelStore(true, false)
			release := sync.OnceFunc(func() { close(st.release) })
			t.Cleanup(release)
			j := newTestJob(t, "id")
			r := newRecorder(st, func(string) *job.Job { return j }, slog.Default())
			r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Length: 10})

			holder := make(chan error, 1)
			go func() { holder <- r.flush(context.Background()) }()
			<-st.entered // the holder has wmu and is inside ApplyRecord
			r.noteWritten(j, durability.WrittenRow{FileIdx: 0, ArtIdx: 2, Length: 10})

			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			waiter := make(chan error, 1)
			go func() { waiter <- wait(ctx, r, j) }()
			select {
			case err := <-waiter:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("waiter returned %v, want its ctx's DeadlineExceeded", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the waiter for the writer lock outlived its ctx while the holder was blocked: the wait is not bounded by the waiter's ctx")
			}
			select {
			case err := <-holder:
				t.Fatalf("the holder finished (%v) before it was released; the test proved nothing", err)
			default:
			}
			r.mu.Lock()
			pending := len(r.pending[j])
			r.mu.Unlock()
			if pending != 1 {
				t.Errorf("pending rows = %d after the waiter gave up, want the 1 noted while it waited: a waiter that never took wmu touches no buffer", pending)
			}
			release()
			if err := <-holder; err != nil {
				t.Fatalf("holder flush: %v", err)
			}
		})
	}
}

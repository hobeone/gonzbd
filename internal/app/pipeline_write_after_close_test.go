package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestWriteArticle_AfterTheJobClosedCreatesNoFile, driven through a real
// assembler worker: a result registered its file while the job was still the
// dispatcher's, and its write reaches the worker after the job closed. No
// file is created and no storage fault is raised.
func TestWriteArticle_AfterTheJobClosedCreatesNoFile(t *testing.T) {
	t.Parallel()
	handOver := func(t *testing.T, _ *Application, p *pipeline, a *assembler.Assembler, j *job.Job) {
		t.Helper()
		p.handedOff = func(got *job.Job) bool { return got == j }
		if err := a.CloseJobHandles(t.Context(), j.ID()); err != nil {
			t.Fatalf("CloseJobHandles: %v", err)
		}
	}
	for _, tc := range []struct {
		name  string
		close func(t *testing.T, application *Application, p *pipeline, a *assembler.Assembler, j *job.Job)
		fatal bool
	}{
		{"removed from the dispatcher", func(t *testing.T, application *Application, _ *pipeline, _ *assembler.Assembler, j *job.Job) {
			t.Helper()
			if err := application.Dispatcher().RemoveJob(context.Background(), j); err != nil {
				t.Fatalf("RemoveJob: %v", err)
			}
		}, false},
		{"handed off after CloseJobHandles", handOver, false},
		{"FatalErr after the hand-over", handOver, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			application := newTestApplication(t)
			disp, j := helperJob(t, application, "write-after-close", 1, 1)
			p := helperPipeline(t, disp)
			var faults atomic.Int32
			a := assembler.New(assembler.Options{
				FileInfo:     p.resolveForWrite,
				OnWriteFault: func(string, int, *storagefault.Fault) { faults.Add(1) },
			}, slog.New(slog.DiscardHandler))
			if err := a.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = a.Stop() })
			p.assembler = a

			if err := p.registerFile(j, 0); err != nil {
				t.Fatalf("registerFile: %v", err)
			}
			info, err := p.resolveFileInfo(j.ID(), 0)
			if err != nil {
				t.Fatalf("resolveFileInfo: %v", err)
			}

			tc.close(t, application, p, a, j)

			res := &downloader.ArticleResult{Job: j, FileIdx: 0, ArtIdx: 0, MessageID: "write-after-close-f0-a0@x"}
			req := assembler.WriteRequest{Offset: 0, Data: []byte("late")}
			if tc.fatal {
				req = assembler.WriteRequest{FatalErr: errors.New("yenc: garbage")}
			}
			if err := a.WriteArticle(t.Context(), refFor(res), req); err != nil {
				t.Fatalf("WriteArticle: %v", err)
			}
			if err := a.Quiesce(t.Context()); err != nil {
				t.Fatalf("Quiesce: %v", err)
			}

			if _, err := os.Stat(info.Path()); !os.IsNotExist(err) {
				t.Errorf("os.Stat(%s) = %v, want not-exist: a write after the job closed created a file nothing will close", info.Path(), err)
			}
			if n := faults.Load(); n != 0 {
				t.Errorf("OnWriteFault fired %d times, want 0: a closed job is not a storage fault", n)
			}
		})
	}
}

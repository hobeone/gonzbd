package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/types"
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
		// enqueuePostProc forgets the job's file info after the close arm
		// (app.go, the forgetJob after closeJobHandles) while the job is still
		// the dispatcher's, so a write queued behind the arm misses the cache.
		{"forgotten after the hand-over", func(t *testing.T, application *Application, p *pipeline, a *assembler.Assembler, j *job.Job) {
			t.Helper()
			handOver(t, application, p, a, j)
			p.forgetJob(j.ID())
		}, false},
		// Application.RemoveJob's order: dispatcher removal, the cancel arm,
		// then forgetJob.
		{"forgotten after RemoveJob", func(t *testing.T, application *Application, p *pipeline, a *assembler.Assembler, j *job.Job) {
			t.Helper()
			if err := application.Dispatcher().RemoveJob(context.Background(), j); err != nil {
				t.Fatalf("RemoveJob: %v", err)
			}
			if err := a.CancelJob(t.Context(), j.ID(), assembler.KeepFiles); err != nil {
				t.Fatalf("CancelJob: %v", err)
			}
			p.forgetJob(j.ID())
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			application := newTestApplication(t)
			disp, j := helperJob(t, application, "write-after-close", 1, 1)
			p := helperPipeline(t, disp)
			var faults atomic.Int32
			a := assembler.New(assembler.Options{
				FileInfo:     p.resolveForWrite,
				OnWriteFault: func(assembler.Owner, int, *storagefault.Fault) { faults.Add(1) },
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

// TestWriteArticle_OfAnEarlierAttemptNeverReachesTheRetrysFile is #1056's
// acceptance, through a real assembler worker: attempt 1 has a file open,
// is removed and retried under the same ID, the retry opens the same file
// index, and then a write of attempt 1 that passed the handler's instance
// check before the removal reaches the worker. It lands nowhere: not in the
// retry's file, not in its part count.
func TestWriteArticle_OfAnEarlierAttemptNeverReachesTheRetrysFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fatal bool
	}{
		{"data", false},
		{"FatalErr", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			application := newTestApplication(t)
			const id = "feedface00105601"
			f := nzb.File{Subject: "file0.rar", Bytes: 2000}
			for i := range 2 {
				f.Articles = append(f.Articles, nzb.Article{ID: fmt.Sprintf("cross-f0-a%d@x", i), Number: i + 1, Bytes: 1000})
			}
			parsed := &nzb.NZB{Files: []nzb.File{f}}
			build := func(name string) *job.Job {
				j, hdr, err := BuildIngestJob(application.config, parsed, name+".nzb",
					types.FetchOptions{NzbName: name + ".nzb", JobID: id}, nil)
				if err != nil {
					t.Fatalf("BuildIngestJob(%s): %v", name, err)
				}
				if err := application.Dispatcher().Add(context.Background(), j, hdr); err != nil {
					t.Fatalf("Add(%s): %v", name, err)
				}
				return j
			}
			p := helperPipeline(t, application.Dispatcher())
			var written, completed atomic.Int32
			a := assembler.New(assembler.Options{
				FileInfo:         p.resolveForWrite,
				OnArticleWritten: func(assembler.Owner, int, int32, int64, int64, uint32) { written.Add(1) },
				OnFileComplete:   func(assembler.Owner, int) { completed.Add(1) },
			}, slog.New(slog.DiscardHandler))
			if err := a.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = a.Stop() })
			p.assembler = a
			write := func(j *job.Job, artIdx int32, req assembler.WriteRequest) {
				t.Helper()
				res := &downloader.ArticleResult{Job: j, FileIdx: 0, ArtIdx: artIdx, MessageID: fmt.Sprintf("cross-f0-a%d@x", artIdx)}
				if err := a.WriteArticle(t.Context(), refFor(res), req); err != nil {
					t.Fatalf("WriteArticle: %v", err)
				}
				if err := a.Quiesce(t.Context()); err != nil {
					t.Fatalf("Quiesce: %v", err)
				}
			}

			// Attempt 1 opens file 0.
			first := build("first")
			if err := p.registerFile(first, 0); err != nil {
				t.Fatalf("registerFile(first): %v", err)
			}
			write(first, 0, assembler.WriteRequest{Offset: 0, Data: []byte("AAAA")})

			// Application.RemoveJob's order: dispatcher removal, the cancel
			// arm, forgetJob. Then the retry registers under the same ID.
			if err := application.Dispatcher().Remove(context.Background(), id); err != nil {
				t.Fatalf("Remove(first): %v", err)
			}
			if err := a.CancelJob(t.Context(), id, assembler.KeepFiles); err != nil {
				t.Fatalf("CancelJob: %v", err)
			}
			p.forgetJob(id)
			second := build("second")
			if err := p.registerFile(second, 0); err != nil {
				t.Fatalf("registerFile(second): %v", err)
			}
			write(second, 0, assembler.WriteRequest{Offset: 0, Data: []byte("BBBB")})
			info, err := p.resolveFileInfo(id, 0)
			if err != nil {
				t.Fatalf("resolveFileInfo(second): %v", err)
			}
			if n := written.Load(); n != 2 {
				t.Fatalf("OnArticleWritten fired %d times before the straggler, want 2", n)
			}

			// The straggler: attempt 1's other article, with the retry's
			// handle open under the same key.
			req := assembler.WriteRequest{Offset: 4, Data: []byte("CCCC")}
			if tc.fatal {
				req = assembler.WriteRequest{FatalErr: errors.New("yenc: garbage")}
			}
			write(first, 1, req)

			if n := written.Load(); n != 2 {
				t.Errorf("OnArticleWritten fired %d times, want 2: the straggler was written", n)
			}
			if n := completed.Load(); n != 0 {
				t.Errorf("OnFileComplete fired %d times, want 0: the straggler counted toward the retry's file", n)
			}
			got, err := os.ReadFile(info.Path())
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", info.Path(), err)
			}
			if len(got) < 8 || string(got[:4]) != "BBBB" || string(got[4:8]) == "CCCC" {
				t.Errorf("the retry's file holds %q..., want BBBB and nothing of attempt 1's at offset 4", got[:min(len(got), 8)])
			}
		})
	}
}

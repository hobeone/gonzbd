package app

import (
	"context"
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// retriedUnderOneID registers an instance under id, removes it, and registers
// a second instance under the same id, the way RetryHistoryJob does after the
// finalizer removed the first. It returns both.
func retriedUnderOneID(t *testing.T, application *Application, id string) (first, second *job.Job) {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject: "file0.rar", Bytes: 2000,
		Articles: []nzb.Article{
			{ID: "stale-f0-a0@x", Number: 1, Bytes: 1000},
			{ID: "stale-f0-a1@x", Number: 2, Bytes: 1000},
		},
	}}}
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
	first = build("first")
	if err := application.Dispatcher().Remove(context.Background(), id); err != nil {
		t.Fatalf("Remove(first): %v", err)
	}
	second = build("second")
	return first, second
}

// TestHandleResult_DropsAResultFetchedForAnEarlierInstance: a fetch outlives
// its job's failure, finalization and retry, and its result arrives after a
// new instance is registered under the same ID. None of the pipeline's
// consumers may apply it to the new instance, which never dispatched it.
func TestHandleResult_DropsAResultFetchedForAnEarlierInstance(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		data []byte
	}{
		{name: "terminal failure", err: errors.New("yenc: garbage")},
		{name: "exhausted", err: downloader.ErrNoServersLeft},
		{name: "success", data: []byte("payload")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			application := newTestApplication(t)
			first, second := retriedUnderOneID(t, application, "feedface00659a01")
			p := helperPipeline(t, application.Dispatcher())
			p.assembler = assembler.New(assembler.Options{
				FileInfo: func(string, int) (assembler.FileInfo, error) {
					t.Error("the assembler resolved a file for a stale result")
					return assembler.FileInfo{}, errors.New("stale")
				},
			}, nil)
			if err := p.assembler.Start(t.Context()); err != nil {
				t.Fatalf("assembler.Start: %v", err)
			}
			t.Cleanup(func() { _ = p.assembler.Stop() })
			var hopeless bool
			p.onJobHopeless = func(string) { hopeless = true }
			var written bool
			p.onArticleWritten = func(string, int) { written = true }

			p.handleResult(t.Context(), &downloader.ArticleResult{
				Job:        first,
				FileIdx:    0,
				ArtIdx:     0,
				MessageID:  "stale-f0-a0@x",
				Subject:    "file0.rar",
				ServerName: "s1",
				Data:       tc.data,
				Err:        tc.err,
			})

			prog := second.Progress()
			if prog.ArticleFailed(0) {
				t.Error("the second instance's article 0 is marked failed by a result the first instance's fetch produced")
			}
			if got := prog.DownloadStarted(); !got.IsZero() {
				t.Errorf("the second instance's download-started = %v, credited by the first instance's fetch", got)
			}
			if got := prog.ServerStats()["s1"]; got != 0 {
				t.Errorf("the second instance's ServerStats[s1] = %d, credited by the first instance's fetch", got)
			}
			if _, err := p.resolveFileInfo(second.ID(), 0); err == nil {
				t.Error("a stale result registered the second instance's file")
			}
			if hopeless {
				t.Error("a stale result reached early-abort accounting")
			}
			if written {
				t.Error("a stale result reached the checkpoint cadence")
			}
		})
	}
}

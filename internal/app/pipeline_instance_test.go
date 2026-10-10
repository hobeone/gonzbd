package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// staleArticles is enough articles for Job.CheckEarlyAbort to be able to
// answer true: it needs ten resolved before it judges the failure rate.
const staleArticles = 11

// retriedUnderOneID registers an instance under id, removes it, and registers
// a second instance under the same id, the way RetryHistoryJob does after the
// finalizer removed the first. It returns both. The first instance is named
// "first" and the second "second", so a file path says which one registered
// it.
func retriedUnderOneID(t *testing.T, application *Application, id string) (first, second *job.Job) {
	t.Helper()
	f := nzb.File{Subject: "file0.rar", Bytes: staleArticles * 1000}
	for i := range staleArticles {
		f.Articles = append(f.Articles, nzb.Article{ID: fmt.Sprintf("stale-f0-a%d@x", i), Number: i + 1, Bytes: 1000})
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
	first = build("first")
	m, err := first.Manifest()
	if err != nil {
		t.Fatalf("Manifest(first): %v", err)
	}
	if err := application.Dispatcher().Remove(context.Background(), id); err != nil {
		t.Fatalf("Remove(first): %v", err)
	}
	// Removal evicted the first instance's manifest. Restored, so a result
	// that reached it would leave a mark the test can see.
	if err := first.RestoreContent(m); err != nil {
		t.Fatalf("RestoreContent(first): %v", err)
	}
	second = build("second")
	return first, second
}

// failAllButArticleZero marks every article of j but article 0 failed, so one
// more failed article reaches CheckEarlyAbort's sample with a failure rate
// over its threshold.
func failAllButArticleZero(t *testing.T, j *job.Job) {
	t.Helper()
	for i := 1; i < staleArticles-1; i++ {
		if err := j.MarkArticleFailed(i); err != nil {
			t.Fatalf("MarkArticleFailed(%d): %v", i, err)
		}
	}
}

// emit marks article 0 emitted on each of js, the way a dispatch of it would.
func emit(t *testing.T, js ...*job.Job) {
	t.Helper()
	for _, j := range js {
		if err := j.MarkArticleEmitted(0); err != nil {
			t.Fatalf("MarkArticleEmitted: %v", err)
		}
	}
}

// staleResultCase is one result a fetch for the first instance produces.
type staleResultCase struct {
	name string
	err  error
	data []byte
}

var staleResultCases = []staleResultCase{
	{name: "terminal failure", err: errors.New("yenc: garbage")},
	{name: "exhausted", err: downloader.ErrNoServersLeft},
	{name: "retryable failure", err: nntp.ErrNoArticle},
	{name: "success", data: []byte("payload")},
}

// assertProgressUntouched fails if anything the pipeline records for article
// 0 changed on j since the fixture set it up: it is still emitted, not
// failed, and no download was credited.
func assertProgressUntouched(t *testing.T, which string, j *job.Job) {
	t.Helper()
	prog := j.Progress()
	if prog.ArticleFailed(0) {
		t.Errorf("the %s instance's article 0 is marked failed by a stale result", which)
	}
	if !prog.ArticleEmitted(0) {
		t.Errorf("the %s instance's article 0 lost its emitted mark to a stale result", which)
	}
	if got := prog.DownloadStarted(); !got.IsZero() {
		t.Errorf("the %s instance's download-started = %v, credited by a stale result", which, got)
	}
	if got := prog.ServerStats()["s1"]; got != 0 {
		t.Errorf("the %s instance's ServerStats[s1] = %d, credited by a stale result", which, got)
	}
}

// TestHandleResult_DropsAResultFetchedForAnEarlierInstance: a fetch outlives
// its job's failure, finalization and retry, and its result arrives after a
// new instance is registered under the same ID. None of the pipeline's
// consumers may apply it, to either instance.
func TestHandleResult_DropsAResultFetchedForAnEarlierInstance(t *testing.T) {
	t.Parallel()
	for _, tc := range staleResultCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			application := newTestApplication(t)
			first, second := retriedUnderOneID(t, application, "feedface00659a01")
			// Both instances one failure short of an early abort, so the
			// stale result would trip it on whichever instance it reached.
			failAllButArticleZero(t, first)
			failAllButArticleZero(t, second)
			emit(t, first, second)

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

			assertProgressUntouched(t, "second", second)
			assertProgressUntouched(t, "first", first)
			if _, err := p.resolveFileInfo(second.ID(), 0); err == nil {
				t.Error("a stale result registered a file under the shared ID")
			}
			if hopeless {
				t.Error("a stale result reached early-abort accounting")
			}
		})
	}
}

// TestHandlers_ActOnTheResultsOwnInstance pins the handlers behind the gate:
// once handleResult has admitted a result, they act on res.Job rather than
// looking its ID up again. Each is called directly here with a result for an
// instance that is no longer registered, which the gate would have dropped,
// so a by-ID lookup would find the later instance and act on it instead.
//
// The assembler is never started, so every write fails and each path's
// return-to-pool clear runs.
func TestHandlers_ActOnTheResultsOwnInstance(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		fileIdx int
		err     error
		data    []byte
		check   func(t *testing.T, p *pipeline, first, second *job.Job, hopeless bool)
	}{
		{name: "terminal failure", err: errors.New("yenc: garbage"),
			check: func(t *testing.T, p *pipeline, first, second *job.Job, hopeless bool) {
				if !first.Progress().ArticleFailed(0) {
					t.Error("the result's own instance's article 0 is not marked failed")
				}
				if second.Progress().ArticleFailed(0) {
					t.Error("the registered instance's article 0 is marked failed by a result for another instance")
				}
				if !hopeless {
					t.Error("early abort did not fire although the result's own instance crossed its threshold")
				}
				assertRegisteredBy(t, p, first.ID(), "first")
			}},
		{name: "retryable failure", err: nntp.ErrNoArticle},
		{name: "success", data: []byte("payload"),
			check: func(t *testing.T, p *pipeline, first, second *job.Job, _ bool) {
				if first.Progress().DownloadStarted().IsZero() {
					t.Error("the result's own instance's download-started was not recorded")
				}
				if got := first.Progress().ServerStats()["s1"]; got != 7 {
					t.Errorf("the result's own instance's ServerStats[s1] = %d, want 7", got)
				}
				if got := second.Progress().DownloadStarted(); !got.IsZero() {
					t.Errorf("the registered instance's download-started = %v, credited by a result for another instance", got)
				}
				if got := second.Progress().ServerStats()["s1"]; got != 0 {
					t.Errorf("the registered instance's ServerStats[s1] = %d, credited by a result for another instance", got)
				}
				assertRegisteredBy(t, p, first.ID(), "first")
			}},
		{name: "success for a file that cannot be registered", fileIdx: 9, data: []byte("payload")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			application := newTestApplication(t)
			first, second := retriedUnderOneID(t, application, "feedface00659a02")
			failAllButArticleZero(t, first)
			emit(t, first, second)

			p := helperPipeline(t, application.Dispatcher())
			p.assembler = assembler.New(assembler.Options{FileInfo: p.resolveFileInfo}, nil)
			var hopeless bool
			p.onJobHopeless = func(string) { hopeless = true }

			res := &downloader.ArticleResult{
				Job: first, FileIdx: tc.fileIdx, ArtIdx: 0, MessageID: "stale-f0-a0@x",
				Subject: "file0.rar", ServerName: "s1", Data: tc.data, Err: tc.err,
			}
			if tc.err != nil {
				p.handleFailureResult(t.Context(), res)
			} else {
				p.handleSuccessResult(t.Context(), res)
			}

			if first.Progress().ArticleEmitted(0) {
				t.Error("the result's own instance's article 0 is still marked emitted after it was returned to the pool")
			}
			if !second.Progress().ArticleEmitted(0) {
				t.Error("the registered instance's article 0 lost its emitted mark to a result for another instance")
			}
			if tc.check != nil {
				tc.check(t, p, first, second, hopeless)
			}
		})
	}
}

// TestIsCurrent answers for the registered instance only: not for an earlier
// instance under the same ID, and not for an ID nothing is registered under.
func TestIsCurrent(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	first, second := retriedUnderOneID(t, application, "feedface00659a03")
	p := helperPipeline(t, application.Dispatcher())

	for _, tc := range []struct {
		name string
		j    *job.Job
		want bool
	}{
		{"the registered instance", second, true},
		{"an earlier instance under the same ID", first, false},
		{"an instance under an unregistered ID", job.New("feedface00659a04", "gone", job.Policy{}), false},
	} {
		if got := p.isCurrent(&downloader.ArticleResult{Job: tc.j}); got != tc.want {
			t.Errorf("isCurrent(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// assertRegisteredBy fails unless file 0 under id was registered from the
// instance named name: registerFile puts the file in a directory named after
// the job, and the two instances carry different names.
func assertRegisteredBy(t *testing.T, p *pipeline, id, name string) {
	t.Helper()
	info, err := p.resolveFileInfo(id, 0)
	if err != nil {
		t.Fatalf("resolveFileInfo: %v", err)
	}
	if dir := filepath.Base(filepath.Dir(info.Path)); !strings.EqualFold(dir, name) {
		t.Errorf("file 0 was registered in %q, want the %q instance's directory", info.Path, name)
	}
}

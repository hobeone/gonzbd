package app

import (
	"errors"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

func newDownloadDirApp(t *testing.T) (*Application, string) {
	t.Helper()
	application, err := New(testConfigInternal(t, t.TempDir()), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return application, application.downloadDir()
}

func queueOneJob(t *testing.T, application *Application) string {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "a.bin",
		Bytes:    10,
		Articles: []nzb.Article{{Bytes: 10, ID: "a@example.com", Number: 1}},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, "a.nzb", types.FetchOptions{NzbName: "queued", PP: 3}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.AddJob(t.Context(), j, hdr, nil, false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	return j.ID()
}

// pipelineDownloadDir reads the pipeline's copy of the base under its lock.
func pipelineDownloadDir(application *Application) string {
	application.pipeline.mu.Lock()
	defer application.pipeline.mu.Unlock()
	return application.pipeline.downloadDir
}

func TestSetDownloadDir_RefusedWhileAJobIsUnfinished(t *testing.T) {
	t.Parallel()
	application, old := newDownloadDirApp(t)
	queueOneJob(t, application)

	err := application.SetDownloadDir(t.TempDir())
	if !errors.Is(err, ErrDownloadDirBusy) {
		t.Fatalf("SetDownloadDir with a queued job = %v, want ErrDownloadDirBusy", err)
	}
	if got := application.downloadDir(); got != old {
		t.Errorf("config download_dir = %q after a refusal, want the old %q", got, old)
	}
	if got := pipelineDownloadDir(application); got != old {
		t.Errorf("pipeline download dir = %q after a refusal, want the old %q", got, old)
	}
}

func TestSetDownloadDir_AppliesToConfigAndPipelineWhenQueueIsEmpty(t *testing.T) {
	t.Parallel()
	application, _ := newDownloadDirApp(t)
	next := t.TempDir()

	if err := application.SetDownloadDir(next); err != nil {
		t.Fatalf("SetDownloadDir on an empty queue: %v", err)
	}
	if got := application.downloadDir(); got != next {
		t.Errorf("config download_dir = %q, want %q", got, next)
	}
	if got := pipelineDownloadDir(application); got != next {
		t.Errorf("pipeline download dir = %q, want %q", got, next)
	}
}

func TestSetDownloadDir_SameValueSucceedsWithAJobQueued(t *testing.T) {
	t.Parallel()
	application, old := newDownloadDirApp(t)
	queueOneJob(t, application)

	if err := application.SetDownloadDir(old); err != nil {
		t.Fatalf("SetDownloadDir to the value already in force: %v", err)
	}
}

func TestSetDownloadDir_SucceedsWhenEveryJobIsSettled(t *testing.T) {
	t.Parallel()
	application, _ := newDownloadDirApp(t)
	id := queueOneJob(t, application)
	j, ok := application.Dispatcher().Job(id)
	if !ok {
		t.Fatal("queued job is not registered")
	}
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if _, err := j.Finish(job.OutcomeFailed, time.Now()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	next := t.TempDir()

	if err := application.SetDownloadDir(next); err != nil {
		t.Fatalf("SetDownloadDir with only a settled job registered: %v", err)
	}
	if got := application.downloadDir(); got != next {
		t.Errorf("config download_dir = %q, want %q", got, next)
	}
}

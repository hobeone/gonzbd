package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// collectingRunner holds a launched job without reporting, and on the launch
// at Assessing takes the job's DirectUnpacker from the orchestrator, as
// enqueuePostProc does when that launch hands the job to post-processing.
type collectingRunner struct {
	app       *Application
	fetching  chan string
	collected chan *directunpack.DirectUnpacker
}

func (r collectingRunner) Run(_ context.Context, id string, state job.State) {
	switch state {
	case job.Fetching:
		r.fetching <- id
	case job.Assessing:
		r.collected <- r.app.duOrch.collect(id)
	default:
	}
}

// TestCompleteFinalizedFile_FeedsTheLastVolumeBeforeReportingTheDownload: the
// last file's completion reports the download finished, and from that report
// the tick can launch the job's post-processing, which collects its
// DirectUnpacker. The unpacker must already have the last volume by then: a
// collect ahead of the feed waits on an unpacker that never gets the volume,
// and the feed starts a second unpacker that nothing collects.
func TestCompleteFinalizedFile_FeedsTheLastVolumeBeforeReportingTheDownload(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	application.config.With(func(c *config.Config) {
		c.PostProc.DirectUnpack = true
		c.PostProc.EnableUnrar = true
	})
	application.ctx = t.Context()
	runner := collectingRunner{
		app:       application,
		fetching:  make(chan string, 1),
		collected: make(chan *directunpack.DirectUnpacker, 1),
	}
	d := dispatch.New(
		1, 1, 10*time.Millisecond, time.Now,
		&appWorkers{app: application},
		application.residency,
		dispatchstore.New(repo.DB()),
		runner,
	)
	application.dispatcher = d
	application.pipeline.dispatcher = d
	t.Cleanup(application.duOrch.abortAll)

	const volumes = 10
	parsed := &nzb.NZB{}
	data := make([][]byte, volumes)
	for i := range volumes {
		name := fmt.Sprintf("multi_new.part%02d.rar", i+1)
		b, err := os.ReadFile(filepath.Join("../unpack/testdata", name))
		if err != nil {
			t.Fatalf("read test archive: %v", err)
		}
		data[i] = b
		parsed.Files = append(parsed.Files, nzb.File{
			Subject:  name,
			Bytes:    int64(len(b)),
			Articles: []nzb.Article{{ID: fmt.Sprintf("v%d@t", i), Bytes: len(b), Number: 1}},
		})
	}
	j, hdr, err := BuildIngestJob(application.config, parsed, "lastvol.nzb", types.FetchOptions{NzbName: "lastvol"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	hdr.PP = 3
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	id := j.ID()
	if err := d.Add(t.Context(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	select {
	case <-runner.fetching:
	case <-time.After(10 * time.Second):
		t.Fatal("the job was never launched at Fetching")
	}

	dir := filepath.Join(application.config.GetGeneral().DownloadDir, j.Name())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for i, f := range parsed.Files {
		path := filepath.Join(dir, f.Subject)
		if err := os.WriteFile(path, data[i], 0o600); err != nil {
			t.Fatal(err)
		}
		application.pipeline.mu.Lock()
		application.pipeline.fileInfo[fileKey{jobID: id, fileIdx: i}] = assembler.FileInfo{Path: path}
		application.pipeline.mu.Unlock()
	}

	// The hook runs once, on the last file: it holds the completion until the
	// launch the report caused has collected the unpacker.
	var collected *directunpack.DirectUnpacker
	application.downloadReportedHook = func(string) {
		select {
		case collected = <-runner.collected:
		case <-time.After(10 * time.Second):
			t.Error("the download report never launched the job at Assessing")
		}
	}
	for i := range volumes {
		if err := application.completeFinalizedFile(t.Context(), FileComplete{JobID: id, FileIdx: i}); err != nil {
			t.Fatalf("completeFinalizedFile(%d): %v", i, err)
		}
	}
	if collected == nil {
		t.Fatal("the launch at Assessing collected no DirectUnpacker")
	}
	t.Cleanup(collected.Abort)
	// Settle the held launch, so the dispatcher's Stop does not wait on it.
	if err := d.Finished(id, job.OutcomeFailed); err != nil {
		t.Errorf("Finished: %v", err)
	}

	if _, ok := application.duOrch.status(id); ok {
		t.Error("an unpacker was started after post-processing collected the job's: " +
			"the last volume went to an unpacker nothing collects")
	}
	select {
	case <-collected.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("the collected DirectUnpacker never finished: it did not get the last volume (status %+v)",
			collected.Status())
	}
	if len(collected.Results()) == 0 {
		t.Errorf("the collected DirectUnpacker extracted nothing; failures %v", collected.Failures())
	}
}

package app

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// retryFixtureNZB renders a minimal NZB with one file of nArticles articles.
func retryFixtureNZB(nArticles int) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="iso-8859-1" ?>` + "\n")
	b.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	b.WriteString(`<file poster="p@t" date="1700000000" subject="&quot;file.bin&quot; yEnc (1/1)">` + "\n")
	b.WriteString("<groups><group>alt.bin.test</group></groups>\n<segments>\n")
	for i := 1; i <= nArticles; i++ {
		fmt.Fprintf(&b, `<segment bytes="1024" number="%d">a%d@t</segment>`+"\n", i, i)
	}
	b.WriteString("</segments>\n</file>\n</nzb>\n")
	return []byte(b.String())
}

// writeRetryNZBBackup gzips raw to adminDir/nzb/<name>, which is where
// rebuildJobFromNZB looks for it.
func writeRetryNZBBackup(t *testing.T, adminDir, name string, raw []byte) {
	t.Helper()
	nzbDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbDir, 0o750); err != nil {
		t.Fatalf("mkdir nzb: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nzbDir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write gz nzb: %v", err)
	}
}

// failJobIntoHistory runs a real job through persistAndCommit as FAILED,
// which keeps the job_files and written_articles rows the retry reads.
func failJobIntoHistory(t *testing.T, application *Application, job *job.Job, nArticles int) {
	t.Helper()
	adminDir := application.config.GetGeneral().AdminDir
	writeRetryNZBBackup(t, adminDir, job.ID()+".nzb.gz", retryFixtureNZB(nArticles))

	entry := history.Entry{
		NzoID:     job.ID(),
		Name:      job.Name(),
		NzbName:   job.ID() + ".nzb",
		NZBBackup: job.ID() + ".nzb.gz",
		Status:    string(constants.StatusFailed),
	}
	if err := application.TriggerPersistAndCommit(slog.Default(), entry, &postproc.Job{Job: job}); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}
	if nw, nf := durabilityRowCounts(t, application, job.ID()); nw != 1 || nf != 1 {
		t.Fatalf("fixture: persistAndCommit left %d written rows and %d job_files rows, want 1 and 1", nw, nf)
	}
}

// TestRetryHistoryJob_DiscardsRowsWhenTheManifestShapeChanged: the rows are
// keyed on article index and a retry re-parses the NZB backup, so if it comes
// back a different shape a row names an article that is no longer at that
// index. The retry deletes every row of the job rather than read any of them.
func TestRetryHistoryJob_DiscardsRowsWhenTheManifestShapeChanged(t *testing.T) {
	t.Parallel()
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, job.ID())
	failJobIntoHistory(t, application, job, nArticles)

	// Swap the backup for one of a different shape.
	adminDir := application.config.GetGeneral().AdminDir
	writeRetryNZBBackup(t, adminDir, job.ID()+".nzb.gz", retryFixtureNZB(nArticles+2))

	if err := application.RetryHistoryJob(t.Context(), job.ID()); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	if nw, _ := durabilityRowCounts(t, application, job.ID()); nw != 0 {
		t.Errorf("the retry kept %d written rows against a manifest whose "+
			"shape changed; they now describe articles that are somewhere else", nw)
	}
}

// TestRetryHistoryJob_AfterDownloadDirDeleted: a user who deleted a failed
// job's download directory and then retries it has nothing on disk for the
// recorded rows to describe, so the retry drops them and refetches. A
// hydration parks on the same missing directory (an unmounted share), but a
// retry is a user's explicit act on a download root that exists.
func TestRetryHistoryJob_AfterDownloadDirDeleted(t *testing.T) {
	t.Parallel()
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, job.ID())
	// seedDurability's job_files row has no filename, which the verifier
	// deletes without opening anything; name the file so it has to be read.
	err := realStore(t, application).ApplyRecord(t.Context(), []durability.RecordBatch{{
		JobID: job.ID(),
		Files: []durability.FileState{{FileIdx: 0, Filename: "file.bin"}},
	}})
	if err != nil {
		t.Fatalf("name the file: %v", err)
	}
	failJobIntoHistory(t, application, job, nArticles)

	jobDir := filepath.Join(application.downloadDir(), job.Name())
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "file.bin"), make([]byte, 100), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.RemoveAll(jobDir); err != nil {
		t.Fatalf("delete the job directory: %v", err)
	}

	if err := application.RetryHistoryJob(t.Context(), job.ID()); err != nil {
		t.Fatalf("RetryHistoryJob after the download directory was deleted: %v", err)
	}
	if _, queued := application.dispatcher.Job(job.ID()); !queued {
		t.Error("the retry succeeded but the job is not in the dispatcher")
	}
	if nw, _ := durabilityRowCounts(t, application, job.ID()); nw != 0 {
		t.Errorf("the retry kept %d written rows for a file whose directory is gone", nw)
	}
	rj, _ := application.dispatcher.Job(job.ID())
	if rj != nil && rj.Progress() != nil && rj.Progress().FileComplete(0) {
		t.Error("the retried file is marked complete although its bytes are gone")
	}
}

// TestRetryHistoryJob_AbortsWhenStaleRowsCannotBeDropped is the other half of
// the shape check: deciding to drop the stale rows is not dropping them. A
// retry that carried on would enqueue the job over rows that name articles
// somewhere else, so a failed delete aborts the retry before the job is
// queued.
func TestRetryHistoryJob_AbortsWhenStaleRowsCannotBeDropped(t *testing.T) {
	t.Parallel()
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, job.ID())
	failJobIntoHistory(t, application, job, nArticles)

	// A different shape, so the retry deletes every row.
	adminDir := application.config.GetGeneral().AdminDir
	writeRetryNZBBackup(t, adminDir, job.ID()+".nzb.gz", retryFixtureNZB(nArticles+2))

	application.recorder.st = failRecordFor{recordStore: application.recorder.st, id: job.ID()}

	if _, queued := application.dispatcher.Job(job.ID()); queued {
		t.Fatalf("fixture: job %s is still in the dispatcher before the retry, so the "+
			"post-abort queue assertion would be vacuous", job.ID())
	}

	err := application.RetryHistoryJob(t.Context(), job.ID())
	if err == nil {
		t.Fatal("the retry reported success while the stale rows it decided to drop are still in place")
	}
	if !errors.Is(err, ErrRecordForJob) {
		t.Errorf("error does not wrap the cause: got %v, want it to wrap %v", err, ErrRecordForJob)
	}
	if _, queued := application.dispatcher.Job(job.ID()); queued {
		t.Error("the retry aborted but still enqueued the job")
	}
	if nf, _ := durabilityRowCounts(t, application, job.ID()); nf == 0 {
		t.Error("fixture: the rows are gone, so the failing store was never the one that deleted them")
	}
}

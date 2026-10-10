package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
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
	if nf, ne := durabilityRowCounts(t, application, job.ID()); nf != 1 || ne != 0 {
		t.Fatalf("fixture: persistAndCommit left %d written rows and %d failed rows, want 1 and 0", nf, ne)
	}
}

// TestRetryHistoryJob_ClearsTheFailedArticlesItJustReset pins that a retry
// re-attempts the articles that failed, even when failed_articles rows
// outlived the failed departure that should have reclaimed them (#561). The
// retry reclaims them, and the article it exists to re-attempt is Outstanding.
func TestRetryHistoryJob_ClearsTheFailedArticlesItJustReset(t *testing.T) {
	t.Parallel()
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	// Article 0 has a written row; article 1 is permanently failed.
	seedDurability(t, application, job.ID())
	failJobIntoHistory(t, application, job, nArticles)
	// The stray row: what a late write leaves after the departure's reclaim.
	if _, err := application.historyRepo.DB().ExecContext(t.Context(),
		`INSERT INTO failed_articles (job_id, art_idx) VALUES (?, 1)`, job.ID()); err != nil {
		t.Fatal(err)
	}

	if err := application.RetryHistoryJob(t.Context(), job.ID()); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	if _, ne := durabilityRowCounts(t, application, job.ID()); ne != 0 {
		t.Errorf("%d failed-article rows survive the retry", ne)
	}
	j, ok := application.dispatcher.Job(job.ID())
	if !ok {
		t.Fatal("job not in dispatcher")
	}
	var outstanding []int32
	j.ForEachUnfinishedArticle(func(_ int, artIdx int32, _ string, _ int, _ int, _ string) bool {
		outstanding = append(outstanding, artIdx)
		return true
	})
	if !slices.Contains(outstanding, 1) {
		t.Errorf("article 1 is not Outstanding after the retry (outstanding = %v); "+
			"it is the article the retry was asked to re-attempt", outstanding)
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

	nf, ne := durabilityRowCounts(t, application, job.ID())
	if nf != 0 || ne != 0 {
		t.Errorf("the retry kept %d written rows and %d failed rows against a manifest whose "+
			"shape changed; they now describe articles that are somewhere else", nf, ne)
	}
}

// failingReclaimStore delegates everything to the real store except Reclaim.
// Embedding the interface keeps the stub honest: a retry that grows a call to
// another store method gets the real one.
type failingReclaimStore struct {
	durabilityStore
	err error
}

func (f failingReclaimStore) Reclaim(context.Context, string, ...string) error { return f.err }

// TestRetryHistoryJob_AbortsWhenStaleFailedMarksCannotBeCleared: a retry whose
// reclaim of stray failed_articles rows fails aborts, rather than enqueue the
// job over rows it decided to remove.
func TestRetryHistoryJob_AbortsWhenStaleFailedMarksCannotBeCleared(t *testing.T) {
	t.Parallel()
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, job.ID())
	failJobIntoHistory(t, application, job, nArticles)

	wantErr := errors.New("database is locked")
	application.durable = failingReclaimStore{durabilityStore: application.durable, err: wantErr}

	err := application.RetryHistoryJob(t.Context(), job.ID())
	if err == nil {
		t.Fatal("the retry reported success while the stale failed marks it decided to clear are still in place")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error does not wrap the cause: got %v, want it to wrap %v", err, wantErr)
	}
	if _, queued := application.dispatcher.Job(job.ID()); queued {
		t.Error("the retry aborted but still enqueued the job")
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

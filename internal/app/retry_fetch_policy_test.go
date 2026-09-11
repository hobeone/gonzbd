package app_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
)

// retryNZBWithRecoveryVolume renders a two-file NZB: a payload file with
// nPayload articles, plus a par2 recovery volume (".volNNN+MM.par2", which
// job.IsRecoveryVolume matches) with nRecovery articles — so BuildIngestJob
// classifies the second file as a deferrable recovery volume and a retry has
// a fetch policy to re-derive.
func retryNZBWithRecoveryVolume(nPayload, nRecovery int) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="iso-8859-1" ?>` + "\n")
	b.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	writeNZBFile(&b, "payload.bin", "p", nPayload)
	writeNZBFile(&b, "payload.vol000+01.par2", "r", nRecovery)
	b.WriteString("</nzb>\n")
	return []byte(b.String())
}

// writeNZBFile appends one <file> element carrying nArticles segments, whose
// message-IDs are namespaced by idPrefix so two files in the same NZB never
// collide on article ID.
func writeNZBFile(b *strings.Builder, filename, idPrefix string, nArticles int) {
	fmt.Fprintf(b, `<file poster="p@t" date="1700000000" subject="&quot;%s&quot; yEnc (1/%d)">`+"\n", filename, nArticles)
	b.WriteString("<groups><group>alt.bin.test</group></groups>\n<segments>\n")
	for i := 1; i <= nArticles; i++ {
		fmt.Fprintf(b, `<segment bytes="1024" number="%d">%s%d@t</segment>`+"\n", i, idPrefix, i)
	}
	b.WriteString("</segments>\n</file>\n")
}

// recoveryFileIndex returns the one manifest index FileIsPar2Recovery
// reports true for, failing the test if there is not exactly one.
func recoveryFileIndex(t *testing.T, m *job.Manifest) int {
	t.Helper()
	idx := -1
	for i := range m.NumFiles() {
		if m.FileIsPar2Recovery(i) {
			if idx != -1 {
				t.Fatalf("manifest has more than one recovery volume: %d and %d", idx, i)
			}
			idx = i
		}
	}
	if idx == -1 {
		t.Fatal("manifest has no recovery volume — fixture is wrong")
	}
	return idx
}

// seedJobFilesRow inserts a job_files row directly, standing in for the row
// a previously FAILED attempt left behind: job_finalizer.go's
// shouldDeleteDurability keeps job_files for a failed job rather than
// deleting it, so this is the state a retry actually finds on disk.
func seedJobFilesRow(t *testing.T, db *sql.DB, jobID string, fileIndex int, complete bool, fetch job.FetchPolicy) {
	t.Helper()
	c := 0
	if complete {
		c = 1
	}
	if _, err := db.Exec(
		`INSERT INTO job_files (job_id, file_index, complete, filename, assembled_crc32, fetch_policy)
		 VALUES (?, ?, ?, '', 0, ?)`,
		jobID, fileIndex, c, int(fetch)); err != nil {
		t.Fatalf("seed job_files row: %v", err)
	}
}

// seedHistoryJobFilesRow inserts a history_job_files row — the retained
// per-file progress historyFileProgress reads to build RetryHistoryJob's
// overlay. articleCount must match the re-parsed NZB's file range or
// retainedMatchesManifest rejects the whole overlay before RestoreFileMeta
// ever runs.
//
// fetch is a parameter, not a hardcoded 0, and that is load-bearing: this is
// the ONLY table the retry path reads a policy from. An earlier draft of this
// helper wrote 0 unconditionally, which made the configuration-honoured case
// below inert — with on-demand par2 off the derived policy is also
// FetchAlways, so the assertion agreed with the pre-fix defect and passed
// against it. Seeding job_files instead does not substitute: nothing reads
// that table before an eviction.
func seedHistoryJobFilesRow(
	t *testing.T, db *sql.DB, jobID string, fileIndex int,
	complete bool, articleCount int, fetch job.FetchPolicy,
) {
	t.Helper()
	c := 0
	if complete {
		c = 1
	}
	if _, err := db.Exec(
		`INSERT INTO history_job_files (job_id, file_index, complete, filename, assembled_crc32, article_count, fetch_policy)
		 VALUES (?, ?, ?, '', 0, ?, ?)`,
		jobID, fileIndex, c, articleCount, int(fetch)); err != nil {
		t.Fatalf("seed history_job_files row: %v", err)
	}
}

// TestRetryHistoryJob_ConfigurationIsHonoured pins that a retry derives its
// fetch policy from the CURRENT config, not from whatever a previous attempt
// left behind. On-demand par2 was on when the failed attempt ran — its
// history_job_files row holds a discarded FetchNever on the recovery volume —
// and is off now, so the retry must fetch the volume unconditionally rather
// than keep honouring a policy the user has since disabled (#329, case a).
//
// history_job_files is the table that matters here: it is what
// historyFileProgress reads. The job_files rows seeded below are the state a
// retry finds on disk and are what Task 3b's seed-and-flush corrects, but
// nothing reads them before an eviction, so they cannot carry this leak.
func TestRetryHistoryJob_ConfigurationIsHonoured(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newRetryTestApp(t)
	application.Config().With(func(c *config.Config) { c.Downloads.OnDemandPar2 = false })

	writeGzNZB(t, adminDir, "cfgoff.nzb.gz", retryNZBWithRecoveryVolume(2, 1))

	const id = "retrycfghonoured1"
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     id,
		Name:      "cfgoff",
		NzbName:   "cfgoff.nzb",
		NZBBackup: "cfgoff.nzb.gz",
		Status:    string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	seedHistoryJobFilesRow(t, repo.DB(), id, 0, false, 2, job.FetchAlways)
	seedHistoryJobFilesRow(t, repo.DB(), id, 1, false, 1, job.FetchNever)
	seedJobFilesRow(t, repo.DB(), id, 0, false, job.FetchAlways)
	seedJobFilesRow(t, repo.DB(), id, 1, false, job.FetchNever)

	if err := application.RetryHistoryJob(t.Context(), id); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	j, ok := application.Dispatcher().Job(id)
	if !ok {
		t.Fatal("retried job is not in the queue")
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	idx := recoveryFileIndex(t, m)
	if got := j.Progress().FileFetchPolicy(idx); got != job.FetchAlways {
		t.Errorf("FileFetchPolicy(%d) = %v after a retry with on-demand par2 off, want FetchAlways — "+
			"the retained FetchNever from history_job_files leaked through instead of the re-derived policy", idx, got)
	}
}

// TestRetryHistoryJob_PriorRulingDoesNotSurvive pins that a par2 verdict from
// the failed attempt (a damage release that promoted the recovery volume to
// FetchAlways) is not carried into the retry. On-demand par2 stays on, so the
// retry must re-derive FetchIfNeeded and re-evaluate the volume against
// whatever the retry actually damages, rather than trust a ruling made
// against contents the retry is about to change (#329, case b).
func TestRetryHistoryJob_PriorRulingDoesNotSurvive(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newRetryTestApp(t)

	writeGzNZB(t, adminDir, "priorruling.nzb.gz", retryNZBWithRecoveryVolume(2, 1))

	const id = "retrypriorruling1"
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     id,
		Name:      "priorruling",
		NzbName:   "priorruling.nzb",
		NZBBackup: "priorruling.nzb.gz",
		Status:    string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	seedHistoryJobFilesRow(t, repo.DB(), id, 0, false, 2, job.FetchAlways)
	seedHistoryJobFilesRow(t, repo.DB(), id, 1, false, 1, job.FetchAlways)
	seedJobFilesRow(t, repo.DB(), id, 0, false, job.FetchAlways)
	seedJobFilesRow(t, repo.DB(), id, 1, false, job.FetchAlways)

	if err := application.RetryHistoryJob(t.Context(), id); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	j, ok := application.Dispatcher().Job(id)
	if !ok {
		t.Fatal("retried job is not in the queue")
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	idx := recoveryFileIndex(t, m)
	if got := j.Progress().FileFetchPolicy(idx); got != job.FetchIfNeeded {
		t.Errorf("FileFetchPolicy(%d) = %v after retry, want FetchIfNeeded — the job_files row's "+
			"retained FetchAlways (a damage-release verdict against the failed attempt's contents) "+
			"survived into the retry instead of being re-derived", idx, got)
	}
}

// TestRetryHistoryJob_CompletedVolumeKeepsBytes pins that a recovery volume
// already Complete when the attempt failed stays Complete across a retry —
// Complete + FetchIfNeeded is a coherent state (recompute never writes
// Complete, sizeFigures excludes any non-FetchAlways file from both expected
// and remaining, and IsComplete skips it) — while still taking the
// re-derived policy rather than whatever job_files happened to hold
// (#329, case c).
func TestRetryHistoryJob_CompletedVolumeKeepsBytes(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newRetryTestApp(t)

	writeGzNZB(t, adminDir, "keepsbytes.nzb.gz", retryNZBWithRecoveryVolume(2, 1))

	const id = "retrykeepsbytes01"
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     id,
		Name:      "keepsbytes",
		NzbName:   "keepsbytes.nzb",
		NZBBackup: "keepsbytes.nzb.gz",
		Status:    string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	seedHistoryJobFilesRow(t, repo.DB(), id, 0, false, 2, job.FetchAlways)
	// The recovery volume already finished downloading before the rest of
	// the job failed — released to FetchAlways by a damage verdict, then
	// completed.
	seedHistoryJobFilesRow(t, repo.DB(), id, 1, true, 1, job.FetchAlways)
	seedJobFilesRow(t, repo.DB(), id, 0, false, job.FetchAlways)
	seedJobFilesRow(t, repo.DB(), id, 1, true, job.FetchAlways)

	if err := application.RetryHistoryJob(t.Context(), id); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	j, ok := application.Dispatcher().Job(id)
	if !ok {
		t.Fatal("retried job is not in the queue")
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	idx := recoveryFileIndex(t, m)
	if !j.Progress().FileComplete(idx) {
		t.Errorf("FileComplete(%d) = false after retry, want true — a recovery volume that had "+
			"already finished must not be re-fetched just because the job as a whole failed", idx)
	}
	if got := j.Progress().FileFetchPolicy(idx); got != job.FetchIfNeeded {
		t.Errorf("FileFetchPolicy(%d) = %v after retry, want FetchIfNeeded — a completed volume "+
			"must still take the re-derived policy rather than whatever job_files held", idx, got)
	}
}

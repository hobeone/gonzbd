package app_test

import (
	"bytes"
	"database/sql"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
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

// seedCompletedFile leaves file fileIdx of a FAILED entry the way a failed
// attempt that had finished it leaves it: the file on disk under
// downloadDir/jobName, a complete job_files row naming it, and a
// written_articles row per article [firstArt, firstArt+n) whose CRC matches
// the bytes. A FAILED entry keeps both tables, and the retry reads the file
// back against them.
func seedCompletedFile(t *testing.T, db *sql.DB, downloadDir, jobName, jobID string,
	fileIdx int, firstArt, n int32, filename string) {
	t.Helper()
	dir := filepath.Join(downloadDir, jobName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 0, int(n)*1024)
	rows := make([]durability.WrittenRow, 0, n)
	for i := range n {
		part := bytes.Repeat([]byte{byte('A' + fileIdx*8 + int(i))}, 1024)
		rows = append(rows, durability.WrittenRow{
			FileIdx: fileIdx, ArtIdx: firstArt + i, Offset: int64(i) * 1024, Length: 1024,
			CRC32: crc32.ChecksumIEEE(part),
		})
		data = append(data, part...)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO job_files (job_id, file_index, complete, filename, fetch_policy)
		 VALUES (?, ?, 1, ?, ?)`,
		jobID, fileIdx, filename, int(job.FetchAlways)); err != nil {
		t.Fatalf("seed job_files row: %v", err)
	}
	app.SeedWritten(t, durability.NewStore(db), jobID, rows)
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

// seedJobFilesRow inserts a job_files row directly, standing in for a row a
// failed job's reclaim did not take — a reclaim that failed, or a checkpoint
// flush that wrote after it. The retry reclaims such rows before it seeds.
func seedJobFilesRow(t *testing.T, db *sql.DB, jobID string, fileIndex int, complete bool, fetch job.FetchPolicy) {
	t.Helper()
	c := 0
	if complete {
		c = 1
	}
	if _, err := db.Exec(
		`INSERT INTO job_files (job_id, file_index, complete, filename, fetch_policy)
		 VALUES (?, ?, ?, '', ?)`,
		jobID, fileIndex, c, int(fetch)); err != nil {
		t.Fatalf("seed job_files row: %v", err)
	}
}

// TestRetryHistoryJob_ConfigurationIsHonoured pins that a retry derives its
// fetch policy from the CURRENT config, not from whatever a previous attempt
// left behind. On-demand par2 was on when the failed attempt ran — its
// job_files row, which the FAILED entry keeps and the retry reads, holds a
// discarded FetchNever on the recovery volume — and is off now, so the retry
// must fetch the volume unconditionally rather than keep honouring a policy
// the user has since disabled (#329, case a).
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
			"the retained FetchNever from job_files leaked through instead of the re-derived policy", idx, got)
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

// TestRetryHistoryJob_ResumesCompletedFilesFromTheRecord pins where a retry's
// file progress comes from: the FAILED entry's job_files and written_articles
// rows, read back against the file. The file's articles come back Done and its
// job_files row stays complete, or the next eviction re-hydrates the file as
// incomplete and re-fetches it.
func TestRetryHistoryJob_ResumesCompletedFilesFromTheRecord(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newRetryTestApp(t)

	writeGzNZB(t, adminDir, "resumes.nzb.gz", retryNZBWithRecoveryVolume(2, 1))

	const id = "retryresumes0001"
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     id,
		Name:      "resumes",
		NzbName:   "resumes.nzb",
		NZBBackup: "resumes.nzb.gz",
		Status:    string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	seedCompletedFile(t, repo.DB(), application.Config().GetGeneral().DownloadDir, "resumes", id,
		1, 2, 1, "payload.vol000+01.par2")

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
	if !j.Progress().ArticleDone(2) {
		t.Errorf("file %d's article is not Done after retry; its bytes were verified on disk", idx)
	}
	var complete int
	if err := repo.DB().QueryRowContext(t.Context(),
		`SELECT complete FROM job_files WHERE job_id = ? AND file_index = ?`, id, idx).Scan(&complete); err != nil {
		t.Fatalf("read job_files: %v", err)
	}
	if complete != 1 {
		t.Errorf("job_files.complete = %d for file %d after retry, want 1 — the next hydration "+
			"would restore it as incomplete and re-fetch a file already on disk", complete, idx)
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
	// The recovery volume already finished downloading before the rest of
	// the job failed — released to FetchAlways by a damage verdict, then
	// completed.
	seedCompletedFile(t, repo.DB(), application.Config().GetGeneral().DownloadDir, "keepsbytes", id,
		1, 2, 1, "payload.vol000+01.par2")
	seedJobFilesRow(t, repo.DB(), id, 0, false, job.FetchAlways)

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
	if !j.Progress().ArticleDone(2) {
		t.Errorf("file %d's article is not Done after retry — a recovery volume that had "+
			"already finished must not be re-fetched just because the job as a whole failed", idx)
	}
	if got := j.Progress().FileFetchPolicy(idx); got != job.FetchIfNeeded {
		t.Errorf("FileFetchPolicy(%d) = %v after retry, want FetchIfNeeded — a completed volume "+
			"must still take the re-derived policy rather than whatever job_files held", idx, got)
	}
}

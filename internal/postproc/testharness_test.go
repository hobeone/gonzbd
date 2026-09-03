package postproc

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/hobeone/gonzbd/internal/job"
)

// This file is the fixture harness for this package's tests, replacing the
// real *queue.Job/*queue.Queue every test in this package used to build
// against before internal/postproc was repointed onto internal/job. It
// mirrors internal/downloader/testharness_test.go, which solved the same
// problem for that package first (see that file's own doc comment).

// ---------------------------------------------------------------------------
// Manifest fixtures
// ---------------------------------------------------------------------------

// testArticle is one article in a testFile, mirroring job.JobArticle.
type testArticle struct {
	ID     string
	Bytes  int
	Number int
}

// testFile is one file in a manifest fixture, mirroring job.JobFile.
type testFile struct {
	Subject        string
	Date           time.Time
	Bytes          int64
	IsPar2Recovery bool
	Articles       []testArticle
}

// manifestJSONFile/manifestJSONArticle/manifestJSONDoc mirror the unexported
// on-disk shape job.Manifest.[Un]MarshalJSON uses (internal/job/manifest.go)
// field-for-field, including json tags. job.Manifest's own constructor
// (newManifest) and its NZB-parsing caller (NewJob) are both unexported or
// not yet moved into internal/job -- NewJob is still internal/queue's, which
// this package no longer imports -- so UnmarshalJSON, the one exported
// construction path, is how a fixture manifest is built here: marshal this
// shape, then let job.Manifest decode its own wire format.
type manifestJSONArticle struct {
	ID     string `json:"id"`
	Bytes  int    `json:"bytes"`
	Number int    `json:"number"`
}

type manifestJSONFile struct {
	Subject        string                `json:"subject"`
	Date           time.Time             `json:"date"`
	Bytes          int64                 `json:"bytes"`
	IsPar2Recovery bool                  `json:"is_par2_recovery,omitempty"`
	Articles       []manifestJSONArticle `json:"articles"`
}

type manifestJSONDoc struct {
	Files      []manifestJSONFile `json:"files"`
	TotalBytes int64              `json:"total_bytes"`
}

// buildManifest constructs a *job.Manifest from files via the JSON
// round-trip described above. A nil/empty files slice produces a valid
// zero-file manifest, mirroring the old &nzb.NZB{} fixture many of this
// package's tests built a job from.
func buildManifest(t *testing.T, files []testFile) *job.Manifest {
	t.Helper()
	doc := manifestJSONDoc{Files: make([]manifestJSONFile, len(files))}
	for fi, f := range files {
		arts := make([]manifestJSONArticle, len(f.Articles))
		for ai, a := range f.Articles {
			arts[ai] = manifestJSONArticle(a)
		}
		doc.Files[fi] = manifestJSONFile{
			Subject:        f.Subject,
			Date:           f.Date,
			Bytes:          f.Bytes,
			IsPar2Recovery: f.IsPar2Recovery,
			Articles:       arts,
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("buildManifest: marshal: %v", err)
	}
	m := &job.Manifest{}
	if err := json.Unmarshal(raw, m); err != nil {
		t.Fatalf("buildManifest: unmarshal: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Job fixtures
// ---------------------------------------------------------------------------

// newNamedJob builds a resident *job.Job with the given id, display name and
// PP-derived policy, attaching an empty manifest (mirroring queue.NewJob
// called against &nzb.NZB{}, which produced a job with zero files).
func newNamedJob(t *testing.T, id, name string, pp int) *job.Job {
	t.Helper()
	j := job.New(id, name, job.PolicyFromPP(pp))
	if err := j.AttachContent(buildManifest(t, nil)); err != nil {
		t.Fatalf("newNamedJob: AttachContent: %v", err)
	}
	return j
}

// newQueueJob builds a *job.Job whose id and display name are both id --
// replacing the identically-named helper this package used to build against
// internal/queue.Job (queue.NewJob(&nzb.NZB{}, queue.AddOptions{Name: id,
// PP: pp}, ...)). Named to match so most call sites needed no rename.
func newQueueJob(t *testing.T, id string, pp int) *job.Job {
	t.Helper()
	return newNamedJob(t, id, id, pp)
}

// newBareJob returns a *job.Job carrying only the given id/name -- no
// content attached, not even an empty manifest. Mirrors the old
// &queue.Job{ID: id} composite literal, which likewise carried nothing but
// an ID: job.Job.Manifest()/Progress() are ErrNotResident/nil on it, same as
// a bare queue.Job's Manifest() used to report before any content existed.
func newBareJob(id string) *job.Job {
	return job.New(id, id, job.Policy{})
}

// makeJob creates a minimal postproc.Job wrapping a fresh job.Job, for use
// in tests that only need a name to exist.
func makeJob(t *testing.T, name string) *Job {
	t.Helper()
	return &Job{
		Job: newQueueJob(t, name, 3), // Repair + Unpack + Delete (production default)
	}
}

// ---------------------------------------------------------------------------
// Article/file state doors
// ---------------------------------------------------------------------------

// artIdxsFor resolves message IDs to global article indices against j's
// current manifest.
func artIdxsFor(t *testing.T, j *job.Job, msgIDs ...string) []int {
	t.Helper()
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("artIdxsFor: manifest: %v", err)
	}
	byID := make(map[string]int, m.NumArticles())
	for i := range m.NumArticles() {
		byID[m.ArticleID(i)] = i
	}
	out := make([]int, 0, len(msgIDs))
	for _, id := range msgIDs {
		idx, ok := byID[id]
		if !ok {
			t.Fatalf("artIdxsFor: job has no article %s", id)
		}
		out = append(out, idx)
	}
	return out
}

// ackDone marks articles Done through the job's own door, keyed by message ID.
func ackDone(t *testing.T, j *job.Job, msgIDs ...string) {
	t.Helper()
	for _, idx := range artIdxsFor(t, j, msgIDs...) {
		if err := j.MarkArticleDone(idx); err != nil {
			t.Fatalf("ackDone: MarkArticleDone(%d): %v", idx, err)
		}
	}
}

// ackFailed marks articles permanently failed through the job's own door,
// keyed by message ID.
func ackFailed(t *testing.T, j *job.Job, msgIDs ...string) {
	t.Helper()
	for _, idx := range artIdxsFor(t, j, msgIDs...) {
		if err := j.MarkArticleFailed(idx); err != nil {
			t.Fatalf("ackFailed: MarkArticleFailed(%d): %v", idx, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Reflection pokes for doors internal/job has not been given yet
// ---------------------------------------------------------------------------

// pokeProgressField and pokeFileField set an unexported field of a
// *job.JobProgress (or of one of its per-file entries) directly, bypassing
// Go's usual unexported-field protection via the standard
// reflect.NewAt(...unsafe.Pointer...) trick.
//
// internal/job.JobProgress exposes several fields as READERS only
// (Par2ReleaseReason/Par2Recovered, ServerStats, FileAssembledCRC32,
// FileFilename, the per-file FetchPolicy) because production code writes
// them only from doors this branch has not yet ported from internal/queue
// onto internal/job: the on-demand-par2 undefer/discard path
// (internal/queue/job_articles.go's undeferRecovery and
// internal/queue/queue.go's DiscardDeferredPar2), server-stat recording
// (internal/queue.Queue.RecordDownload), and the assembler's
// CRC/filename-on-completion write are all still internal/queue-only --
// `grep -rn 'func.*undeferRecovery\|func.*RecordDownload\|func.*DiscardDeferredPar2'
// internal/job/ internal/dispatch/ internal/sched/` returns nothing on this
// branch. Porting those doors is a different task's scope (they belong to
// on-demand par2 and the assembler, neither of which lives in
// internal/postproc); this harness reaches the same end states their
// absence would otherwise make untestable here, confined entirely to this
// package's test files -- no production code in this package does this.
func pokeProgressField(t *testing.T, p *job.JobProgress, field string, value any) {
	t.Helper()
	v := reflect.ValueOf(p).Elem().FieldByName(field)
	if !v.IsValid() {
		t.Fatalf("pokeProgressField: JobProgress has no field %q", field)
	}
	settable := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem() //nolint:gosec // G103: test-only fixture workaround, see doc comment above
	settable.Set(reflect.ValueOf(value))
}

func pokeFileField(t *testing.T, p *job.JobProgress, fi int, field string, value any) {
	t.Helper()
	filesV := reflect.ValueOf(p).Elem().FieldByName("files")
	filesV = reflect.NewAt(filesV.Type(), unsafe.Pointer(filesV.UnsafeAddr())).Elem() //nolint:gosec // G103: see pokeProgressField
	if fi < 0 || fi >= filesV.Len() {
		t.Fatalf("pokeFileField: file index %d out of range (len %d)", fi, filesV.Len())
	}
	fv := filesV.Index(fi).FieldByName(field)
	if !fv.IsValid() {
		t.Fatalf("pokeFileField: FileProgress has no field %q", field)
	}
	fv.Set(reflect.ValueOf(value))
}

// setPar2ReleaseReason and setPar2Recovered seed the on-demand-par2 verdict
// fields recordVerdict-adjacent rendering code reads. See pokeProgressField.
func setPar2ReleaseReason(t *testing.T, j *job.Job, reason string) {
	t.Helper()
	pokeProgressField(t, j.Progress(), "par2ReleaseReason", reason)
}

func setPar2Recovered(t *testing.T, j *job.Job, v bool) {
	t.Helper()
	pokeProgressField(t, j.Progress(), "par2Recovered", v)
}

// setFileFetchPolicy seeds a file's FetchPolicy directly -- the on-demand
// construction path (JobFile.Deferred) is dropped by newManifest today (it
// operates on *Manifest, which carries no Deferred bit), so nothing wires a
// fresh job's recovery volumes to FetchIfNeeded yet. This is the fixture
// substitute until that door exists.
func setFileFetchPolicy(t *testing.T, j *job.Job, fi int, fp job.FetchPolicy) {
	t.Helper()
	pokeFileField(t, j.Progress(), fi, "Fetch", fp)
}

// recordServerBytes seeds JobProgress.serverStats, replacing
// queue.Queue.RecordDownload for fixtures that need a non-empty ServerStats().
func recordServerBytes(t *testing.T, j *job.Job, server string, bytes int64) {
	t.Helper()
	p := j.Progress()
	v := reflect.ValueOf(p).Elem().FieldByName("serverStats")
	settable := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem() //nolint:gosec // G103: see pokeProgressField
	m, _ := reflect.TypeAssert[map[string]int64](settable)
	if m == nil {
		m = map[string]int64{}
	}
	m[server] += bytes
	settable.Set(reflect.ValueOf(m))
}

// seedFileCRC and seedFileFilename replace queue.Queue.SetFileCRC32FromRuns
// and SetFileFilename for fixtures that need a resolved on-disk name and/or
// an assembled CRC32 without a real download.
func seedFileCRC(t *testing.T, j *job.Job, fi int, crc uint32) {
	t.Helper()
	pokeFileField(t, j.Progress(), fi, "AssembledCRC32", crc)
}

func seedFileFilename(t *testing.T, j *job.Job, fi int, name string) {
	t.Helper()
	pokeFileField(t, j.Progress(), fi, "Filename", name)
}

// setDownloadStamps seeds the download-start/finish timestamps, replacing
// queue.Queue.MarkJobStarted/MarkDownloadFinished for fixtures that need a
// non-zero download duration without a real download.
func setDownloadStamps(t *testing.T, j *job.Job, started, finished time.Time) {
	t.Helper()
	p := j.Progress()
	pokeProgressField(t, p, "downloadStarted", started)
	pokeProgressField(t, p, "downloadFinished", finished)
}

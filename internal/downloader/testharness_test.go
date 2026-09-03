package downloader

import (
	"encoding/json"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// This file is the fixture harness for this package's tests, replacing the
// real *queue.Queue every test in this package used to build against.
//
// internal/downloader depends on JobSource (downloader.go), a narrow
// interface *dispatch.Dispatcher satisfies structurally. Standing up a real
// Dispatcher needs Residency/Store/Runner/Workers fakes belonging to
// internal/dispatch's own test suite, so tests here use fakeJobSource
// instead -- a plain map guarded by a mutex, holding real *job.Job values.
//
// job.Job/job.Manifest are otherwise exactly what production code uses:
// there is no second, test-only content type.

// fakeJobSource is a minimal JobSource: register/deregister *job.Job values
// directly, no persistence, no scheduling.
type fakeJobSource struct {
	mu      sync.Mutex
	jobs    map[string]*job.Job
	headers map[string]dispatch.Header
	paused  bool
}

func newFakeJobSource() *fakeJobSource {
	return &fakeJobSource{
		jobs:    make(map[string]*job.Job),
		headers: make(map[string]dispatch.Header),
	}
}

// add registers j with a zero Header (Added is the zero time.Time, which
// forEachUnfinishedArticle's propagation-delay check reads as "not recently
// added" -- see dispatch.Header's doc comment). Use addWithHeader for a test
// that needs to control Header.Added.
func (f *fakeJobSource) add(j *job.Job) {
	f.addWithHeader(j, dispatch.Header{})
}

func (f *fakeJobSource) addWithHeader(j *job.Job, h dispatch.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[j.ID()] = j
	f.headers[j.ID()] = h
}

// List returns one Row per registered job: ID and Header (the header-tier
// fields a real Dispatcher.List would supply without a manifest read).
// Row.View is left zero -- downloader reads job.Job.Intent()/State()/
// RepairState() directly through Job(id) instead of a rendered view, so
// nothing in this package consumes it.
func (f *fakeJobSource) List() []dispatch.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.jobs))
	for id := range f.jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic iteration order for tests
	out := make([]dispatch.Row, 0, len(ids))
	for _, id := range ids {
		out = append(out, dispatch.Row{ID: id, Header: f.headers[id]})
	}
	return out
}

func (f *fakeJobSource) Job(id string) (*job.Job, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	return j, ok
}

func (f *fakeJobSource) PauseJob(id string) error {
	j, ok := f.Job(id)
	if !ok {
		return dispatch.ErrNotFound
	}
	return j.SetIntent(job.IntentPause)
}

func (f *fakeJobSource) ResumeJob(id string) error {
	j, ok := f.Job(id)
	if !ok {
		return dispatch.ErrNotFound
	}
	return j.SetIntent(job.IntentRun)
}

func (f *fakeJobSource) Paused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

func (f *fakeJobSource) setPaused(p bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = p
}

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

// manifestJSONFile/manifestJSONArticle/manifestJSON mirror the unexported
// on-disk shape job.Manifest.[Un]MarshalJSON uses (internal/job/manifest.go)
// field-for-field, including json tags. job.Manifest's own constructor
// (newManifest) and its NZB-parsing caller (NewJob) are both unexported or
// not yet moved into internal/job (NewJob is still internal/queue's, which
// this package no longer imports), so UnmarshalJSON -- the one exported
// construction path -- is how a fixture manifest is built here: marshal this
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
// round-trip described above.
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

// newTestJob builds a resident *job.Job at State Fetching from files, ready
// to be registered with a fakeJobSource and dispatched against.
func newTestJob(t *testing.T, id string, files []testFile) *job.Job {
	t.Helper()
	j := job.New(id, id, job.Policy{})
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("newTestJob: BeginAttempt: %v", err)
	}
	if err := j.AttachContent(buildManifest(t, files)); err != nil {
		t.Fatalf("newTestJob: AttachContent: %v", err)
	}
	return j
}

// ackDoneIdx marks articles Done through the job's own door. Replaces the
// old ackDoneIdx, which went through queue.Queue.SeedFromRuns to replay
// durability.Run records -- job.Job.MarkArticleDone needs no such proof, so
// the durability-replay machinery this used to require is gone.
func ackDoneIdx(t *testing.T, j *job.Job, artIdxs ...int32) {
	t.Helper()
	for _, a := range artIdxs {
		if err := j.MarkArticleDone(int(a)); err != nil {
			t.Fatalf("ackDoneIdx: MarkArticleDone(%d): %v", a, err)
		}
	}
}

// artIdxsFor resolves message IDs to global article indices against j's
// current manifest.
func artIdxsFor(t *testing.T, j *job.Job, msgIDs ...string) []int32 {
	t.Helper()
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("artIdxsFor: manifest: %v", err)
	}
	byID := make(map[string]int32, m.NumArticles())
	for i := range m.NumArticles() {
		byID[m.ArticleID(i)] = int32(i) //nolint:gosec // G115: article counts are far below int32
	}
	out := make([]int32, 0, len(msgIDs))
	for _, id := range msgIDs {
		idx, ok := byID[id]
		if !ok {
			t.Fatalf("artIdxsFor: job has no article %s", id)
		}
		out = append(out, idx)
	}
	return out
}

// ackDone is ackDoneIdx keyed by message ID.
func ackDone(t *testing.T, j *job.Job, msgIDs ...string) {
	t.Helper()
	ackDoneIdx(t, j, artIdxsFor(t, j, msgIDs...)...)
}

// ackFailed marks articles permanently failed through the job's own door,
// keyed by message ID. Replaces the old ackFailed, which called
// queue.Queue.AckPermanentFailure.
func ackFailed(t *testing.T, j *job.Job, msgIDs ...string) {
	t.Helper()
	for _, a := range artIdxsFor(t, j, msgIDs...) {
		if err := j.MarkArticleFailed(int(a)); err != nil {
			t.Fatalf("ackFailed: MarkArticleFailed(%d): %v", a, err)
		}
	}
}

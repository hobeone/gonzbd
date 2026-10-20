package job

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hobeone/gonzbd/internal/nzb"
)

// Manifest is the job's article table: the flat, global-indexed message-ID,
// byte-count and part-number arrays for every article of every file. Its file
// structure — subjects, sizes, article counts, offsets and recovery figures —
// is the FileTable it was built from, reachable as Files(); a Manifest holds
// no per-file field of its own.
//
// It is immutable after construction and safe to share by reference — every
// Job.Manifest caller gets the same pointer — because the only writes to its
// fields happen in newManifest (`git grep -n '[&]Manifest{' -- 'internal/*.go'
// ':!*_test.go'` returns 1 line). newManifest is the sole constructor and
// refuses an article list whose length is not the table's TotalArticles.
// NewManifestFromArticles is its exported door; NewManifest is the wrapper
// that ingest and tests use to build the table and the manifest together from
// []JobFile.
//
// The JSON form is the article list alone (MarshalJSON, DecodeArticles); the
// file structure is rebuilt from the job_files rows.
type Manifest struct {
	ft            *FileTable
	articleIDs    []string // flat, global article index
	articleBytes  []int
	articleNumber []int
}

// JobFile is the intermediate, NZB-parsed shape NewManifest converts into a
// FileTable and a Manifest; internal/app's BuildIngestJob builds it. It is not part of Job's runtime state —
// construction scaffolding only.
//
//nolint:revive // stutter is preserved for consistency during queue -> job transition
type JobFile struct {
	Subject  string
	Articles []JobArticle
	Bytes    int64
}

// JobArticle is the intermediate, NZB-parsed shape of a single article,
// consumed by newManifest during construction.
//
//nolint:revive // stutter is preserved for consistency during queue -> job transition
type JobArticle struct {
	ID     string
	Bytes  int
	Number int
}

// NewManifest builds a FileTable and a Manifest from parsed files. The
// article count cannot disagree with the table by construction, so a refusal
// from NewManifestFromArticles is a bug here and panics rather than returning
// an error no caller could act on.
func NewManifest(files []JobFile) *Manifest {
	specs := make([]FileSpec, len(files))
	var articles []JobArticle
	for fi, f := range files {
		specs[fi] = FileSpec{Subject: f.Subject, Bytes: f.Bytes, ArticleCount: len(f.Articles)}
		articles = append(articles, f.Articles...)
	}
	m, err := NewManifestFromArticles(NewFileTable(specs), articles)
	if err != nil {
		panic(err)
	}
	return m
}

// NewManifestFromArticles builds a Manifest over ft from the flat article list,
// ordered file by file as ft's offsets describe. It refuses a list whose
// length is not ft.TotalArticles().
func NewManifestFromArticles(ft *FileTable, articles []JobArticle) (*Manifest, error) {
	return newManifest(ft, articles)
}

// newManifest flattens articles into the parallel global arrays beside ft.
//
// NewManifestFromArticles is its only caller (`git grep -n 'newManifes[t](' --
// '*.go' ':!*_test.go'` returns 2 lines: that call and this declaration), and
// NewManifest reaches it through that door, so every Manifest
// in the process has its arrays sized against its table here and nowhere else.
func newManifest(ft *FileTable, articles []JobArticle) (*Manifest, error) {
	if ft == nil {
		return nil, fmt.Errorf("job: manifest: nil file table")
	}
	if want := ft.TotalArticles(); len(articles) != want {
		return nil, fmt.Errorf("job: manifest: file table describes %d articles, got %d", want, len(articles))
	}
	m := &Manifest{
		ft:            ft,
		articleIDs:    make([]string, len(articles)),
		articleBytes:  make([]int, len(articles)),
		articleNumber: make([]int, len(articles)),
	}
	for i, a := range articles {
		m.articleIDs[i] = a.ID
		m.articleBytes[i] = a.Bytes
		m.articleNumber[i] = a.Number
	}
	return m, nil
}

// Files returns the file table this manifest's articles are indexed against.
func (m *Manifest) Files() *FileTable { return m.ft }

// isPar2File reports whether subject names a par2 file (index or recovery
// volume). IsPar2File wraps it, and fileMetaFromTable classifies
// FileProgress.IsPar2 with it. A FileTable's own figures do not use it — see
// FileTable for why a name-based test is the wrong predicate for repair
// capacity.
func isPar2File(subject string) bool {
	return strings.Contains(strings.ToLower(subject), ".par2")
}

// NumFiles returns the number of files in the manifest.
func (m *Manifest) NumFiles() int { return m.ft.NumFiles() }

// FileRange returns the [lo, hi) global article index range owned by
// file fileIdx.
func (m *Manifest) FileRange(fileIdx int) (lo, hi int) { return m.ft.Range(fileIdx) }

// ArticleInFile reports whether fileIdx names a file of m and artIdx an
// article inside that file's range. It is the range check a written row
// passes before it may resolve an article: placeRows, Job.MarkArticleWritten
// and the retry's rowsFitManifest ask it —
// `git grep -n '[A]rticleInFile(' -- '*.go' ':!*_test.go'` returns 4 lines,
// those three and this declaration.
func (m *Manifest) ArticleInFile(fileIdx int, artIdx int32) bool {
	if fileIdx < 0 || fileIdx >= m.NumFiles() {
		return false
	}
	lo, hi := m.FileRange(fileIdx)
	return int(artIdx) >= lo && int(artIdx) < hi
}

// fileIndexForArticle returns the file index owning global article index i,
// derived from the table's offsets rather than a cached per-article
// back-pointer — JobProgress stores no FileIdx field, since the value is
// always cheaply recomputable and caching it would risk drifting out of sync
// with the offsets.
func (m *Manifest) fileIndexForArticle(i int) int {
	// offsets[fi+1] is the first index NOT owned by file fi, so the first fi
	// where offsets[fi+1] > i is the owning file.
	return sort.Search(m.ft.NumFiles(), func(fi int) bool {
		return m.ft.offsets[fi+1] > i
	})
}

// NumArticles returns the total number of articles across all files.
func (m *Manifest) NumArticles() int { return len(m.articleIDs) }

// ArticleID returns the messageID of global article index i.
func (m *Manifest) ArticleID(i int) string { return m.articleIDs[i] }

// ArticleBytes returns the byte count of global article index i.
func (m *Manifest) ArticleBytes(i int) int { return m.articleBytes[i] }

// ArticleNumber returns the part number of global article index i.
func (m *Manifest) ArticleNumber(i int) int { return m.articleNumber[i] }

// manifestJSONArticle is one article's on-disk shape.
type manifestJSONArticle struct {
	ID     string `json:"id"`
	Bytes  int    `json:"bytes"`
	Number int    `json:"number"`
}

// manifestJSON is the on-disk shape of a job's manifest file: the flat article
// list, in global article order, and nothing else. The file structure —
// subjects, sizes, article counts — lives in the job_files rows and is
// rebuilt as a FileTable before the articles are read (DecodeArticles then
// NewManifestFromArticles), so the file carries no figure that a table could
// disagree with.
type manifestJSON struct {
	Articles []manifestJSONArticle `json:"articles"`
}

// MarshalJSON implements json.Marshaler.
func (m *Manifest) MarshalJSON() ([]byte, error) {
	articles := make([]manifestJSONArticle, len(m.articleIDs))
	for i := range articles {
		articles[i] = manifestJSONArticle{
			ID:     m.articleIDs[i],
			Bytes:  m.articleBytes[i],
			Number: m.articleNumber[i],
		}
	}
	return json.Marshal(manifestJSON{Articles: articles})
}

// DecodeArticles reads the flat article list a Manifest marshals to.
//
// It returns the articles rather than a Manifest because a Manifest cannot be
// built without its FileTable (NewManifestFromArticles), which the caller
// holds and this package's JSON does not carry.
//
// It also re-checks each Message-ID, which is the sole thing here that does
// not trust this package's own earlier output — see the comment at the check
// for why a persisted ID is not self-evidently safe.
func DecodeArticles(r io.Reader) ([]JobArticle, error) {
	var mj manifestJSON
	if err := json.NewDecoder(r).Decode(&mj); err != nil {
		return nil, err
	}
	articles := make([]JobArticle, len(mj.Articles))
	for ai, a := range mj.Articles {
		// A Message-ID read back from disk is re-checked, which is the
		// one place this package does not simply trust its own earlier
		// output. internal/nntp interpolates the ID into a command line
		// and no longer validates it, so an ID carrying CR or LF is a
		// command-injection vector — and a manifest written before that
		// rule existed at parse time can still contain one.
		//
		// Refusing the whole manifest rather than dropping the article
		// is deliberate: Hydrate (internal/app/residency.go) returns the
		// error and Dispatcher.reconcileResidency (internal/dispatch/tick.go)
		// settles the job Failed, so it fails visibly, whereas a silent drop
		// would leave it quietly short of articles with nothing to explain it.
		if !nzb.MessageIDIsFetchable(a.ID) {
			return nil, fmt.Errorf("job: manifest article %d: unusable message-id %q", ai, a.ID)
		}
		// Convertible because the two shapes are field-for-field
		// identical; adding a field to either one breaks this line
		// rather than silently dropping it.
		articles[ai] = JobArticle(a)
	}
	return articles, nil
}

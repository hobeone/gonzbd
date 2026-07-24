package queue

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Manifest is the immutable article/file structure of a job: parsed once
// from the NZB (or loaded once from disk) and never mutated afterward.
// Safe to share by reference across Job and every Snapshot/SnapshotJob
// clone of it — there is no aliasing hazard because nothing ever writes
// through it after construction, except the lazy messageID index below,
// which is guarded by the owning Queue's write lock.
type Manifest struct {
	files         []manifestFile
	articleIDs    []string // flat, global article index
	articleBytes  []int
	articleNumber []int
	// fileArticleOffsets[fi]..fileArticleOffsets[fi+1] is the global article
	// range [lo, hi) owned by file fi — a cumulative prefix-sum boundary
	// array, one longer than files (a final sentinel = total article count).
	fileArticleOffsets []int

	totalBytes int64
	par2Bytes  int64
	par2Files  int

	// messageIDIndex is the messageID -> global article index lookup, built
	// lazily on first call to articleIndexByID and dropped by
	// dropMessageIDIndex. Must only be read or built while the owning
	// Queue's write lock (q.mu) is held. Safe to share unsynchronized
	// across snapshots because the only call sites (the four
	// article-mutation Queue methods) all already hold q.mu for write.
	messageIDIndex map[string]int
}

type manifestFile struct {
	subject        string
	date           time.Time
	bytes          int64
	isPar2Recovery bool
}

// newManifest builds a Manifest from files, flattening each file's nested
// articles into the parallel global arrays and computing the
// fileArticleOffsets prefix sum plus totalBytes/par2Bytes/par2Files from
// scratch. Used both by NewJob (initial construction) and by
// DiscardDeferredPar2 (rebuild against a reduced file set) — the latter
// overwrites the returned Manifest's par2Bytes/par2Files afterward, since
// discard must leave those two fields stale rather than recomputed.
func newManifest(files []JobFile) *Manifest {
	m := &Manifest{
		files:              make([]manifestFile, len(files)),
		fileArticleOffsets: make([]int, len(files)+1),
	}
	for fi, f := range files {
		m.files[fi] = manifestFile{
			subject:        f.Subject,
			date:           f.Date,
			bytes:          f.Bytes,
			isPar2Recovery: f.IsPar2Recovery,
		}
		m.fileArticleOffsets[fi] = len(m.articleIDs)
		for _, a := range f.Articles {
			m.articleIDs = append(m.articleIDs, a.ID)
			m.articleBytes = append(m.articleBytes, a.Bytes)
			m.articleNumber = append(m.articleNumber, a.Number)
		}
		m.totalBytes += f.Bytes
		if isPar2File(f.Subject) {
			m.par2Bytes += f.Bytes
			m.par2Files++
		}
	}
	m.fileArticleOffsets[len(files)] = len(m.articleIDs)
	return m
}

// isPar2File reports whether subject names a par2 file (index or recovery
// volume), shared by newManifest and NewJob so the classification heuristic
// can't drift between the two.
func isPar2File(subject string) bool {
	return strings.Contains(strings.ToLower(subject), ".par2")
}

// NumFiles returns the number of files in the manifest.
func (m *Manifest) NumFiles() int { return len(m.files) }

// FileSubject returns the NZB subject line for file fileIdx.
func (m *Manifest) FileSubject(fileIdx int) string { return m.files[fileIdx].subject }

// FileDate returns the posting date for file fileIdx.
func (m *Manifest) FileDate(fileIdx int) time.Time { return m.files[fileIdx].date }

// FileBytes returns the NZB-claimed byte count for file fileIdx.
func (m *Manifest) FileBytes(fileIdx int) int64 { return m.files[fileIdx].bytes }

// FileIsPar2Recovery reports whether file fileIdx is a par2 recovery volume.
func (m *Manifest) FileIsPar2Recovery(fileIdx int) bool { return m.files[fileIdx].isPar2Recovery }

// FileRange returns the [lo, hi) global article index range owned by
// file fileIdx.
func (m *Manifest) FileRange(fileIdx int) (lo, hi int) {
	return m.fileArticleOffsets[fileIdx], m.fileArticleOffsets[fileIdx+1]
}

// fileIndexForArticle returns the file index owning global article index i,
// derived from fileArticleOffsets rather than a cached per-article
// back-pointer — JobProgress stores no FileIdx field, since the value is
// always cheaply recomputable from the manifest and caching it would risk
// drifting out of sync with fileArticleOffsets after a rebuild.
func (m *Manifest) fileIndexForArticle(i int) int {
	// fileArticleOffsets[fi+1] is the first index NOT owned by file fi, so
	// the first fi where offsets[fi+1] > i is the owning file.
	return sort.Search(len(m.fileArticleOffsets)-1, func(fi int) bool {
		return m.fileArticleOffsets[fi+1] > i
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

// TotalBytes returns the sum of all files' claimed byte counts.
func (m *Manifest) TotalBytes() int64 { return m.totalBytes }

// Par2Bytes returns the sum of all par2 files' byte counts.
func (m *Manifest) Par2Bytes() int64 { return m.par2Bytes }

// Par2Files returns the count of par2 files.
func (m *Manifest) Par2Files() int { return m.par2Files }

// articleIndexByID returns the global article index for messageID, or
// (0, false) if not found. Lazily builds messageIDIndex on first call.
// Must be called with the owning Queue's write lock held.
func (m *Manifest) articleIndexByID(messageID string) (int, bool) {
	if m.messageIDIndex == nil {
		m.buildMessageIDIndex()
	}
	i, ok := m.messageIDIndex[messageID]
	return i, ok
}

// buildMessageIDIndex populates messageIDIndex over all articles.
func (m *Manifest) buildMessageIDIndex() {
	m.messageIDIndex = make(map[string]int, len(m.articleIDs))
	for i, id := range m.articleIDs {
		m.messageIDIndex[id] = i
	}
}

// dropMessageIDIndex releases messageIDIndex. Called from the same place
// dropArtIndex is called today (SetPostProcStarted). NOT called from
// DiscardDeferredPar2's equivalent — that path builds an entirely fresh
// Manifest whose messageIDIndex starts nil, so there is nothing to drop;
// calling dropMessageIDIndex there would be a harmless no-op, not a
// requirement.
func (m *Manifest) dropMessageIDIndex() {
	m.messageIDIndex = nil
}

// manifestJSONArticle is one article's on-disk shape within a manifestJSONFile.
type manifestJSONArticle struct {
	ID     string `json:"id"`
	Bytes  int    `json:"bytes"`
	Number int    `json:"number"`
}

// manifestJSONFile is one file's on-disk shape, nesting its articles —
// clearer to read than Manifest's internal flat parallel-array
// representation, and free to differ from it since there is no
// wire-format compatibility requirement on this persistence format.
type manifestJSONFile struct {
	Subject        string                `json:"subject"`
	Date           time.Time             `json:"date"`
	Bytes          int64                 `json:"bytes"`
	IsPar2Recovery bool                  `json:"is_par2_recovery,omitempty"`
	Articles       []manifestJSONArticle `json:"articles"`
}

// manifestJSON is Manifest's on-disk shape. messageIDIndex is deliberately
// excluded — it is rebuilt lazily on demand rather than persisted.
// Par2Bytes/Par2Files are persisted as-is rather than recomputed on load,
// so a manifest left stale by DiscardDeferredPar2 stays stale across a
// save/load cycle too.
type manifestJSON struct {
	Files      []manifestJSONFile `json:"files"`
	TotalBytes int64              `json:"total_bytes"`
	Par2Bytes  int64              `json:"par2_bytes"`
	Par2Files  int                `json:"par2_files"`
}

// MarshalJSON implements json.Marshaler.
func (m *Manifest) MarshalJSON() ([]byte, error) {
	files := make([]manifestJSONFile, m.NumFiles())
	for fi := range files {
		lo, hi := m.FileRange(fi)
		articles := make([]manifestJSONArticle, 0, hi-lo)
		for i := lo; i < hi; i++ {
			articles = append(articles, manifestJSONArticle{
				ID:     m.articleIDs[i],
				Bytes:  m.articleBytes[i],
				Number: m.articleNumber[i],
			})
		}
		files[fi] = manifestJSONFile{
			Subject:        m.files[fi].subject,
			Date:           m.files[fi].date,
			Bytes:          m.files[fi].bytes,
			IsPar2Recovery: m.files[fi].isPar2Recovery,
			Articles:       articles,
		}
	}
	return json.Marshal(manifestJSON{
		Files:      files,
		TotalBytes: m.totalBytes,
		Par2Bytes:  m.par2Bytes,
		Par2Files:  m.par2Files,
	})
}

// UnmarshalJSON implements json.Unmarshaler.
func (m *Manifest) UnmarshalJSON(data []byte) error {
	var mj manifestJSON
	if err := json.Unmarshal(data, &mj); err != nil {
		return err
	}
	m.files = make([]manifestFile, len(mj.Files))
	m.fileArticleOffsets = make([]int, len(mj.Files)+1)
	for fi, f := range mj.Files {
		m.files[fi] = manifestFile{
			subject:        f.Subject,
			date:           f.Date,
			bytes:          f.Bytes,
			isPar2Recovery: f.IsPar2Recovery,
		}
		m.fileArticleOffsets[fi] = len(m.articleIDs)
		for _, a := range f.Articles {
			m.articleIDs = append(m.articleIDs, a.ID)
			m.articleBytes = append(m.articleBytes, a.Bytes)
			m.articleNumber = append(m.articleNumber, a.Number)
		}
	}
	m.fileArticleOffsets[len(mj.Files)] = len(m.articleIDs)
	m.totalBytes = mj.TotalBytes
	m.par2Bytes = mj.Par2Bytes
	m.par2Files = mj.Par2Files
	return nil
}

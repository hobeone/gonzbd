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

	totalBytes    int64
	recoveryBytes int64
	recoveryFiles int

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
// fileArticleOffsets prefix sum, totalBytes, and the recovery figures.
//
// NewJob is the only caller. DiscardDeferredPar2 used to call it too, to
// rebuild against a reduced file set, and that rebuild is why the recovery
// figures were once stored rather than derived — the discard deliberately
// left them describing the larger, pre-discard job. Since #331 the discard
// only marks a FetchPolicy and changes no file set, so nothing can make
// these figures disagree with the file list they are computed from.
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
	}
	m.fileArticleOffsets[len(files)] = len(m.articleIDs)
	m.recoveryBytes, m.recoveryFiles = recoveryFigures(m.files)
	return m
}

// recoveryFigures sums the par2 recovery volumes in files, returning their
// total size and count. Both construction paths — newManifest and
// UnmarshalJSON — call it, so the two cannot disagree about what a job's
// repair capacity is.
//
// The predicate is isPar2Recovery — a subject matching the ".volNNN+MM.par2"
// convention — not the broader name-based isPar2File, which matches any
// ".par2" subject and so also picks up the file conventionally used as an
// index. An index holds per-file checksums, and counting those as repair
// capacity overstated it everywhere these figures are read.
//
// "Conventionally" is doing real work in that sentence, and the figure is
// weaker than it looks. The PAR2 specification says a file carrying recovery
// slices *should* be named with a .volNNN+MM segment; it does not require it,
// does not forbid recovery slices in a plainly-named .par2, and does not
// define an index file at all. par2 itself reads packets and ignores names.
// So this counts recognized capacity, and a job can hold more than it reports.
// Callers must not read zero here as proof that a job cannot be repaired —
// see JobProgress.HasPar2Files and the guards that use it.
func recoveryFigures(files []manifestFile) (bytes int64, count int) {
	for _, f := range files {
		if f.isPar2Recovery {
			bytes += f.bytes
			count++
		}
	}
	return bytes, count
}

// isPar2File reports whether subject names a par2 file (index or recovery
// volume). NewJob is the only caller: it pairs this with isRecoveryVolume to
// decide JobFile.IsPar2Recovery, which is the classification everything else
// keys on. Manifest's own figures no longer use it — see recoveryFigures for
// why a name-based test is the wrong predicate for repair capacity.
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

// RecoveryBytes returns the summed size of the files whose subjects identify
// them as par2 recovery volumes, excluding the file conventionally used as the
// set's index, which holds per-file checksums rather than recovery slices.
//
// This is a proxy for repair capacity, and a doubly weak one. Read the
// qualifications before comparing anything against it.
//
// It is *recognized* capacity, not total capacity. The classification comes
// from the NZB subject, and the ".volNNN+MM.par2" convention is a convention:
// the PAR2 specification recommends it for files carrying recovery slices
// without requiring it, never forbids slices in a plainly-named .par2, and
// defines no index file. par2 reads packets, not names. A job may therefore
// hold recovery data this figure does not see, so zero is not evidence a job
// cannot be repaired.
//
// Even where the classification is right, bytes are only a proxy for what
// actually decides repair, which is block counts — usable parity shards
// against unusable data shards — and nothing can know those until par2 parses
// the volumes. The comparison is sound in one direction only: damage
// exceeding this figure does imply unrepairable, since recovery bytes are at
// least the slice payload. Damage failing to exceed it implies nothing,
// because scattered damage destroys more blocks than its byte count suggests.
func (m *Manifest) RecoveryBytes() int64 { return m.recoveryBytes }

// RecoveryFiles returns the count of par2 recovery volumes, excluding the
// index. See RecoveryBytes.
func (m *Manifest) RecoveryFiles() int { return m.recoveryFiles }

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
// dropArtIndex is called today (SetPostProcStarted). DiscardDeferredPar2 does
// not call it and does not need to: since #331 that path mutates a
// FetchPolicy in place and leaves the manifest — index included — exactly as
// it was.
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
//
// The recovery figures are excluded too, and recomputed on load from the
// per-file is_par2_recovery flags this format already carries. They were
// persisted while DiscardDeferredPar2 rebuilt the manifest against a reduced
// file set, because the discard deliberately left them describing the larger
// pre-discard job and a recomputation would have erased that. #331 removed
// the rebuild, so there is no staleness left to preserve — only the risk that
// a stored copy drifts from the file list it claims to summarize.
//
// Dropping the two keys is a soft landing for manifests written by an earlier
// build: encoding/json ignores keys with no matching field, and the
// recomputation produces the corrected figure regardless of what they carried.
// Note it does not reproduce them — an older build counted the par2 index, so
// a legacy blob's par2_bytes is larger than the recovery figure derived from
// the same file list. That difference is the point of the change, not a loss.
type manifestJSON struct {
	Files      []manifestJSONFile `json:"files"`
	TotalBytes int64              `json:"total_bytes"`
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
	m.recoveryBytes, m.recoveryFiles = recoveryFigures(m.files)
	return nil
}

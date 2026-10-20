package job

// FileTable is the per-file structure of a job: each file's subject, byte
// size and article count, with the article offsets, totals and recovery
// figures derived from them. It is immutable after construction and safe to
// share by reference.
//
// newFileTable is the only writer of every field, including every derived
// one (offsets, totalBytes, the recovery figures, isPar2Recovery), so a
// table cannot describe a file list its figures were not computed from.
// NewFileTable is the exported door to it.
//
// IsRecoveryVolume is the one predicate for recovery capacity: a subject
// matching the ".volNNN+MM.par2" convention, not the broader name-based
// isPar2File, which matches any ".par2" subject and so also picks up the file
// conventionally used as an index. An index holds per-file checksums, and
// counting those as repair capacity overstated it everywhere these figures
// are read.
//
// "Conventionally" is doing real work in that sentence, and the figure is
// weaker than it looks. The PAR2 specification says a file carrying recovery
// slices *should* be named with a .volNNN+MM segment; it does not require it,
// does not forbid recovery slices in a plainly-named .par2, and does not
// define an index file at all. par2 itself reads packets and ignores names.
// So this counts recognized capacity, and a job can hold more than it reports.
// Callers must not read zero here as proof that a job cannot be repaired —
// see JobProgress.HasPar2Files and the guards that use it.
type FileTable struct {
	files []fileEntry
	// offsets[fi]..offsets[fi+1] is the global article range [lo, hi) owned by
	// file fi — a cumulative prefix-sum boundary array, one longer than files
	// (a final sentinel = total article count).
	offsets []int

	totalBytes        int64
	recoveryByteTotal int64
	recoveryFileTotal int
}

type fileEntry struct {
	subject        string
	bytes          int64
	articleCount   int
	isPar2Recovery bool
}

// FileSpec is the per-file NZB-derived structure NewFileTable takes.
type FileSpec struct {
	Subject      string
	Bytes        int64
	ArticleCount int
}

// NewFileTable builds a FileTable from specs. The specs are copied; the table
// does not alias the caller's slice.
func NewFileTable(files []FileSpec) *FileTable {
	return newFileTable(files)
}

func newFileTable(specs []FileSpec) *FileTable {
	t := &FileTable{
		files:   make([]fileEntry, len(specs)),
		offsets: make([]int, len(specs)+1),
	}
	next := 0
	for fi, s := range specs {
		t.files[fi] = fileEntry{
			subject:        s.Subject,
			bytes:          s.Bytes,
			articleCount:   s.ArticleCount,
			isPar2Recovery: IsRecoveryVolume(s.Subject),
		}
		t.offsets[fi] = next
		next += s.ArticleCount
		t.totalBytes += s.Bytes
	}
	t.offsets[len(specs)] = next
	t.recoveryByteTotal, t.recoveryFileTotal = recoveryFiguresOf(t.files)
	return t
}

// recoveryFiguresOf sums the par2 recovery volumes in files, returning their
// total size and count. See FileTable for why the predicate is
// IsRecoveryVolume.
func recoveryFiguresOf(files []fileEntry) (bytes int64, count int) {
	for _, f := range files {
		if f.isPar2Recovery {
			bytes += f.bytes
			count++
		}
	}
	return bytes, count
}

// NumFiles returns the number of files in the table.
func (t *FileTable) NumFiles() int { return len(t.files) }

// Subject returns the NZB subject line for file fi.
func (t *FileTable) Subject(fi int) string { return t.files[fi].subject }

// Bytes returns the NZB-claimed byte count for file fi.
func (t *FileTable) Bytes(fi int) int64 { return t.files[fi].bytes }

// IsPar2Recovery reports whether file fi is a par2 recovery volume,
// IsRecoveryVolume(Subject(fi)) computed once at construction.
func (t *FileTable) IsPar2Recovery(fi int) bool { return t.files[fi].isPar2Recovery }

// ArticleCount returns the number of articles file fi owns.
func (t *FileTable) ArticleCount(fi int) int { return t.files[fi].articleCount }

// Range returns the [lo, hi) global article index range owned by file fi.
func (t *FileTable) Range(fi int) (lo, hi int) { return t.offsets[fi], t.offsets[fi+1] }

// TotalArticles returns the article count across all files.
func (t *FileTable) TotalArticles() int { return t.offsets[len(t.files)] }

// TotalBytes returns the NZB-claimed byte count across all files.
func (t *FileTable) TotalBytes() int64 { return t.totalBytes }

// RecoveryBytes returns the total size of the par2 recovery volumes.
func (t *FileTable) RecoveryBytes() int64 { return t.recoveryByteTotal }

// RecoveryFiles returns the number of par2 recovery volumes.
func (t *FileTable) RecoveryFiles() int { return t.recoveryFileTotal }

// Specs returns a copy of the per-file structure the table was built from.
func (t *FileTable) Specs() []FileSpec {
	out := make([]FileSpec, len(t.files))
	for fi, f := range t.files {
		out[fi] = FileSpec{Subject: f.subject, Bytes: f.bytes, ArticleCount: f.articleCount}
	}
	return out
}

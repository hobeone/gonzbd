package durability

import "context"

// DurableArticle is one article a completed Drain reported as fsynced,
// carrying exactly what durable_runs needs to place it: which file, which
// position within it, and the CRC32 of its decoded bytes. It mirrors
// WrittenArticle's fields deliberately rather than inventing a parallel
// vocabulary — WrittenArticle is what the barrier drains, and a
// DurableArticle is the same fact after the fsync that makes it durable, so
// RunStore.Commit's callers build these directly from what Drain returned.
type DurableArticle struct {
	// FileIdx is the target file within the job.
	FileIdx int32
	// ArtIdx is the article's global index within the job's manifest.
	ArtIdx int32
	// Offset is the decoded byte position within the target file.
	Offset int64
	// Length is the decoded byte length.
	Length int32
	// CRC32 is the CRC32 of the decoded bytes.
	CRC32 uint32
}

// Run is one row of durable_runs: a maximal span of articles that abut in
// both byte offset and article index, made durable together. FirstArtIdx and
// LastArtIdx name which articles the run accounts for; Offset and Length
// bound its bytes; CRC32 is combined left-to-right over the articles it
// covers via crc32util.Combine, so when a file collapses to one Run starting
// at Offset 0, that Run's CRC32 IS the whole-file CRC.
type Run struct {
	// FileIdx is the target file within the job.
	FileIdx int32
	// FirstArtIdx is the lowest article index this run accounts for.
	FirstArtIdx int32
	// LastArtIdx is the highest article index this run accounts for.
	LastArtIdx int32
	// Offset is the run's starting byte position within the file.
	Offset int64
	// Length is the run's total byte length — the sum of its articles'
	// decoded lengths.
	Length int64
	// CRC32 is the CRC32 of the run's bytes, combined left-to-right over
	// its constituent articles.
	CRC32 uint32
}

// RunStore is the durability record, described in
// docs/superpowers/specs/2026-08-22-single-durability-record-design.md.
//
// One record, written only after the fsync that makes it true (S1, S2), and
// grouped into runs rather than kept per article. It replaced a pairing of two
// per-article records with independent writers, which could disagree — and
// every disagreement was a defect (#389, #421). Barrier is the only thing that
// puts CONTENT into a row; Resumer only ever deletes, as do the four other
// deletion paths listed in this package's doc comment. None of them can make a
// row assert anything, which is the bound §3.4's trust argument rests on.
type RunStore interface {
	// Commit takes ARTICLES, not runs — the caller hands over what a
	// completed Drain reported, and the store is the one place a run is
	// ever constructed from them. A caller that pre-built runs would put
	// that derived value's construction at two independently-maintained
	// sites, and — more importantly — would make the required dedup
	// unreachable: an at-least-once redelivery must be subtracted against
	// what is already stored at ARTICLE granularity, before grouping, or a
	// redelivery adjacent to genuinely new articles produces a run no
	// stored row covers and a false overlap (Σ length exceeding the file's
	// true size). See the design doc §6 for the worked example.
	//
	// Commit is idempotent: an article whose ArtIdx a stored row already
	// covers is dropped rather than re-inserted. It merges the surviving
	// articles with each other and with any stored run they abut, so a
	// file whose runs arrive in order collapses toward a single row over
	// successive commits. The whole call is atomic — every row for this
	// commit lands, or none do.
	Commit(ctx context.Context, jobID string, arts []DurableArticle) error

	// ForFile returns every stored run for one file, ordered by Offset.
	ForFile(ctx context.Context, jobID string, fileIdx int32) ([]Run, error)

	// ForJob returns every stored run for a job, across all its files,
	// ordered by FileIdx then Offset.
	ForJob(ctx context.Context, jobID string) ([]Run, error)

	// DeleteFile removes every run for ONE file of a job.
	//
	// It exists for Resumer, whose whole mutation budget is this: a file
	// shorter than its runs claim has disproved them, and the response is
	// to drop them so the file is fetched again (§3.4). Scoped to a file
	// rather than a job because a resume proves nothing about the job's
	// other files — it stat'ed one path.
	DeleteFile(ctx context.Context, jobID string, fileIdx int32) error

	// DeleteJob removes every run for a job that has left the queue. It
	// touches only durable_runs — failed_articles is owned and written
	// solely by internal/queue, and this store never writes to it.
	DeleteJob(ctx context.Context, jobID string) error
}

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

// Collision is one exact-offset duplicate a Commit dropped: two entries
// claiming the same byte at the same offset of the same file, of which only
// one can be stored because (job_id, file_idx, offset) is the primary key.
//
// It is returned rather than persisted because it describes an EVENT, not a
// property. Once the commit has landed, the surviving row is indistinguishable
// from a row that never had a rival: the dropped one contributes nothing to
// Σ length, leaves no gap, and is not named by any index. A later pass over
// the stored rows cannot re-derive this, which is why it must travel out of
// the call that observed it.
//
// Kept and Dropped name the LOWEST article index on each side. Either side may
// be a run that has already merged several articles, so the pair identifies the
// collision rather than exhausting the articles involved — and after the drop
// there is no longer a pair on disk to enumerate.
//
// What it means for the file is not decidable from here, and the report must
// not overstate it. Two articles claiming one offset arises from a multi-part
// UU post whose parts all assert offset 0 (dispatch.go's block): within one
// open-file episode the assembler resolves it and the file completes SHORT,
// which is benign; across a close-handles cycle or a restart the second write
// overwrites the first and the file completes WRONG. Both need par2. Only the
// second is corruption, and this type cannot tell them apart.
type Collision struct {
	// FileIdx is the file whose offset was claimed twice.
	FileIdx int32
	// Offset is the byte position both entries claimed.
	Offset int64
	// Kept is the lowest article index of the surviving entry.
	Kept int32
	// Dropped is the lowest article index of the discarded entry.
	Dropped int32
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
	//
	// A second kind of drop happens, and it is not a redelivery: where
	// two entries end up at the SAME Offset — a stored row and an
	// incoming article, or two incoming articles — the shorter is
	// dropped, whichever side it is on. Nothing merges at an equal
	// offset, so both would otherwise be written to the one primary key
	// (job_id, file_idx, offset) and INSERT OR REPLACE would keep an
	// arbitrary one. Dropping the shorter deterministically is what keeps
	// FinalizeFile's truncate bound from shrinking to it on a LATER
	// finalize. mergeAdjacentRuns carries the argument.
	//
	// This is the exact-offset collision internal/downloader/dispatch.go's
	// UU block describes, and the returned Collisions are how it is
	// reported: the dropped row contributes nothing to Σ length, so
	// §3.3's completion check has no evidence to find and cannot be the
	// signal. §3.3's "an overlapped file keeps more rows" is therefore
	// true of PARTIAL overlaps and not of this one, and this return value
	// exists because that difference is invisible from the stored rows
	// afterwards — the commit that drops the row is the last moment the
	// collision can be observed at all.
	//
	// Collisions are returned rather than raised: a commit that hits one
	// has still succeeded, the transaction still lands, and the file is
	// still repairable. See Collision.
	Commit(ctx context.Context, jobID string, arts []DurableArticle) ([]Collision, error)

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

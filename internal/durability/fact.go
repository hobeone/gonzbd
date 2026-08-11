// Package durability owns the persistence of download progress.
//
// It divides every fact into two classes, per
// docs/superpowers/specs/2026-08-11-download-durability-design.md:
//
//   - Class A (ArticleFact) — immutable facts discovered when an article is
//     decoded. They assert nothing about presence on disk: an ArticleFact
//     says only "if the bytes at [Offset, Offset+Length) are present, they
//     hash to CRC32". That is true the instant the article is decoded and
//     stays true forever, so Class A may be committed at any time in any
//     order, with no barrier and no ordering constraint against the data.
//
//   - Class B (FileExtent) — a cache of values derived from Class A plus the
//     file's actual bytes. Class B asserts presence, so it may only be
//     committed by Barrier, strictly after the fsync that makes its claims
//     true (S1). It is never authoritative: where it disagrees with a
//     recomputation, the recomputation is correct by definition (S4).
package durability

import "context"

// ArticleFact is an immutable Class A fact about one article.
//
// Offset and Length are the article's *decoded* byte range, which is not
// knowable until the article is fetched — the NZB carries only an encoded
// byte count, while the true range comes from the yEnc =ypart header. That
// is why this mapping must be persisted rather than derived from the NZB.
type ArticleFact struct {
	// FileIdx is the article's file within the job.
	FileIdx int32
	// ArtIdx is the article's global index within the job's manifest.
	ArtIdx int32
	// Offset is the decoded byte position within the target file.
	Offset int64
	// Length is the decoded byte length.
	Length int32
	// CRC32 is the CRC32 of the decoded bytes, valid only when HasCRC.
	CRC32 uint32
	// HasCRC is false for UU-encoded articles, which carry no CRC.
	// Distinguishing this from CRC32 == 0 is required by R23.
	HasCRC bool
}

// FactLog is the append-only store of Class A facts.
//
// Append has no ordering relationship to file writes (R2). Losing a suffix
// of the log is safe and costs only a re-fetch (R3).
type FactLog interface {
	// Append records facts. It is idempotent per (jobID, ArtIdx): appending
	// a fact whose ArtIdx is already present is a no-op, not an update,
	// because a Class A fact never changes (R1).
	Append(ctx context.Context, jobID string, facts []ArticleFact) error

	// ForFile returns every recorded fact for one file, ordered by Offset.
	ForFile(ctx context.Context, jobID string, fileIdx int32) ([]ArticleFact, error)

	// DeleteJob removes every fact for a job that has left the queue.
	DeleteJob(ctx context.Context, jobID string) error
}

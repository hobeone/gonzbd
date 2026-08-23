// Package durability owns the persistence of download progress.
//
// It records ONE fact about a download, in one table, whose content is put
// there by one writer at one moment, per
// docs/superpowers/specs/2026-08-22-single-durability-record-design.md:
//
//	durable_runs(job_id, file_idx, first_art_idx, last_art_idx,
//	             offset, length, crc32)
//
// A Run is a maximal span of articles that abut in BOTH byte offset and
// article index and were made durable together. It is written only after
// the fsync that makes its bytes durable (S1, S2), so it asserts presence
// as well as content: the bytes at [Offset, Offset+Length) ARE on disk and
// they hash to CRC32.
//
// # What this replaced, and why the shape changed
//
// Until this design the same download was described twice — Class A
// (ArticleFact, appended at decode with no ordering against the write) and
// Class B (FileExtent, a per-file durable bitmap committed by the barrier
// after the fsync). Two writers describing one thing can disagree, and every
// disagreement was a defect: #389 recorded an article the assembler then
// rejected, #421 recorded a bogus yEnc offset permanently because the store
// was append-only. Both classes are gone, along with the contiguity
// apparatus that existed to reconcile them — verifiedPrefix, the abutment
// walk, durableAt, the durable Bitmap, and both of FinalizeFile's guards.
//
// The proof those walks re-derived on every checkpoint now happens once, at
// write time, as a single comparison: two articles either abut or they do
// not, and the store records the answer. crc32util.Combine is associative,
// so a run's CRC32 is built pairwise as articles join it, and a file whose
// articles arrive in order collapses to ONE row at offset 0 whose CRC32 is
// the whole-file CRC (§3.5).
//
// # The three questions the record answers
//
//   - Which articles need fetching — the complement of the runs' article
//     ranges. Resume returns the runs; the queue takes the complement.
//   - Where the file ends — max(Offset+Length) over its runs, which is what
//     FinalizeFile trims to (S6: the trim only ever shrinks).
//   - Whether the file's bytes are in doubt — Σ Length greater than the
//     file's size means articles wrote over each other (§3.3), and any file
//     holding more than one row is refused a whole-file CRC (§3.5).
//
// # One writer of content, and several deleters
//
// Barrier is the only thing that puts CONTENT into a row: RunStore.Commit is
// called from Run and from FinalizeFile, both inside the transaction that
// precedes the ack, and nowhere else. Resumer never writes. Its whole job at
// startup is one stat per file — if the file on disk is shorter than its runs
// claim, it DELETES those runs and the articles are fetched again (§3.4). That
// asymmetry is what makes the record trustworthy without reading a byte of it
// back.
//
// Do not read that as "only these two touch the table". Rows are deleted from
// five places outside Commit's own merge: Resumer, RunStore.DeleteJob (a job
// leaving the queue, or a retry re-parsing a changed manifest),
// queue.SQLiteStore.removeCorrupt and pruneDurabilityRows, and
// history.Repository.delete. The bound that actually holds — and the only one
// the trust argument needs — is on CONTENT: a delete can only ever take a
// claim away, which is S3's safe direction.
package durability

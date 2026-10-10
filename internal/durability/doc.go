// Package durability owns the persistence of download progress. Store runs all
// of the production SQL on written_articles and job_files.
//
// The record is a loose one. written_articles holds one row per article whose
// decoded bytes were handed to pwrite and for which pwrite returned nil:
//
//	written_articles(job_id, file_idx, art_idx, offset, length, crc32)
//
// A row is not a durability claim — it may describe bytes the kernel never
// flushed. What makes it safe to use is that a restart does not trust it: the
// verifier in internal/app reads back every row of a complete=0 file and
// compares its CRC before installing the article as Done, and a failed fsync
// deletes the file's rows (internal/app.handleFileUntrusted). A complete=1
// file is installed as it stands, because complete=1 is written only after the
// file's fsync. A row that cannot be verified costs a re-fetch of its article,
// never a hole.
//
// job_files holds the per-file results nothing else records: whether the file
// finished, what it was called, and its fetch policy.
//
// # Writers
//
// Store.ApplyRecord is the only writer of written_articles and the only
// UPDATE of job_files, and the recorder in internal/app is its caller:
// `git grep -n -E 'INTO written_[a]rticles' -- '*.go' ':!*_test.go'` returns 1
// line, in written.go;
// `git grep -n -E 'UPDATE job_[f]iles' -- '*.go' ':!*_test.go'` returns 2
// lines, both in written.go; and
// `git grep -n -E '\.ApplyRecord[(]' -- '*.go' ':!*_test.go'` returns 2 lines,
// both in internal/app/record.go. Store.Admit is the one other writer of
// job_files: it inserts each file's seed row and sets its fetch_policy
// (`git grep -n -E 'INTO job_[f]iles' -- '*.go' ':!*_test.go'` returns 1 line,
// in progress.go).
//
// # Deleters
//
// Rows leave through ApplyRecord's verdicts (a file whose bytes failed
// verification, or whose fsync failed) and through the reclaim rule
// (reclaim.go), which removes a job's rows once nothing reaches the job — no
// queue row and, for the tables a retry reads, no FAILED history entry.
package durability

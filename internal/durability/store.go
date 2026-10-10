package durability

import "database/sql"

// Store owns a job's per-job rows in the history database: written_articles
// and job_files. written.go holds the written_articles half and the recorder's
// write path; progress.go the job_files seed and read; reclaim.go the rule
// that removes both when nothing reaches the job.
type Store struct {
	db *sql.DB
}

// NewStore wraps db. The caller owns db's lifecycle; Store never opens,
// closes, or otherwise acts on the file itself.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

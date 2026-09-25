pkg ./internal/durability/
run TestSaveProgress_|TestStore_SaveProgressUpdatesFilesAndAddsFailedMarks|TestReclaim_
timeout 3m

[the failed-article insert is unguarded]
file internal/durability/progress.go
--- anchor
		`INSERT OR IGNORE INTO failed_articles (job_id, art_idx) SELECT ?1, ?2 WHERE EXISTS (SELECT 1 FROM job_files WHERE job_id = ?1)`,
--- replace
		`INSERT OR IGNORE INTO failed_articles (job_id, art_idx) VALUES (?1, ?2)`,
--- end

[the guard reads the wrong job's files]
file internal/durability/progress.go
--- anchor
		`INSERT OR IGNORE INTO failed_articles (job_id, art_idx) SELECT ?1, ?2 WHERE EXISTS (SELECT 1 FROM job_files WHERE job_id = ?1)`,
--- replace
		`INSERT OR IGNORE INTO failed_articles (job_id, art_idx) SELECT ?1, ?2 WHERE EXISTS (SELECT 1 FROM job_files)`,
--- end

[the guard never matches, so no failed article is ever recorded]
file internal/durability/progress.go
--- anchor
		`INSERT OR IGNORE INTO failed_articles (job_id, art_idx) SELECT ?1, ?2 WHERE EXISTS (SELECT 1 FROM job_files WHERE job_id = ?1)`,
--- replace
		`INSERT OR IGNORE INTO failed_articles (job_id, art_idx) SELECT ?1, ?2 WHERE EXISTS (SELECT 1 FROM job_files WHERE job_id = ?1 AND 1 = 0)`,
--- end

pkg ./internal/app/
run TestSeedJobFiles_WritesTheStructure|TestSeedJobFiles_RefusesAJobWithoutFiles

# seedJobFiles copies the job's file table into job_files so a hydrated job can
# rebuild its structure without the manifest. Each mutation drops one column
# the table owns.

[the seed writes no article count]
file internal/app/app.go
--- anchor
			ArticleCount: s.ArticleCount,
--- replace
			ArticleCount: 0,
--- end

[the seed writes no subject]
file internal/app/app.go
--- anchor
			Subject:      s.Subject,
--- replace
			Subject:      "",
--- end

# A job without a file table must be refused, not seeded with zero rows.
[the seed accepts a job with no file table]
file internal/app/app.go
--- anchor
	if ft == nil {
		return fmt.Errorf("app: seed job_files for %s: job has no file table", j.ID())
--- replace
	if ft == nil {
		return nil
--- end

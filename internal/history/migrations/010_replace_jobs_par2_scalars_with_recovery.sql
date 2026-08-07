-- Replace the jobs table's par2_bytes/par2_files with recovery_bytes and
-- recovery_files.
--
-- The old pair counted every file whose subject contained ".par2", which
-- includes the par2 index. The index is always downloaded and carries no
-- recovery blocks, so counting it overstated the job's repair capacity
-- everywhere the figure is read — including two gates that abort a job as
-- beyond repair. The new pair counts recovery volumes only.
--
-- This retracts the rationale recorded in 005_add_jobs_par2_scalars.sql,
-- which states these columns cannot be derived from job_files because
-- is_par2_recovery flags only recovery volumes and so would undercount. That
-- was true of the old definition and is false of the new one: the recovery
-- figures ARE exactly the is_par2_recovery aggregate. 005 is not edited, per
-- the rule against modifying an applied migration; this comment supersedes it.
--
-- The columns are kept anyway, for a different reason. SQLiteStore.Get's
-- job_files aggregate fails soft, logging and leaving its scalars at zero. A
-- zero recovery figure is not a missing reading — it is a definite claim that
-- the job has no repair capacity, which the UI renders as "No repair data"
-- and both abort gates read as grounds to declare a job hopeless. That must
-- not be reachable from a transient database error, so these two live on the
-- jobs row, which is read unconditionally and fails the load rather than
-- degrading.
--
-- The backfill is exact where it applies: recovery_bytes/recovery_files are
-- precisely the is_par2_recovery aggregate over job_files. It is not total —
-- addTx writes job_files rows only when a manifest is in hand, so a job
-- without rows lands on zero. Issue #318 records that landing this work
-- requires a full reset, so the backfill is belt-and-braces rather than a
-- migration guarantee.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE jobs ADD COLUMN recovery_bytes INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE jobs ADD COLUMN recovery_files INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE jobs SET
  recovery_bytes = (SELECT COALESCE(SUM(bytes), 0) FROM job_files
                    WHERE job_files.job_id = jobs.id AND job_files.is_par2_recovery = 1),
  recovery_files = (SELECT COUNT(*) FROM job_files
                    WHERE job_files.job_id = jobs.id AND job_files.is_par2_recovery = 1);
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE jobs DROP COLUMN par2_bytes;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE jobs DROP COLUMN par2_files;
-- +goose StatementEnd

-- Down restores the schema but not the values. The par2 index's bytes are not
-- recoverable from job_files, which flags recovery volumes only, so a
-- down-migrated row undercounts par2_bytes by exactly the index and
-- par2_files by one per par2 set. Restoring the recovery figures is the
-- closest reversal available; anything else would invent data.
-- +goose Down
-- +goose StatementBegin
ALTER TABLE jobs ADD COLUMN par2_bytes INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE jobs ADD COLUMN par2_files INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE jobs SET par2_bytes = recovery_bytes, par2_files = recovery_files;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE jobs DROP COLUMN recovery_bytes;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE jobs DROP COLUMN recovery_files;
-- +goose StatementEnd

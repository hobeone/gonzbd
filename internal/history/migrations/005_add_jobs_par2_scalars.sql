-- +goose Up
-- +goose StatementBegin
-- The manifest's par2 totals, kept on the job row so a non-resident job can
-- report them without loading its manifest.
--
-- These cannot be derived from job_files: is_par2_recovery flags only the
-- recovery volumes, while the manifest's Par2Bytes/Par2Files also count the
-- par2 index file, so aggregating the flag undercounts rather than matching.
-- That is why Job.setAggregateScalarsFromFiles deliberately leaves the pair
-- at zero, and why they need storage of their own.
ALTER TABLE jobs ADD COLUMN par2_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN par2_files INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE jobs DROP COLUMN par2_bytes;
ALTER TABLE jobs DROP COLUMN par2_files;
-- +goose StatementEnd

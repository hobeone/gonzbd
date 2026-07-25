-- +goose Up
-- +goose StatementBegin
CREATE TABLE queue_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE jobs (
    id         TEXT PRIMARY KEY,
    filename   TEXT NOT NULL,
    name       TEXT NOT NULL,
    password   TEXT,
    url        TEXT,
    category   TEXT,
    priority   INTEGER NOT NULL,
    status     TEXT NOT NULL,
    pp         INTEGER NOT NULL,
    script     TEXT,
    time_added INTEGER NOT NULL,
    md5        TEXT NOT NULL,
    avg_age    INTEGER NOT NULL,
    groups     TEXT,
    meta       TEXT,
    warning    TEXT,
    postproc   INTEGER NOT NULL DEFAULT 0,
    sort_key   INTEGER NOT NULL
);

CREATE TABLE job_files (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id           TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    file_index       INTEGER NOT NULL,
    subject          TEXT NOT NULL,
    date             INTEGER NOT NULL,
    bytes            INTEGER NOT NULL,
    is_par2_recovery INTEGER NOT NULL DEFAULT 0,
    complete         INTEGER NOT NULL DEFAULT 0,
    deferred         INTEGER NOT NULL DEFAULT 0,
    write_cursor     INTEGER NOT NULL DEFAULT 0,
    bytes_downloaded INTEGER NOT NULL DEFAULT 0,
    filename         TEXT,
    assembled_crc32  INTEGER DEFAULT 0,
    articles_done    TEXT,
    UNIQUE(job_id, file_index)
);

CREATE INDEX idx_jobs_sort_key ON jobs(sort_key);
CREATE INDEX idx_jobs_name ON jobs(name);
CREATE INDEX idx_jobs_md5 ON jobs(md5);
CREATE INDEX idx_job_files_job_id ON job_files(job_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE job_files;
DROP TABLE jobs;
DROP TABLE queue_meta;
-- +goose StatementEnd

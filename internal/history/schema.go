package history

// schema is applied once when the database is first created. Column names,
// types, order, and defaults are intentionally identical to the upstream
// Python sabnzbd schema so that users can point the Go daemon at an existing
// history1.db without a migration step.
const schema = `
CREATE TABLE IF NOT EXISTS history (
    id              INTEGER PRIMARY KEY,
    completed       INTEGER,
    name            TEXT,
    nzb_name        TEXT,
    category        TEXT,
    pp              TEXT,
    script          TEXT,
    report          TEXT,
    url             TEXT,
    status          TEXT,
    nzo_id          TEXT UNIQUE,
    storage         TEXT,
    path            TEXT,
    script_log      BLOB,
    script_line     TEXT,
    download_time   INTEGER,
    postproc_time   INTEGER,
    stage_log       TEXT,
    downloaded      INTEGER,
    completeness    INTEGER,
    fail_message    TEXT,
    url_info        TEXT,
    bytes           INTEGER,
    meta            TEXT,
    series          TEXT,
    md5sum          TEXT,
    password        TEXT,
    duplicate_key   TEXT,
    archive         INTEGER DEFAULT 0,
    time_added      INTEGER
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_history_nzo_id ON history(nzo_id);
CREATE INDEX IF NOT EXISTS idx_history_archive_completed ON history(archive, completed DESC);
`

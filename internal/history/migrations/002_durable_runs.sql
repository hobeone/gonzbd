-- Adds the durable_runs store described in
-- docs/superpowers/specs/2026-08-22-single-durability-record-design.md.
--
-- This migration ONLY CREATEs. article_facts and file_extents stay in place
-- and keep their writers; nothing here reads durable_runs or failed_articles
-- yet. A later migration (003) drops the two old tables once every reader
-- and writer has moved over — a single migration that both creates the
-- replacement and drops the original cannot be half-applied, and the split
-- is what makes that later flip possible to land as its own commit.
--
-- +goose Up
-- +goose StatementBegin
CREATE TABLE durable_runs (
    job_id        TEXT    NOT NULL,
    file_idx      INTEGER NOT NULL,
    first_art_idx INTEGER NOT NULL,
    last_art_idx  INTEGER NOT NULL,
    offset        INTEGER NOT NULL,
    length        INTEGER NOT NULL,
    crc32         INTEGER NOT NULL,
    PRIMARY KEY (job_id, file_idx, offset)
) WITHOUT ROWID;
-- One row per maximal run of articles that abut in both byte offset and
-- article index and were made durable together (fsynced, then recorded in
-- the same transaction as the barrier's commit). Replaces the pairing of
-- article_facts (Class A) and file_extents (Class B) with a single record
-- written only after the fsync that makes it true — see the design doc's
-- §1-§2 for why the two-writer shape was the source of #389 and #421.
--
-- Last-write-wins, not append-only: merging is read-modify-write by
-- construction (§3.1), so unlike article_facts's INSERT OR IGNORE, a commit
-- here may delete and replace rows it owns. That is safe because a row is
-- only ever written after a completed fsync, never before or during one, so
-- a later commit can only describe MORE durable bytes than an earlier one,
-- not different ones.
--
-- first_art_idx and last_art_idx name which articles this run accounts for;
-- the resume set is the complement. offset and length bound the completion
-- truncate (max(offset+length)) and the overlap check (Σ length vs stat
-- size, §3.3). crc32 is combined left-to-right over the run's articles via
-- crc32util.Combine; when a file collapses to one row starting at offset 0,
-- that row's crc32 IS the whole-file CRC (§3.5) — no walk, no prefix state.
--
-- The store is the only place a run is ever built (RunStore.Commit takes
-- individual articles, not runs) — see internal/durability/run.go for why a
-- second call site constructing runs would make the required dedup
-- unreachable.
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE failed_articles (
    job_id  TEXT    NOT NULL,
    art_idx INTEGER NOT NULL,
    PRIMARY KEY (job_id, art_idx)
) WITHOUT ROWID;
-- Owned and written solely by internal/queue, not by anything in this
-- package. It exists as a table rather than a packed bitmap column on
-- job_files because its reversal (ResetForRetry, resetForReload) is
-- per-article and per-job — a bitmap column can only express that by
-- rewriting the whole blob, which is the articles_done fragility this
-- redesign is removing, not reintroducing here.
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE failed_articles;
DROP TABLE durable_runs;
-- +goose StatementEnd

package durability

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func newWrittenStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(openTestDB(t))
}

func TestApplyRecord_RowsRequireAJobFilesRow(t *testing.T) {
	t.Parallel()
	s := newWrittenStore(t)
	ctx := context.Background()
	if err := s.ApplyRecord(ctx, []RecordBatch{{
		JobID: "gone",
		Rows:  []WrittenRow{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 10, CRC32: 1}},
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.WrittenRows(ctx, "gone")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("rows for a job with no job_files row = %v, want none (the EXISTS guard)", got)
	}
}

func TestApplyRecord_ReplaceKeepsTheLatestWrite(t *testing.T) {
	t.Parallel()
	s := newWrittenStore(t)
	ctx := context.Background()
	if err := s.Admit(ctx, "j", []uint8{0}); err != nil {
		t.Fatal(err)
	}
	row := WrittenRow{FileIdx: 0, ArtIdx: 3, Offset: 100, Length: 10, CRC32: 1}
	for _, crc := range []uint32{1, 2} {
		row.CRC32 = crc
		if err := s.ApplyRecord(ctx, []RecordBatch{{JobID: "j", Rows: []WrittenRow{row}}}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.WrittenRows(ctx, "j")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CRC32 != 2 {
		t.Errorf("rows = %+v, want one row with crc 2 (INSERT OR REPLACE)", got)
	}
}

func TestApplyRecord_FileStateUpdatesJobFiles(t *testing.T) {
	t.Parallel()
	s := newWrittenStore(t)
	ctx := context.Background()
	if err := s.Admit(ctx, "j", []uint8{0, 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRecord(ctx, []RecordBatch{{JobID: "j",
		Files: []FileState{{FileIdx: 1, Complete: true, Filename: "b.bin", FetchPolicy: 2}},
	}}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.FileRows(ctx, "j")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Complete || rows[0].Filename != "" || rows[0].FetchPolicy != 0 {
		t.Errorf("file 0 changed: %+v", rows[0])
	}
	if !rows[1].Complete || rows[1].Filename != "b.bin" || rows[1].FetchPolicy != 2 {
		t.Errorf("file 1 = %+v, want complete b.bin policy 2", rows[1])
	}
}

func TestApplyRecord_VerdictDeletesAndClears(t *testing.T) {
	t.Parallel()
	s := newWrittenStore(t)
	ctx := context.Background()
	if err := s.Admit(ctx, "j", []uint8{0}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRecord(ctx, []RecordBatch{{JobID: "j",
		Rows:  []WrittenRow{{0, 0, 0, 10, 1}, {0, 1, 10, 10, 2}},
		Files: []FileState{{FileIdx: 0, Complete: true, Filename: "a.bin"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRecord(ctx, []RecordBatch{{JobID: "j",
		Verdicts: []FileVerdict{{FileIdx: 0, DeleteAll: true, ClearComplete: true}},
	}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.WrittenRows(ctx, "j"); len(got) != 0 {
		t.Errorf("rows after DeleteAll = %+v", got)
	}
	rows, err := s.FileRows(ctx, "j")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Complete {
		t.Error("complete survived ClearComplete")
	}
}

func TestApplyRecord_VerdictDeletesNamedArticlesAndSetsComplete(t *testing.T) {
	t.Parallel()
	s := newWrittenStore(t)
	ctx := context.Background()
	if err := s.Admit(ctx, "j", []uint8{0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRecord(ctx, []RecordBatch{{JobID: "j",
		Rows: []WrittenRow{{0, 0, 0, 10, 1}, {0, 1, 10, 10, 2}, {1, 5, 0, 10, 3}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRecord(ctx, []RecordBatch{{JobID: "j",
		Verdicts: []FileVerdict{{FileIdx: 0, DeleteArtIdxs: []int32{1}, SetComplete: true}},
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.WrittenRows(ctx, "j")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ArtIdx != 0 || got[1].FileIdx != 1 {
		t.Errorf("rows = %+v, want file 0 art 0 and file 1 art 5", got)
	}
	rows, err := s.FileRows(ctx, "j")
	if err != nil {
		t.Fatal(err)
	}
	if !rows[0].Complete || rows[1].Complete {
		t.Errorf("complete = %v/%v, want true/false", rows[0].Complete, rows[1].Complete)
	}
}

func TestApplyRecord_RejectsSetAndClearTogether(t *testing.T) {
	t.Parallel()
	s := newWrittenStore(t)
	ctx := context.Background()
	if err := s.Admit(ctx, "j", []uint8{0}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRecord(ctx, []RecordBatch{{JobID: "j",
		Verdicts: []FileVerdict{{FileIdx: 0, SetComplete: true, ClearComplete: true}},
	}}); err == nil {
		t.Error("ApplyRecord with SetComplete and ClearComplete = nil, want an error")
	}
	rows, err := s.FileRows(ctx, "j")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Complete {
		t.Error("a rejected batch changed complete")
	}
}

// applyBatchIn runs applyBatch directly inside a transaction of s.
func applyBatchIn(t *testing.T, s *Store, b RecordBatch) error {
	t.Helper()
	return s.inTx(context.Background(), "test applyBatch", func(tx *sql.Tx) error {
		return applyBatch(context.Background(), tx, b)
	})
}

func TestApplyBatch_SetAndClearIsErrVerdictBothWays(t *testing.T) {
	t.Parallel()
	s := newWrittenStore(t)
	if err := s.Admit(context.Background(), "j", []uint8{0}); err != nil {
		t.Fatal(err)
	}
	err := applyBatchIn(t, s, RecordBatch{JobID: "j", Verdicts: []FileVerdict{{FileIdx: 0, SetComplete: true, ClearComplete: true}}})
	if !errors.Is(err, errVerdictBothWays) {
		t.Errorf("applyBatch = %v, want errVerdictBothWays", err)
	}
}

func TestApplyBatch_DeleteAllThenRowsKeepsTheRows(t *testing.T) {
	t.Parallel()
	s := newWrittenStore(t)
	ctx := context.Background()
	if err := s.Admit(ctx, "j", []uint8{0}); err != nil {
		t.Fatal(err)
	}
	if err := applyBatchIn(t, s, RecordBatch{JobID: "j", Rows: []WrittenRow{{0, 0, 0, 10, 1}}}); err != nil {
		t.Fatal(err)
	}
	err := applyBatchIn(t, s, RecordBatch{JobID: "j",
		Verdicts: []FileVerdict{{FileIdx: 0, DeleteAll: true}},
		Rows:     []WrittenRow{{0, 1, 10, 10, 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.WrittenRows(ctx, "j")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ArtIdx != 1 {
		t.Errorf("rows = %+v, want only the batch's own row (verdict deletes run before upserts)", got)
	}
}

func TestApplyBatch_RowForJobWithoutJobFilesIsNotInserted(t *testing.T) {
	t.Parallel()
	s := newWrittenStore(t)
	if err := applyBatchIn(t, s, RecordBatch{JobID: "gone", Rows: []WrittenRow{{0, 0, 0, 10, 1}}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.WrittenRows(context.Background(), "gone")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("rows = %+v, want none", got)
	}
}

func TestCompareWrittenRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		a, b WrittenRow
		want int
	}{
		{"lower offset first", WrittenRow{Offset: 1, ArtIdx: 9}, WrittenRow{Offset: 2, ArtIdx: 0}, -1},
		{"higher offset last", WrittenRow{Offset: 5, ArtIdx: 0}, WrittenRow{Offset: 2, ArtIdx: 9}, 1},
		{"equal offset: lower article first", WrittenRow{Offset: 3, ArtIdx: 1}, WrittenRow{Offset: 3, ArtIdx: 2}, -1},
		{"equal offset: higher article last", WrittenRow{Offset: 3, ArtIdx: 4}, WrittenRow{Offset: 3, ArtIdx: 2}, 1},
		{"equal", WrittenRow{Offset: 3, ArtIdx: 2, Length: 1}, WrittenRow{Offset: 3, ArtIdx: 2, Length: 9}, 0},
	} {
		if got := CompareWrittenRows(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: CompareWrittenRows = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestApplyRecord_StatementFailuresAreReturnedAndRolledBack makes each
// statement of a batch fail with a trigger and requires ApplyRecord to return
// the error, and the earlier batch of the same transaction to be rolled back.
func TestApplyRecord_StatementFailuresAreReturnedAndRolledBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		trigger string
		bad     RecordBatch
	}{
		{"row insert", `CREATE TRIGGER boom BEFORE INSERT ON written_articles BEGIN SELECT RAISE(ABORT, 'boom'); END`,
			RecordBatch{JobID: "j", Rows: []WrittenRow{{FileIdx: 0, ArtIdx: 1, Offset: 0, Length: 10, CRC32: 1}}}},
		{"delete all", `CREATE TRIGGER boom BEFORE DELETE ON written_articles BEGIN SELECT RAISE(ABORT, 'boom'); END`,
			RecordBatch{JobID: "j", Verdicts: []FileVerdict{{FileIdx: 0, DeleteAll: true}}}},
		{"delete named articles", `CREATE TRIGGER boom BEFORE DELETE ON written_articles BEGIN SELECT RAISE(ABORT, 'boom'); END`,
			RecordBatch{JobID: "j", Verdicts: []FileVerdict{{FileIdx: 0, DeleteArtIdxs: []int32{1}}}}},
		{"file state", `CREATE TRIGGER boom BEFORE UPDATE ON job_files WHEN NEW.filename = 'bad' BEGIN SELECT RAISE(ABORT, 'boom'); END`,
			RecordBatch{JobID: "j", Files: []FileState{{FileIdx: 0, Filename: "bad"}}}},
		{"verdict complete", `CREATE TRIGGER boom BEFORE UPDATE ON job_files WHEN NEW.complete = 1 BEGIN SELECT RAISE(ABORT, 'boom'); END`,
			RecordBatch{JobID: "j", Verdicts: []FileVerdict{{FileIdx: 0, SetComplete: true}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newWrittenStore(t)
			ctx := context.Background()
			if err := s.Admit(ctx, "j", []uint8{0}); err != nil {
				t.Fatal(err)
			}
			// A row for the delete triggers to fire on.
			if err := s.ApplyRecord(ctx, []RecordBatch{{JobID: "j",
				Rows: []WrittenRow{{FileIdx: 0, ArtIdx: 1, Offset: 0, Length: 10, CRC32: 1}}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, tc.trigger); err != nil {
				t.Fatal(err)
			}
			// good changes job_files.filename (not 'bad') and, for the file-state
			// cases, is the effect that must not survive the failing batch.
			good := RecordBatch{JobID: "j", Files: []FileState{{FileIdx: 0, Filename: "kept?"}}}
			err := s.ApplyRecord(ctx, []RecordBatch{good, tc.bad})
			if err == nil {
				t.Fatal("ApplyRecord = nil, want the statement's error")
			}
			if !strings.Contains(err.Error(), "boom") {
				t.Errorf("ApplyRecord = %v, want it to carry the failing statement's message", err)
			}
			var filename string
			if err := s.db.QueryRowContext(ctx,
				`SELECT filename FROM job_files WHERE job_id = 'j' AND file_index = 0`).Scan(&filename); err != nil {
				t.Fatal(err)
			}
			if filename == "kept?" {
				t.Errorf("filename = %q: the first batch survived a failed transaction", filename)
			}
		})
	}
}

func TestWrittenRows_ReturnsAQueryError(t *testing.T) {
	t.Parallel()
	s := newWrittenStore(t)
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := s.WrittenRows(context.Background(), "j")
	if err == nil || got != nil {
		t.Errorf("WrittenRows on a closed db = %v, %v, want nil and an error", got, err)
	}
}

func TestApplyRecord_CancelledContextReturnsAnErrorAndWritesNothing(t *testing.T) {
	t.Parallel()
	s := newWrittenStore(t)
	if err := s.Admit(context.Background(), "j", []uint8{0}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.ApplyRecord(ctx, []RecordBatch{{JobID: "j", Rows: []WrittenRow{{FileIdx: 0, ArtIdx: 0, Length: 10}}}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("ApplyRecord = %v, want context.Canceled", err)
	}
	got, err := s.WrittenRows(context.Background(), "j")
	if err != nil || len(got) != 0 {
		t.Errorf("rows = %v, %v, want none", got, err)
	}
}

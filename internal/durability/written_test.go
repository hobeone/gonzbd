package durability

import (
	"context"
	"testing"
)

func newWrittenStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(openTestDB(t), "history.db")
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

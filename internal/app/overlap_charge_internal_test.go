package app

import (
	"testing"
)

// writeFixtureArticleAt hands one 100-byte article of file 0 to the assembler
// at offset, then waits for the worker to process it.
func writeFixtureArticleAt(t *testing.T, application *Application, jobID string, globalArt int, offset int64) {
	t.Helper()
	j, ok := application.dispatcher.Job(jobID)
	if !ok {
		t.Fatalf("job %s not in the dispatcher", jobID)
	}
	if err := application.pipeline.registerFile(j, 0); err != nil {
		t.Fatalf("registerFile 0: %v", err)
	}
	ref, req := assemblerWrite(j, 0, globalArt, offset)
	if err := application.assembler.WriteArticle(t.Context(), ref, req); err != nil {
		t.Fatalf("WriteArticle %d: %v", globalArt, err)
	}
	if err := application.assembler.Quiesce(t.Context()); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
}

// TestOverlapRefusal_ChargesOnlyTheUncoveredBytes pins #432: an article the
// assembler refuses because its range intersects one another article already
// wrote is charged against par2 for the part of its range no written article
// covers, not for its whole size. Those covered bytes are on disk, and
// charging them can read a repairable job as beyond repair.
//
// Every fixture article is 100 bytes in the NZB and 100 bytes decoded.
// Article 0 writes [0,100) first.
func TestOverlapRefusal_ChargesOnlyTheUncoveredBytes(t *testing.T) {
	cases := map[string]struct {
		offset int64 // article 1's offset; its range is [offset, offset+100)
		want   int64
	}{
		"wholly covered":  {offset: 0, want: 0},
		"partly covered":  {offset: 40, want: 40},
		"barely touching": {offset: 99, want: 99},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			application, j := newDurabilityTestApp(t, 1, 3)
			writeFixtureArticleAt(t, application, j.ID(), 0, 0)
			if got := j.FailedBytes(); got != 0 {
				t.Fatalf("FailedBytes = %d after the first write, want 0", got)
			}
			writeFixtureArticleAt(t, application, j.ID(), 1, tc.offset)

			if got := j.FailedBytes(); got != tc.want {
				t.Errorf("FailedBytes = %d after article 1 at [%d,%d) was refused over "+
					"article 0's [0,100), want %d — only the bytes no written article "+
					"covers are owed to par2", got, tc.offset, tc.offset+100, tc.want)
			}
			if got := j.ContentFailedBytes(); got != tc.want {
				t.Errorf("ContentFailedBytes = %d, want %d", got, tc.want)
			}
			if got := j.Progress().FileFailedBytes(0); got != tc.want {
				t.Errorf("FileFailedBytes(0) = %d, want %d", got, tc.want)
			}
		})
	}
}

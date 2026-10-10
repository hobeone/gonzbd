package assembler

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCompletedFileIsTrimmedAndClosedBeforeItIsReported pins finalizeFile's
// order against a real file: the last part arrives, the file is fsynced,
// trimmed from its preallocated size to the end of its last written byte and
// closed, and only then reported complete.
func TestCompletedFileIsTrimmedAndClosedBeforeItIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job1_0.dat")
	// TotalParts 1: the single article below completes the file. ExpectedSize
	// preallocates past the written extent, which is what the trim removes.
	files := map[string]FileInfo{"job1:0": {Path: path, TotalParts: 1, ExpectedSize: 4096}}

	sizeAtReport := make(chan int64, 1)
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(string, int) {
		st, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat at report: %v", err)
			sizeAtReport <- -1
			return
		}
		sizeAtReport <- st.Size()
	}
	a := startAssembler(t, opts)

	if err := writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "a0",
		Offset: 0, Data: make([]byte, 100),
	}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	select {
	case got := <-sizeAtReport:
		if got != 100 {
			t.Errorf("file is %d bytes when reported complete, want 100 — the "+
				"report came before the trim, so a caller recording it complete "+
				"would record pre-allocation's trailing zeros", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the file never completed; the fixture is not exercising finalizeFile")
	}
}

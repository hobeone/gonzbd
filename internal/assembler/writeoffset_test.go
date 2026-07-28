package assembler

import (
	"fmt"
	"math"
	"os"
	"sync"
	"testing"
)

// A yEnc `=ypart begin=` value is attacker-controlled: it comes from the
// article body returned by the NNTP server, is parsed as an unbounded int64
// by the decoder, and reaches the assembler as WriteRequest.Offset. A hostile
// or compromised server can therefore ask us to WriteAt an arbitrary offset,
// producing a file whose apparent size is bounded only by int64.
//
// These tests pin the rejection at the assembler boundary, which is the only
// place FileInfo.ExpectedSize is in scope.

// registerSizedFile registers a FileInfo with an explicit ExpectedSize so the
// bounds check has something to check against. registerFile deliberately
// leaves ExpectedSize zero (meaning "unknown"), which disables the check.
func registerSizedFile(t *testing.T, dir string, files map[string]FileInfo, jobID string, fileIdx, totalParts int, expected int64) string {
	t.Helper()
	path := fmt.Sprintf("%s/%s_%d.dat", dir, jobID, fileIdx)
	files[fmt.Sprintf("%s:%d", jobID, fileIdx)] = FileInfo{
		Path:         path,
		TotalParts:   totalParts,
		ExpectedSize: expected,
	}
	return path
}

func TestWriteOffset_OutOfRangeRejected(t *testing.T) {
	const expectedSize = 4096

	tests := []struct {
		name       string
		offset     int64
		data       []byte
		wantReject bool
	}{
		{
			name:       "offset near MaxInt64 overflows end calculation",
			offset:     math.MaxInt64 - 1024,
			data:       []byte("HOSTILE!"),
			wantReject: true,
		},
		{
			name:       "negative offset",
			offset:     -1,
			data:       []byte("HOSTILE!"),
			wantReject: true,
		},
		{
			name:       "offset far beyond declared file size",
			offset:     expectedSize * 100,
			data:       []byte("HOSTILE!"),
			wantReject: true,
		},
		{
			name:       "legitimate offset within declared size is accepted",
			offset:     0,
			data:       []byte("GOODDATA"),
			wantReject: false,
		},
		{
			name:       "legitimate offset near the end is accepted",
			offset:     expectedSize - 8,
			data:       []byte("GOODDATA"),
			wantReject: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			files := make(map[string]FileInfo)
			path := registerSizedFile(t, dir, files, "job1", 0, 2, expectedSize)

			var mu sync.Mutex
			failed := make(map[string]bool)
			done := make(map[string]bool)

			opts := makeOpts(dir, files)
			opts.DoneFlushInterval = -1
			opts.MarkArticlesFailed = func(_ string, msgIDs []string) ([]string, error) {
				mu.Lock()
				defer mu.Unlock()
				for _, id := range msgIDs {
					failed[id] = true
				}
				return msgIDs, nil
			}
			opts.MarkArticlesDone = func(_ string, msgIDs []string) error {
				mu.Lock()
				defer mu.Unlock()
				for _, id := range msgIDs {
					done[id] = true
				}
				return nil
			}

			a := startAssembler(t, opts)
			const msgID = "under-test"
			_ = a.WriteArticle(t.Context(), WriteRequest{
				JobID: "job1", FileIdx: 0,
				Offset:    tc.offset,
				Data:      tc.data,
				MessageID: msgID,
			})
			if err := a.Stop(); err != nil {
				t.Fatalf("Stop: %v", err)
			}

			mu.Lock()
			gotFailed, gotDone := failed[msgID], done[msgID]
			mu.Unlock()

			if tc.wantReject {
				if !gotFailed {
					t.Errorf("offset %d: article was not recorded as failed (failed=%v done=%v); "+
						"an out-of-range write offset must route through the normal article-failure "+
						"path so par2 repair can still recover the file", tc.offset, gotFailed, gotDone)
				}
			} else if gotFailed {
				t.Errorf("offset %d: legitimate article was rejected", tc.offset)
			}

			// Regardless of bookkeeping, the file must never balloon to the
			// hostile offset. Preallocation may size it up to ExpectedSize.
			if fi, err := os.Stat(path); err == nil {
				if fi.Size() > expectedSize*2 {
					t.Errorf("offset %d: file size %d far exceeds ExpectedSize %d — "+
						"the out-of-range write was applied", tc.offset, fi.Size(), expectedSize)
				}
			}
		})
	}
}

// A rejected write leaves the file missing data, so the whole-file CRC must be
// reported as unavailable (zero) rather than computed from the parts that did
// land. FileInfo.ExpectedSize's doc and the crcValid field both specify that a
// failed article invalidates the file CRC.
//
// Without this, quickcheck reports the file as "CRC mismatch — corrupted"
// rather than "download had failures, CRC unavailable". Repair runs either way
// (both feed stage_quickcheck's `unverifiable` count), so this is a diagnostic
// correctness issue rather than a data-loss one — but the two states are
// materially different stories for an operator.
func TestWriteOffset_RejectedWriteInvalidatesFileCRC(t *testing.T) {
	const expectedSize = 4096

	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerSizedFile(t, dir, files, "job1", 0, 2, expectedSize)

	var mu sync.Mutex
	var gotCRC uint32
	var completions int

	opts := makeOpts(dir, files)
	opts.DoneFlushInterval = -1
	opts.OnFileComplete = func(_ string, _ int, fileCRC uint32) {
		mu.Lock()
		defer mu.Unlock()
		completions++
		gotCRC = fileCRC
	}
	opts.MarkArticlesFailed = func(_ string, ids []string) ([]string, error) { return ids, nil }
	opts.MarkArticlesDone = func(string, []string) error { return nil }

	a := startAssembler(t, opts)

	// Part 0 lands normally and contributes a CRC.
	_ = a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, Offset: 0,
		Data: []byte("GOODDATA"), MessageID: "part0", CRC: 0xDEADBEEF,
	})
	// Part 1 is rejected for an out-of-range offset.
	_ = a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, Offset: expectedSize * 100,
		Data: []byte("HOSTILE!"), MessageID: "part1", CRC: 0xCAFEBABE,
	})
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if completions == 0 {
		t.Fatal("file never completed — the test is not reaching finalizeFile")
	}
	if gotCRC != 0 {
		t.Errorf("file CRC = %#08x, want 0: part 1 was rejected, so the file is "+
			"missing data and its CRC must be reported as unavailable rather than "+
			"computed from the surviving parts", gotCRC)
	}
}

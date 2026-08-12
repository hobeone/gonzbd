package assembler

import (
	"fmt"
	"math"
	"os"
	"testing"
	"time"
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

			opts := makeOpts(dir, files)
			a := startAssembler(t, opts)
			target := a.SyncTargetFor("job1", nil)

			const msgID = "under-test"
			_ = a.WriteArticle(t.Context(), WriteRequest{
				JobID: "job1", FileIdx: 0,
				Offset:    tc.offset,
				Data:      tc.data,
				MessageID: msgID,
			})

			waitUntil(t, func() bool { return len(target.Files()) == 1 }, 2*time.Second, "file to open")

			// There is no ack to inspect any more (X2): whether the write
			// landed is read straight from the barrier's own evidence
			// surface — a rejected offset must never appear in Drain's
			// return, and a legitimate one must.
			got, err := target.Drain(t.Context(), 0)
			if err != nil {
				t.Fatalf("Drain: %v", err)
			}

			if err := a.Stop(); err != nil {
				t.Fatalf("Stop: %v", err)
			}

			present := len(got) == 1
			if tc.wantReject {
				if present {
					t.Errorf("offset %d: rejected article appears in Drain (%v); an "+
						"out-of-range write offset must never reach disk", tc.offset, got)
				}
			} else if !present {
				t.Errorf("offset %d: legitimate article is absent from Drain (%v)", tc.offset, got)
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

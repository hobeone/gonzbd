package nzb

import (
	"strings"
	"testing"
)

// File.Bytes feeds Manifest.FileBytes and then the assembler's ExpectedSize,
// which bounds every write to ExpectedSize plus 12.5%. Excluding a dropped
// duplicate's bytes shrinks that bound below the file's real size, so the
// LATER, legitimate parts of the same file are refused with "write extends
// past declared file size".
//
// The arithmetic is not marginal. Dropping k of N equally-sized segments
// leaves the bound at (N-k)/N x 1.125 of the true size, which falls below 1.0
// as soon as k/N exceeds one ninth — a single duplicate in a file of eight
// segments or fewer is enough, and par2 volumes are routinely that small.
//
// So the bytes are kept. The asymmetry decides it: File.Bytes is an estimate
// used as an upper bound, and over-stating it only loosens the bound, while
// under-stating it refuses data that genuinely belongs to the file.
func TestParse_DroppedDuplicateStillCountsTowardFileBytes(t *testing.T) {
	src := nzbWithSegments(seg(1, 100, "dup@h") + seg(2, 100, "dup@h") + seg(3, 100, "other@h"))

	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f := got.Files[0]
	if len(f.Articles) != 2 {
		t.Fatalf("len(Articles) = %d, want 2", len(f.Articles))
	}
	if f.Bytes != 300 {
		t.Errorf("File.Bytes = %d, want 300 — the dropped duplicate's bytes were "+
			"excluded, which shrinks the assembler's ExpectedSize bound below the "+
			"file's real size and refuses its later parts", f.Bytes)
	}
}

// The same accounting across files: a file reduced to nothing by duplicates is
// still skipped, and the surviving file's total is unaffected by what the other
// one lost.
func TestParse_DroppedDuplicateBytesDoNotLeakAcrossFiles(t *testing.T) {
	src := nzbWithSegments(
		seg(1, 100, "a@h")+seg(2, 100, "b@h"),
		seg(1, 100, "a@h")+seg(2, 100, "c@h"),
	)

	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Files) != 2 {
		t.Fatalf("len(Files) = %d, want 2", len(got.Files))
	}
	if got.Files[0].Bytes != 200 {
		t.Errorf("Files[0].Bytes = %d, want 200 — the first file lost nothing", got.Files[0].Bytes)
	}
	if got.Files[1].Bytes != 200 {
		t.Errorf("Files[1].Bytes = %d, want 200 — the duplicate a@h it dropped still "+
			"counts toward its own declared size", got.Files[1].Bytes)
	}
}

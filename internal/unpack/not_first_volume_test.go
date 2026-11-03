package unpack

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A middle volume named like a first one does not extract. two_members.part2
// holds only the continuation of second.txt; as obf.part1.rar it is the whole
// archive GoUnRAR is given. It must fail as a missing volume, and say so
// rather than blaming a corrupt header.
func TestGoUnRAR_MiddleVolumeNamedAsFirstFails(t *testing.T) {
	dir := t.TempDir()
	src := copyInto(t, "testdata/two_members.part2.rar", dir)
	main := filepath.Join(dir, "obf.part1.rar")
	if err := os.Rename(src, main); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	var lines []string
	opts := Options{GoRarFallback: false, OnLine: func(l string) { lines = append(lines, l) }}
	archive := Archive{Type: RarArchive, Name: "obf", MainFile: main, Parts: []string{main}}

	res, err := GoUnRAR(t.Context(), testLogger(), archive, outDir, "", opts)

	if err == nil {
		t.Fatalf("GoUnRAR on a middle volume = nil error (lines %q), want a failure", lines)
	}
	if res.Reason != FailMissingVolume {
		t.Errorf("Reason = %q, want %q", res.Reason, FailMissingVolume)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "not the first volume") }) ||
		slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "corrupt archive header") }) {
		t.Errorf("output lines = %q, want one naming the missing first volume and none blaming a corrupt header", lines)
	}
	if entries, err := os.ReadDir(outDir); err != nil || len(entries) != 0 {
		t.Errorf("output dir = %v, %v; want empty", entries, err)
	}
}

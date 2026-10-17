package par2

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeSetFile writes one par2 file describing a single entry under the given
// SetID and slice size, and returns the Set that parses it.
func writeSetFile(t *testing.T, dir, name string, setID [16]byte, sliceSize uint64) Set {
	t.Helper()
	fileID := [16]byte{9}
	body := payload(setID[0], 2048)
	content := buildPacket(setID, typeMain, buildMainBody(sliceSize, fileID))
	content = append(content, buildPacket(setID, typeFileDesc,
		buildFileDescBody(fileID, [16]byte{0xA0}, hash16kOf(body), uint64(len(body)), name+".rar"))...)
	path := filepath.Join(dir, name+".par2")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write par2: %v", err)
	}
	return Set{Name: name, MainFile: path}
}

type setSpec struct {
	id   byte
	size uint64
}

func TestIdentification_SliceSize(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		sets  []setSpec
		want  uint64
		wantK bool
	}{
		{"one set", []setSpec{{1, 16384}}, 16384, true},
		{"no sets", nil, 0, false},
		{"two sets, different ids and sizes", []setSpec{{1, 16384}, {2, 32768}}, 0, false},
		{"two sets, different ids and the same size", []setSpec{{1, 16384}, {2, 16384}}, 0, false},
		{"one SetID with two sizes", []setSpec{{1, 16384}, {1, 32768}}, 0, false},
		{"zero slice size", []setSpec{{1, 0}}, 0, false},
		{"one SetID in two groups", []setSpec{{1, 16384}, {1, 16384}}, 16384, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var sets []Set
			for i, s := range tc.sets {
				sets = append(sets, writeSetFile(t, dir, string(rune('a'+i)), [16]byte{s.id}, s.size))
			}
			id, err := Identify(dir, sets, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("Identify: %v", err)
			}
			got, ok := id.SliceSize()
			if ok != tc.wantK || got != tc.want {
				t.Errorf("SliceSize() = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantK)
			}
		})
	}
}

func TestAssessment_DeliveredIndices(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// c and d share a Hash16k, so only the whole-file CRC32 can place "zz-c".
	head := payload(21, 16*1024)
	c := append(append([]byte{}, head...), payload(22, 8*1024)...)
	d := append(append([]byte{}, head...), payload(23, 9*1024)...)
	a, b, e := payload(1, 20*1024), payload(2, 20*1024), payload(5, 20*1024)

	sets := par2SetForWithCRC(t, dir, map[string][]byte{
		"a.rar": a, "b.rar": b, "c.rar": c, "d.rar": d, "e.rar": e,
	})
	writeFile(t, dir, "a.rar", a)
	writeFile(t, dir, "zz-b", b)
	writeFile(t, dir, "zz-c", c)
	// Identified, but absent from the delivered list below.
	writeFile(t, dir, "e.rar", e)

	// Delivered order shares no prefix with the identification order, and the
	// files of interest are never at position 0.
	files := []AssembledFile{
		{FileName: "extra.bin"},
		{FileName: "zz-c", CRC32: 1},
		{FileName: "zz-b", CRC32: 1},
		{FileName: "a.rar", CRC32: 1},
	}
	a2, err := Assess(dir, sets, files, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}

	want := map[string]struct {
		by  MatchMethod
		pos int
	}{
		"a.rar": {MatchName, 3},
		"zz-b":  {MatchHash16k, 2},
		"zz-c":  {MatchCRC32, 1},
		"e.rar": {MatchName, -1},
	}
	got := a2.DeliveredIndices()
	if len(got) != len(a2.ID.Files) {
		t.Fatalf("DeliveredIndices has %d elements for %d identified files", len(got), len(a2.ID.Files))
	}
	if len(a2.ID.Files) != len(want) {
		t.Fatalf("identified %d files, want %d", len(a2.ID.Files), len(want))
	}
	for i, f := range a2.ID.Files {
		w, ok := want[f.OnDisk]
		if !ok {
			t.Fatalf("unexpected identified file %q", f.OnDisk)
		}
		if f.By != w.by {
			t.Errorf("%s matched by %v, want %v", f.OnDisk, f.By, w.by)
		}
		if got[i] != w.pos {
			t.Errorf("DeliveredIndices[%d] (%s) = %d, want %d (all: %v)", i, f.OnDisk, got[i], w.pos, got)
		}
	}
	if slices.Contains(got, 0) {
		t.Errorf("position 0 (extra.bin) was reported for an identified file: %v", got)
	}
}

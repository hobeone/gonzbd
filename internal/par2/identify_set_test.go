package par2

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// Every entry an Identification reports carries the par2 set it was read
// from, identified or not, so a caller can judge the sets one at a time.
// test/fixtures/par2/layout_b_mixed holds two sets: extras.par2 protects
// extras.txt, and withnfo.par2 protects feature.bin and feature.nfo.
func TestIdentify_RecordsEachEntrysSet(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, name := range []string{"extras.par2", "extras.txt", "withnfo.par2", "feature.nfo"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "par2", "layout_b_mixed", name))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, dir, name, data)
	}
	sets, err := FindPar2Files(dir)
	if err != nil {
		t.Fatal(err)
	}

	id := identifyIn(t, dir, sets)

	for onDisk, want := range map[string]string{"extras.txt": "extras", "feature.nfo": "withnfo"} {
		f, ok := byOnDisk(id, onDisk)
		if !ok {
			t.Errorf("%s was not identified", onDisk)
			continue
		}
		if f.Desc.Set != want {
			t.Errorf("%s identified from set %q, want %q", onDisk, f.Desc.Set, want)
		}
	}
	if len(id.Unaccounted) != 1 || id.Unaccounted[0].FileName != "feature.bin" || id.Unaccounted[0].Set != "withnfo" {
		t.Errorf("Unaccounted = %+v, want feature.bin from set withnfo", id.Unaccounted)
	}
}

// CRCExcluding reads the verdict over the sets not named, from the same
// identification and assembled CRCs as the Assessment's own.
func TestAssessment_CRCExcluding(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	files := []AssembledFile{
		{FileName: "extras.txt", CRC32: 0x11111111},
		{FileName: "feature.nfo", CRC32: 0x22222222},
	}
	id := Identification{
		Files: []Identified{
			{OnDisk: "extras.txt", Desc: FileDesc{FileName: "extras.txt", FileCRC32: 0x11111111, Set: "extras"}},
			{OnDisk: "feature.nfo", Desc: FileDesc{FileName: "feature.nfo", FileCRC32: 0x99999999, Set: "withnfo"}},
		},
		Unaccounted: []FileDesc{{FileName: "feature.bin", Set: "withnfo"}},
	}
	a := Assessment{ID: id, CRC: verifyIdentified(id, files, log), files: files}

	if got := a.CRCExcluding(nil, log); got.Matched != a.CRC.Matched || got.Mismatched != a.CRC.Mismatched || got.Unverified != a.CRC.Unverified {
		t.Errorf("CRCExcluding(nil) = %+v, want a.CRC %+v", got, a.CRC)
	}
	if a.CRC.Mismatched != 1 || a.CRC.Unverified != 1 {
		t.Fatalf("a.CRC = %+v, want the withnfo set's mismatch and unaccounted entry in it", a.CRC)
	}

	got := a.CRCExcluding(map[string]bool{"withnfo": true}, log)
	if got.Checked != 1 || got.Matched != 1 || got.Mismatched != 0 || got.Unverified != 0 || got.NoCRC != 0 {
		t.Errorf("CRCExcluding(withnfo) = %+v, want only extras.txt, checked and matched", got)
	}

	got = a.CRCExcluding(map[string]bool{"extras": true}, log)
	if got.Matched != 0 || got.Mismatched != 1 || got.Unverified != 1 {
		t.Errorf("CRCExcluding(extras) = %+v, want withnfo's mismatch and unaccounted entry", got)
	}
}

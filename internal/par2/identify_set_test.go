package par2

import (
	"encoding/binary"
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

// A set on disk that contributes no entry is reported as Unreadable, beside
// the sets that did, whether it holds no packet at all or a truncated one.
func TestIdentify_ReportsUnreadableSets(t *testing.T) {
	t.Parallel()

	for name, junk := range map[string][]byte{
		"zero-filled":      make([]byte, 4096),
		"truncated header": []byte("PAR2\x00PKT\x40\x00"),
		// ParsePar2SetWithOptions reaches EOF before any packet and returns an
		// empty set without an error.
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, f := range []string{"extras.par2", "extras.txt"} {
				data, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "par2", "layout_b_mixed", f))
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, dir, f, data)
			}
			writeFile(t, dir, "junk.par2", junk)
			sets, err := FindPar2Files(dir)
			if err != nil {
				t.Fatal(err)
			}

			id := identifyIn(t, dir, sets)

			if len(id.Unreadable) != 1 || id.Unreadable[0] != "junk" {
				t.Errorf("Unreadable = %q, want [junk]", id.Unreadable)
			}
			if _, ok := byOnDisk(id, "extras.txt"); !ok {
				t.Error("extras.txt was not identified from the readable set")
			}
			if _, ok := id.SliceSize(); !ok {
				t.Error("SliceSize refused: the unreadable set must not count as a second set")
			}
		})
	}

	t.Run("alone", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, dir, "data.par2", make([]byte, 4096))
		writeFile(t, dir, "data.bin", []byte("payload"))
		sets, err := FindPar2Files(dir)
		if err != nil {
			t.Fatal(err)
		}
		id := identifyIn(t, dir, sets)
		if len(id.Files) != 0 || len(id.Unaccounted) != 0 || len(id.Unreadable) != 1 || id.Unreadable[0] != "data" {
			t.Errorf("identification = %+v, want only Unreadable [data]", id)
		}
	})
}

// A set whose main file describes nothing is read from a volume, which repeats
// the packets the index lost (#1006). Without a readable volume it stays
// unreadable.
func TestFindPar2Files_ReadsAVolumeWhenTheIndexDescribesNothing(t *testing.T) {
	t.Parallel()

	fixture := func(t *testing.T, dir string, names ...string) {
		t.Helper()
		for _, f := range names {
			data, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "par2", "layout_b_mixed", f))
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, dir, f, data)
		}
	}

	for name, index := range map[string][]byte{
		"zero-filled": make([]byte, 4096),
		"empty":       {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			fixture(t, dir, "extras.txt", "extras.vol0+1.par2")
			writeFile(t, dir, "extras.par2", index)

			sets, err := FindPar2Files(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(sets) != 1 || !sets[0].Readable() || filepath.Base(sets[0].ParseFile()) != "extras.vol0+1.par2" {
				t.Fatalf("sets = %+v, want one readable set parsed from extras.vol0+1.par2", sets)
			}

			id := identifyIn(t, dir, sets)
			if len(id.Unreadable) != 0 {
				t.Errorf("Unreadable = %q, want none: the volume describes the set", id.Unreadable)
			}
			if _, ok := byOnDisk(id, "extras.txt"); !ok {
				t.Error("extras.txt was not identified from the volume")
			}

			res, err := GoVerify(t.Context(), discardLogger(), sets[0].ParseFile(), candidatesIn(t, dir), nil)
			if err != nil {
				t.Fatalf("GoVerify from the volume: %v", err)
			}
			if res.Status != StatusAllFilesOK {
				t.Errorf("GoVerify status = %v, want all files OK", res.Status)
			}
		})
	}

	t.Run("a readable index is preferred to a volume", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		fixture(t, dir, "extras.txt", "extras.par2", "extras.vol0+1.par2")
		sets, err := FindPar2Files(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(sets) != 1 || filepath.Base(sets[0].ParseFile()) != "extras.par2" {
			t.Fatalf("sets = %+v, want the set parsed from extras.par2", sets)
		}
	})

	t.Run("no readable file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		fixture(t, dir, "extras.txt")
		writeFile(t, dir, "extras.par2", make([]byte, 4096))
		writeFile(t, dir, "extras.vol0+1.par2", nil) // parses, without error, to nothing
		sets, err := FindPar2Files(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(sets) != 1 || sets[0].Readable() || filepath.Base(sets[0].ParseFile()) != "extras.par2" {
			t.Fatalf("sets = %+v, want one unreadable set that still names its main file", sets)
		}
		if id := identifyIn(t, dir, sets); len(id.Unreadable) != 1 {
			t.Errorf("Unreadable = %q, want [extras]", id.Unreadable)
		}
	})

	// An index cut short after its Main packet still lists the files but
	// describes none of them; a volume that describes them is read instead.
	mainOnly := func(t *testing.T) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "par2", "layout_b_mixed", "extras.par2"))
		if err != nil {
			t.Fatal(err)
		}
		var out []byte
		for len(data) >= 64 {
			n := binary.LittleEndian.Uint64(data[8:16])
			if n < 64 || n > uint64(len(data)) {
				t.Fatalf("fixture packet length %d", n)
			}
			if [16]byte(data[48:64]) == typeMain {
				out = append(out, data[:n]...)
			}
			data = data[n:]
		}
		if len(out) == 0 {
			t.Fatal("fixture has no Main packet")
		}
		return out
	}
	t.Run("an index with only its Main packet gives way to a volume", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		fixture(t, dir, "extras.txt", "extras.vol0+1.par2")
		writeFile(t, dir, "extras.par2", mainOnly(t))
		sets, err := FindPar2Files(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(sets) != 1 || filepath.Base(sets[0].ParseFile()) != "extras.vol0+1.par2" {
			t.Fatalf("sets = %+v, want the set parsed from extras.vol0+1.par2", sets)
		}
	})
	t.Run("an index with only its Main packet and no volume is still read", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		fixture(t, dir, "extras.txt")
		writeFile(t, dir, "extras.par2", mainOnly(t))
		sets, err := FindPar2Files(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(sets) != 1 || !sets[0].Readable() || filepath.Base(sets[0].ParseFile()) != "extras.par2" {
			t.Fatalf("sets = %+v, want a readable set parsed from extras.par2", sets)
		}
	})
}

// A recovery volume repeats each File Description and IFSC packet once per
// recovery block it holds; a parse keeps each file once.
func TestParsePar2Set_KeepsEachFileOnce(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("..", "..", "test", "fixtures", "par2", "layout_a")
	index, err := ParsePar2Set(filepath.Join(dir, "Real.Name.par2"))
	if err != nil {
		t.Fatal(err)
	}
	vol, err := ParsePar2Set(filepath.Join(dir, "Real.Name.vol0+2.par2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Files) == 0 || len(vol.Files) != len(index.Files) {
		t.Errorf("volume parses to %d files, index to %d: want the same, and at least one", len(vol.Files), len(index.Files))
	}
}

// With untilDescribed a parse stops at the first File Description, before
// the recovery data that follows it in a volume.
func TestParsePar2Set_UntilDescribedStopsAtTheFirstFileDescription(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "test", "fixtures", "par2", "layout_a", "Real.Name.vol0+2.par2")
	full, err := parsePar2Set(path, DefaultParseOptions(), false)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := parsePar2Set(path, DefaultParseOptions(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.Files) != 1 || probe.RecoveryBlocks >= full.RecoveryBlocks {
		t.Errorf("probe read %d files and %d recovery blocks, full parse %d blocks: want one file and fewer blocks",
			len(probe.Files), probe.RecoveryBlocks, full.RecoveryBlocks)
	}
}

// resolveReadable takes the first candidate with a File Description, else the
// first that describes a file through its Main packet alone, else nothing.
func TestSet_ResolveReadable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vol := filepath.Join("..", "..", "test", "fixtures", "par2", "layout_b_mixed", "extras.vol0+1.par2")
	junk := filepath.Join(dir, "junk.par2")
	writeFile(t, dir, "junk.par2", make([]byte, 4096))
	for name, tc := range map[string]struct {
		s    Set
		want string
	}{
		"a volume after junk":   {Set{MainFile: junk, ExtraFiles: []string{vol}}, vol},
		"a volume with no main": {Set{ExtraFiles: []string{vol}}, vol},
		"junk only":             {Set{MainFile: junk}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tc.s.resolveReadable(DefaultParseOptions())
			if tc.s.readable != tc.want {
				t.Errorf("readable = %q, want %q", tc.s.readable, tc.want)
			}
		})
	}
}

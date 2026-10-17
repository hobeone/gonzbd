package par2

import (
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// partialSetID is the SetID every fixture in this file writes, so two files
// built here are copies of one par2 set.
var partialSetID = [16]byte{5, 5, 5, 5}

// partialPacket is one file's File Description packet, followed by its IFSC
// packet when withIFSC is set, as par2SetForWithCRC writes them for the
// file's position i among the set's sorted names.
func partialPacket(i int, name string, body []byte, withIFSC bool) []byte {
	id := [16]byte{byte(i + 1)}
	out := buildPacket(partialSetID, typeFileDesc,
		buildFileDescBody(id, [16]byte{0xC0 + byte(i)}, hash16kOf(body), uint64(len(body)), name))
	if withIFSC {
		out = append(out, buildPacket(partialSetID, typeIFSC, buildIFSCBody(id, ifscSlicesFor(body)))...)
	}
	return out
}

// writePartialCopy writes name as a copy of partialSetID whose Main packet
// declares nDeclared files and which carries packets for the files given.
func writePartialCopy(t *testing.T, dir, name string, nDeclared int, packets ...[]byte) {
	t.Helper()
	ids := make([][16]byte, nDeclared)
	for i := range ids {
		ids[i] = [16]byte{byte(i + 1)}
	}
	content := buildPacket(partialSetID, typeMain, buildMainBody(testSliceSize, ids...))
	for _, p := range packets {
		content = append(content, p...)
	}
	writeFile(t, dir, name, content)
}

// A damaged copy of a par2 set that sorts before the complete one must not
// shadow the descriptions only the complete one carries. Here aaa.par2 lost
// b.rar's packets, and b.rar was delivered corrupt: its CRC can only be
// compared if set.par2's description of it is collected.
func TestCollectManifests_PartialDuplicateKeepsTheOtherCopysEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, b := payload(1, 20*1024), payload(2, 20*1024)
	par2SetForWithCRC(t, dir, map[string][]byte{"a.rar": a, "b.rar": b})
	writePartialCopy(t, dir, "aaa.par2", 2, partialPacket(0, "a.rar", a, true))
	writeFile(t, dir, "a.rar", a)
	writeFile(t, dir, "b.rar", b)

	sets, err := FindPar2Files(dir)
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}
	if len(sets) != 2 || sets[0].Name != "aaa" {
		t.Fatalf("FindPar2Files = %+v, want the partial copy aaa first (fixture guard)", sets)
	}
	log := slog.New(slog.DiscardHandler)

	if got := collectManifests(sets, log); len(got) != 2 {
		t.Fatalf("collected %d entries, want 2: %+v", len(got), got)
	}

	files := []AssembledFile{
		{FileName: "a.rar", CRC32: crc32.ChecksumIEEE(a)},
		{FileName: "b.rar", CRC32: crc32.ChecksumIEEE(b) ^ 1},
	}
	as, err := AssessWithOptions(dir, sets, files, log, DefaultParseOptions())
	if err != nil {
		t.Fatalf("AssessWithOptions: %v", err)
	}
	if as.CRC.Mismatched != 1 || as.CRC.Matched != 1 || len(as.ID.Unaccounted) != 0 {
		t.Errorf("CRC = %+v, Unaccounted = %v; want b.rar mismatched, a.rar matched, none unaccounted",
			as.CRC, as.ID.Unaccounted)
	}
	if _, ok := as.ID.SliceSize(); !ok {
		t.Error("SliceSize not ok for two copies of one set")
	}
}

// Where one copy carries a file's description without its checksums, the
// copy that carries them is the one collected, whichever sorts first, and the
// entry keeps the Set name of the copy read first.
func TestCollectManifests_PrefersTheCopyWithChecksums(t *testing.T) {
	t.Parallel()
	a := payload(1, 20*1024)
	for _, tc := range []struct{ partial, wantSet string }{
		{"aaa.par2", "aaa"},
		{"zzz.par2", "set"},
	} {
		t.Run(tc.partial, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			par2SetForWithCRC(t, dir, map[string][]byte{"a.rar": a})
			writePartialCopy(t, dir, tc.partial, 1, partialPacket(0, "a.rar", a, false))

			sets, err := FindPar2Files(dir)
			if err != nil {
				t.Fatalf("FindPar2Files: %v", err)
			}
			got := collectManifests(sets, slog.New(slog.DiscardHandler))
			if len(got) != 1 {
				t.Fatalf("collected %d entries, want 1: %+v", len(got), got)
			}
			if want := crc32.ChecksumIEEE(a); got[0].FileCRC32 != want {
				t.Errorf("FileCRC32 = %08x, want %08x from set.par2's IFSC packet", got[0].FileCRC32, want)
			}
			if got[0].Set != tc.wantSet {
				t.Errorf("Set = %q, want %q", got[0].Set, tc.wantSet)
			}
		})
	}
}

// A file the Main packet lists in the recovery set, but that no copy of the
// set describes, is Unaccounted: its name and size are lost with its packet,
// so nothing delivered can be shown to be it.
func TestIdentify_DeclaredButUndescribedIsUnaccounted(t *testing.T) {
	t.Parallel()
	a := payload(1, 20*1024)
	log := slog.New(slog.DiscardHandler)

	t.Run("one of two described", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writePartialCopy(t, dir, "set.par2", 2, partialPacket(0, "a.rar", a, true))
		writeFile(t, dir, "a.rar", a)
		sets, err := FindPar2Files(dir)
		if err != nil {
			t.Fatalf("FindPar2Files: %v", err)
		}
		id, err := IdentifyWithOptions(dir, sets, log, DefaultParseOptions())
		if err != nil {
			t.Fatalf("IdentifyWithOptions: %v", err)
		}
		if len(id.Files) != 1 || len(id.Unaccounted) != 1 {
			t.Fatalf("identified %d, unaccounted %v; want a.rar identified and one entry unaccounted", len(id.Files), id.Unaccounted)
		}
		if u := id.Unaccounted[0]; u.FileID != ([16]byte{2}) || u.Set != "set" || u.FileName == "" || !u.Undescribed {
			t.Errorf("unaccounted entry = %+v, want undescribed FileID 2 from set %q with a placeholder name", u, "set")
		}
	})

	t.Run("two copies, neither describes it", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writePartialCopy(t, dir, "set.par2", 2, partialPacket(0, "a.rar", a, true))
		writePartialCopy(t, dir, "other.par2", 2, partialPacket(0, "a.rar", a, true))
		writeFile(t, dir, "a.rar", a)
		sets, err := FindPar2Files(dir)
		if err != nil {
			t.Fatalf("FindPar2Files: %v", err)
		}
		id, err := IdentifyWithOptions(dir, sets, log, DefaultParseOptions())
		if err != nil {
			t.Fatalf("IdentifyWithOptions: %v", err)
		}
		if len(id.Unaccounted) != 1 {
			t.Errorf("Unaccounted = %v, want the one undescribed file once", id.Unaccounted)
		}
	})

	t.Run("none described", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writePartialCopy(t, dir, "set.par2", 1)
		writeFile(t, dir, "a.rar", a)
		sets, err := FindPar2Files(dir)
		if err != nil {
			t.Fatalf("FindPar2Files: %v", err)
		}
		id, err := IdentifyWithOptions(dir, sets, log, DefaultParseOptions())
		if err != nil {
			t.Fatalf("IdentifyWithOptions: %v", err)
		}
		if !id.NothingIdentified() || len(id.Unaccounted) != 1 {
			t.Errorf("Files %v, Unaccounted %v; want nothing identified and one entry unaccounted", id.Files, id.Unaccounted)
		}
	})
}

// recoveryFileIDs reads the recovery set's FileIDs from a Main packet body:
// the count, then that many IDs. Non-recovery IDs after them are not
// protected, and a count the body is too short for yields none.
func TestRecoveryFileIDs(t *testing.T) {
	// mainBodyOfN lists n recovery files, the first of them id1.
	mainBodyOfN := func(n int) []byte {
		ids := make([][16]byte, n)
		ids[0] = [16]byte{1}
		return buildMainBody(testSliceSize, ids...)
	}
	t.Parallel()
	id1, id2 := [16]byte{1}, [16]byte{2}
	tests := []struct {
		name string
		body []byte
		want int
	}{
		{"count matches", buildMainBodyWithCount(testSliceSize, 2, id1, id2), 2},
		{"a non-recovery id follows", buildMainBodyWithCount(testSliceSize, 1, id1, id2), 1},
		{"count beyond the body", buildMainBodyWithCount(testSliceSize, 3, id1, id2), 0},
		{"no count", buildMainBodyWithCount(testSliceSize, 0)[:8], 0},
		{"the PAR2 limit", mainBodyOfN(maxRecoverySetFiles), maxRecoverySetFiles},
		{"beyond the PAR2 limit", mainBodyOfN(maxRecoverySetFiles + 1), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "x.par2")
			if err := os.WriteFile(path, buildPacket(partialSetID, typeMain, tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			set, err := ParsePar2Set(path)
			if err != nil {
				t.Fatalf("ParsePar2Set: %v", err)
			}
			if len(set.RecoveryFileIDs) != tc.want {
				t.Errorf("RecoveryFileIDs = %v, want %d", set.RecoveryFileIDs, tc.want)
			}
			if tc.want > 0 && set.RecoveryFileIDs[0] != id1 {
				t.Errorf("RecoveryFileIDs[0] = %v, want %v", set.RecoveryFileIDs[0], id1)
			}
		})
	}
}

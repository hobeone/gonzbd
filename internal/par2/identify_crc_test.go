package par2

import (
	"crypto/md5" //nolint:gosec // par2 records MD5; matching it is the point
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const testSliceSize = 16 * 1024

// ifscSlicesFor derives the per-slice checksums for one file's content, for
// buildIFSCBody (parser_test.go) to serialize.
//
// par2 stores each slice's CRC32 over the slice ZERO-PADDED to the set's slice
// size, which is what reconstructCRCs then unpads for the tail. Building the
// fixture the same way is what makes the reconstructed FileCRC32 equal the
// whole file's CRC32 — get this wrong and the fixture silently tests nothing,
// because an entry with FileCRC32 == 0 is skipped by pass 3 rather than failed.
// TestIdentify_FixtureActuallyCarriesCRCs is the guard against exactly that.
func ifscSlicesFor(content []byte) []ifscSlice {
	out := make([]ifscSlice, 0, (len(content)/testSliceSize)+1)
	for off := 0; off < len(content); off += testSliceSize {
		end := min(off+testSliceSize, len(content))
		slice := make([]byte, testSliceSize)
		copy(slice, content[off:end])
		out = append(out, ifscSlice{
			md5Hash: md5.Sum(slice), //nolint:gosec // par2 compatibility
			crc32:   crc32.ChecksumIEEE(slice),
		})
	}
	return out
}

// par2SetForWithCRC is par2SetFor plus IFSC packets, so the resulting FileDescs
// carry a reconstructed FileCRC32. Identify's third pass is gated on that
// being non-zero, so a fixture without it cannot exercise the pass at all.
func par2SetForWithCRC(t *testing.T, dir string, entries map[string][]byte) []Set {
	t.Helper()

	setID := [16]byte{5, 5, 5, 5}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	slices.Sort(names)

	ids := make([][16]byte, len(names))
	for i := range names {
		ids[i] = [16]byte{byte(i + 1)}
	}

	content := buildPacket(setID, typeMain, buildMainBody(testSliceSize, ids...))
	for i, name := range names {
		body := entries[name]
		content = append(content, buildPacket(setID, typeFileDesc,
			buildFileDescBody(ids[i], [16]byte{0xC0 + byte(i)}, hash16kOf(body), uint64(len(body)), name))...)
		content = append(content, buildPacket(setID, typeIFSC, buildIFSCBody(ids[i], ifscSlicesFor(body)))...)
	}

	if err := os.WriteFile(filepath.Join(dir, "set.par2"), content, 0o600); err != nil {
		t.Fatalf("write par2: %v", err)
	}
	sets, err := FindPar2Files(dir)
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}
	if len(sets) == 0 {
		t.Fatalf("FindPar2Files found no sets in %s", dir)
	}
	return sets
}

func TestIdentify_FlattenedNameMatchesSubdirectoryEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	shot := payload(20, 20*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"Screens/shot.jpg": shot})
	// SanitizeFilename replaces "/" with "_" during download, so a par2 tree
	// can arrive flattened rather than nested.
	writeFile(t, dir, "Screens_shot.jpg", shot)

	id := identifyIn(t, dir, sets)

	got, ok := byOnDisk(id, "Screens_shot.jpg")
	if !ok {
		t.Fatalf("flattened name not identified; unaccounted = %+v", id.Unaccounted)
	}
	if got.By != MatchFlattenedName {
		t.Errorf("matched by %v, want flattened-name", got.By)
	}
	if !got.NeedsRename() {
		t.Error("NeedsRename() = false, but the file must move into Screens/")
	}
}

// TestIdentify_FixtureActuallyCarriesCRCs guards the fixture itself. Pass 3
// SKIPS any entry whose FileCRC32 is zero, so a broken IFSC builder would make
// the disambiguation test below pass for the wrong reason — by never running
// the pass it is meant to exercise.
func TestIdentify_FixtureActuallyCarriesCRCs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	body := payload(30, 40*1024)
	sets := par2SetForWithCRC(t, dir, map[string][]byte{"a.rar": body})

	descs, err := ParseFileDescriptions(sets[0].ParseFile())
	if err != nil {
		t.Fatalf("ParseFileDescriptions: %v", err)
	}
	if len(descs) != 1 {
		t.Fatalf("got %d descriptions, want 1", len(descs))
	}
	if want := crc32.ChecksumIEEE(body); descs[0].FileCRC32 != want {
		t.Fatalf("reconstructed FileCRC32 = %08x, want %08x — the IFSC fixture is wrong, "+
			"so any test relying on pass 3 is vacuous", descs[0].FileCRC32, want)
	}
}

func TestIdentify_WholeFileCRCDisambiguatesASharedHash16k(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Identical through the first 16 KB and divergent after, so Hash16k cannot
	// tell them apart but the whole-file CRC32 can. This is the only case
	// pass 3 exists for.
	head := payload(21, 16*1024)
	a := append(append([]byte{}, head...), payload(22, 8*1024)...)
	b := append(append([]byte{}, head...), payload(23, 9*1024)...)

	sets := par2SetForWithCRC(t, dir, map[string][]byte{"a.rar": a, "b.rar": b})
	writeFile(t, dir, "obfuscated-a", a)

	id := identifyIn(t, dir, sets)

	got, ok := byOnDisk(id, "obfuscated-a")
	if !ok {
		t.Fatalf("shared-Hash16k file was not identified at all; unaccounted = %+v", id.Unaccounted)
	}
	if got.By != MatchCRC32 {
		t.Errorf("matched by %v, want crc32 — Hash16k is ambiguous here", got.By)
	}
	if got.Desc.FileName != "a.rar" {
		t.Errorf("identified as %q, want a.rar", got.Desc.FileName)
	}
}

func TestIdentify_FileVanishingBetweenScanAndHashIsNotFatal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	a := payload(24, 40*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"movie.part1.rar": a})
	writeFile(t, dir, "obfuscated-one", a)

	// scanFlatFiles has already listed it; the hash read then fails. A
	// disappearing file must leave the entry unaccounted rather than error
	// out — the caller's answer is then "fetch the volumes", which is right.
	if err := os.Remove(filepath.Join(dir, "obfuscated-one")); err != nil {
		t.Fatal(err)
	}

	id, err := Identify(dir, sets, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Identify returned an error for a vanished file: %v", err)
	}
	if id.Accounted() {
		t.Error("Accounted() = true, but the only candidate is gone")
	}
}

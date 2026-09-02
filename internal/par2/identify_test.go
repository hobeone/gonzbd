package par2

import (
	"crypto/md5" //nolint:gosec // par2 records MD5; matching it is the point
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// payload returns deterministic bytes whose first 16 KB are unique to seed.
// Identification keys on the first 16 KB, so anything past that is padding
// and only exists to give files a realistic length.
func payload(seed byte, size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

func hash16kOf(b []byte) [16]byte {
	n := min(len(b), 16*1024)
	return md5.Sum(b[:n]) //nolint:gosec // par2 compatibility, not security
}

// par2SetFor writes a par2 index describing the given par2-path → content
// pairs, and returns the Sets FindPar2Files produces. Only Main and FileDesc
// packets are written: Identify reads FileName and Hash16k and nothing else,
// so omitting IFSC keeps the fixture honest about what is actually consulted.
func par2SetFor(t *testing.T, dir string, entries map[string][]byte) []Set {
	t.Helper()

	setID := [16]byte{7, 7, 7, 7}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	// Stable packet order, so a fixture cannot pass or fail by map ordering.
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}

	ids := make([][16]byte, len(names))
	for i := range names {
		ids[i] = [16]byte{byte(i + 1)}
	}

	content := buildPacket(setID, typeMain, buildMainBody(16384, ids...))
	for i, name := range names {
		body := entries[name]
		content = append(content, buildPacket(setID, typeFileDesc,
			buildFileDescBody(ids[i], [16]byte{0xA0 + byte(i)}, hash16kOf(body), uint64(len(body)), name))...)
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

func writeFile(t *testing.T, dir, name string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func identifyIn(t *testing.T, dir string, sets []Set) Identification {
	t.Helper()
	id, err := Identify(dir, sets, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	return id
}

// byOnDisk finds the identification for one on-disk name.
func byOnDisk(id Identification, name string) (Identified, bool) {
	for _, f := range id.Files {
		if f.OnDisk == name {
			return f, true
		}
	}
	return Identified{}, false
}

func TestIdentify_FlatSetWithCorrectNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	a, b := payload(1, 40*1024), payload(2, 40*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"movie.part1.rar": a, "movie.part2.rar": b})
	writeFile(t, dir, "movie.part1.rar", a)
	writeFile(t, dir, "movie.part2.rar", b)

	id := identifyIn(t, dir, sets)

	if !id.Accounted() {
		t.Errorf("Accounted() = false, unaccounted = %d", len(id.Unaccounted))
	}
	if len(id.Files) != 2 {
		t.Fatalf("identified %d files, want 2", len(id.Files))
	}
	// The property that makes un-gating safe: a correctly-named flat file is
	// identified WITHOUT implying a rename. The matchers this replaces would
	// have issued a self-move for each of these.
	for _, f := range id.Files {
		if f.By != MatchName {
			t.Errorf("%s matched by %v, want name", f.OnDisk, f.By)
		}
		if f.NeedsRename() {
			t.Errorf("%s reports NeedsRename for a file already correctly named (par2 path %q)",
				f.OnDisk, f.Desc.FileName)
		}
	}
}

func TestIdentify_ObfuscatedNamesMatchByContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	a, b := payload(3, 40*1024), payload(4, 40*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"movie.part1.rar": a, "movie.part2.rar": b})
	// What the probe found on a real release: random names, no extension, no
	// relationship whatsoever to the par2 names.
	writeFile(t, dir, "7xq6N6P340dCh9Lnih5hY3jsArfSN1", a)
	writeFile(t, dir, "XCsY92hrQfBOU5G7ldbC9xtmPQPKtf", b)

	id := identifyIn(t, dir, sets)

	if !id.Accounted() {
		t.Fatalf("Accounted() = false; obfuscated files are still the files par2 describes (unaccounted %d)",
			len(id.Unaccounted))
	}
	got, ok := byOnDisk(id, "7xq6N6P340dCh9Lnih5hY3jsArfSN1")
	if !ok {
		t.Fatalf("obfuscated file was not identified at all")
	}
	if got.By != MatchHash16k {
		t.Errorf("matched by %v, want hash16k", got.By)
	}
	if got.Desc.FileName != "movie.part1.rar" {
		t.Errorf("identified as %q, want movie.part1.rar", got.Desc.FileName)
	}
	if !got.NeedsRename() {
		t.Error("NeedsRename() = false for an obfuscated file; the rename is the whole point")
	}
}

func TestIdentify_SubdirectoryEntryMatchesFlatFileByBasename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	shot := payload(5, 20*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"Screens/shot.jpg": shot})
	writeFile(t, dir, "shot.jpg", shot)

	id := identifyIn(t, dir, sets)

	got, ok := byOnDisk(id, "shot.jpg")
	if !ok {
		t.Fatalf("flat file not identified against a subdirectory par2 entry")
	}
	if got.By != MatchName {
		t.Errorf("matched by %v, want name (basenames are equal)", got.By)
	}
	if !got.NeedsRename() {
		t.Error("NeedsRename() = false, but the file must move into Screens/")
	}
}

func TestIdentify_LayoutBLeavesEntriesUnaccounted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// par2 protects the extracted payload; the NZB delivered archives.
	sets := par2SetFor(t, dir, map[string][]byte{"movie.mkv": payload(6, 50*1024)})
	writeFile(t, dir, "movie.part1.rar", payload(7, 40*1024))
	writeFile(t, dir, "movie.part2.rar", payload(8, 40*1024))

	id := identifyIn(t, dir, sets)

	if id.Accounted() {
		t.Error("Accounted() = true, but nothing delivered is what par2 describes")
	}
	if len(id.Unaccounted) != 1 || id.Unaccounted[0].FileName != "movie.mkv" {
		t.Errorf("Unaccounted = %+v, want exactly movie.mkv", id.Unaccounted)
	}
	if len(id.Files) != 0 {
		t.Errorf("identified %d files against a set that describes none of them", len(id.Files))
	}
}

func TestIdentify_SkipsPar2OwnSidecars(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	a := payload(9, 40*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"movie.part1.rar": a})
	writeFile(t, dir, "7xq6N6P340dCh9Lnih5hY3jsArfSN1", a)
	// A sidecar whose content is identical to the protected file: if sidecars
	// were candidates, this could be claimed instead of the real file.
	writeFile(t, dir, "movie.nfo", a)

	id := identifyIn(t, dir, sets)

	for _, f := range id.Files {
		if f.OnDisk == "movie.nfo" {
			t.Fatalf("movie.nfo was identified as %q; sidecars are not candidates", f.Desc.FileName)
		}
	}
	got, ok := byOnDisk(id, "7xq6N6P340dCh9Lnih5hY3jsArfSN1")
	if !ok {
		t.Fatal("the real file was not identified")
	}
	if got.Desc.FileName != "movie.part1.rar" {
		t.Errorf("identified as %q, want movie.part1.rar", got.Desc.FileName)
	}
	// set.par2 and movie.nfo both.
	if len(id.Ignored) != 2 {
		t.Errorf("Ignored = %v, want the .par2 and the .nfo", id.Ignored)
	}
}

func TestIdentify_AmbiguousHash16kIdentifiesNeither(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Two par2 entries whose first 16 KB are identical. Content cannot tell
	// them apart, and guessing would hand par2 the wrong name for a file.
	same := payload(10, 40*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"a.rar": same, "b.rar": same})
	writeFile(t, dir, "obfuscated-one", same)

	id := identifyIn(t, dir, sets)

	for _, f := range id.Files {
		if f.By == MatchHash16k {
			t.Errorf("identified %s as %q by content, but two entries share that Hash16k",
				f.OnDisk, f.Desc.FileName)
		}
	}
	if id.Accounted() {
		t.Error("Accounted() = true despite an undecidable set")
	}
}

func TestIdentify_NoParSetsIsNotAnError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "movie.part1.rar", payload(11, 1024))

	id, err := Identify(dir, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Identify with no sets: %v", err)
	}
	// Vacuously accounted: there are no entries to account for. The caller
	// decides what that means; Identify does not editorialize.
	if !id.Accounted() {
		t.Error("Accounted() = false with no par2 entries at all")
	}
	if len(id.Files) != 0 {
		t.Errorf("identified %d files with no par2 sets", len(id.Files))
	}
}

func TestIdentify_DoesNotTouchTheFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	a := payload(12, 40*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"Screens/shot.jpg": a, "movie.mkv": payload(13, 20*1024)})
	writeFile(t, dir, "obfuscated-name", a)

	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	beforeNames := make([]string, len(before))
	for i, de := range before {
		beforeNames[i] = de.Name()
	}

	id := identifyIn(t, dir, sets)
	if len(id.Files) == 0 {
		t.Fatal("fixture is wrong: nothing was identified, so a no-mutation check proves nothing")
	}

	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(beforeNames) {
		t.Fatalf("directory changed: %d entries before, %d after", len(beforeNames), len(after))
	}
	for i, de := range after {
		if de.Name() != beforeNames[i] {
			t.Errorf("entry %d is %q, was %q — Identify must not move anything", i, de.Name(), beforeNames[i])
		}
	}
}

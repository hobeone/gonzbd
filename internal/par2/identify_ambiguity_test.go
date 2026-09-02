package par2

import (
	"testing"
)

// Three entries sharing a Hash16k must be refused, not crash.
//
// The first implementation poisoned the hash index with a sentinel -1 and read
// it back as manifest[prev] on the next duplicate, so the THIRD entry indexed
// the slice at -1 and panicked. Two duplicates never reached that read, which
// is why the two-entry test passed.
func TestIdentify_ThreeEntriesSharingAHash16kDoNotPanic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	same := payload(50, 40*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"a.rar": same, "b.rar": same, "c.rar": same})
	writeFile(t, dir, "obfuscated-one", same)

	id := identifyIn(t, dir, sets)

	for _, f := range id.Files {
		if f.By == MatchHash16k {
			t.Errorf("identified %s as %q by content, but three entries share that Hash16k",
				f.OnDisk, f.Desc.FileName)
		}
	}
	if len(id.Unaccounted) != 3 {
		t.Errorf("Unaccounted = %d, want all 3 entries left unidentified", len(id.Unaccounted))
	}
}

// A Hash16k match covers only the first 16 KB, so the length has to agree too.
//
// Without this check Identify reports a truncated file as accounted while
// relocateFile — which does check the size — refuses to move it, so the two
// halves of quickcheck disagree about the same file. Worse, the fetch decision
// would treat a truncated download as fully accounted.
func TestIdentify_Hash16kMatchWithWrongLengthIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	full := payload(51, 40*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"movie.part1.rar": full})
	// Same first 16 KB, truncated after — the shape of an interrupted write.
	writeFile(t, dir, "obfuscated-truncated", full[:20*1024])

	id := identifyIn(t, dir, sets)

	if len(id.Files) != 0 {
		t.Errorf("identified %+v; the first 16 KB agree but the file is half the recorded length", id.Files)
	}
	if id.Accounted() {
		t.Error("Accounted() = true for a truncated file")
	}
}

// A basename match is stronger evidence than a flattened-name match, so every
// basename must be tried before any flattened form.
//
// Running them per-entry instead lets an earlier entry's weak match consume a
// file that a later entry matches exactly.
func TestIdentify_BasenameBeatsFlattenedAcrossEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// "a/b.txt" flattens to "a_b.txt"; "Sub/a_b.txt" has basename "a_b.txt".
	// Both want the one file on disk, and the basename claim must win.
	weak := payload(52, 4*1024)
	strong := payload(53, 4*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"a/b.txt": weak, "Sub/a_b.txt": strong})
	writeFile(t, dir, "a_b.txt", strong)

	id := identifyIn(t, dir, sets)

	got, ok := byOnDisk(id, "a_b.txt")
	if !ok {
		t.Fatalf("a_b.txt was not identified at all; unaccounted = %+v", id.Unaccounted)
	}
	if got.Desc.FileName != "Sub/a_b.txt" {
		t.Errorf("a_b.txt identified as %q by %v; the basename match on Sub/a_b.txt is the stronger claim",
			got.Desc.FileName, got.By)
	}
	if got.By != MatchName {
		t.Errorf("matched by %v, want name", got.By)
	}
}

// Pass 3 has the same ambiguity problem as pass 2 and needs the same answer.
// Overwriting the first entry with the second would name a delivered file
// after whichever entry happened to come last in the manifest.
func TestIdentify_DuplicateCRCAndSizeIdentifiesNeither(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Byte-identical files: same Hash16k AND same CRC32+length, so neither
	// pass can separate them.
	same := payload(54, 40*1024)
	sets := par2SetForWithCRC(t, dir, map[string][]byte{"copy1.bin": same, "copy2.bin": same})
	writeFile(t, dir, "obfuscated-copy", same)

	id := identifyIn(t, dir, sets)

	for _, f := range id.Files {
		if f.By == MatchCRC32 {
			t.Errorf("identified %s as %q by CRC32, but two entries share that CRC and length",
				f.OnDisk, f.Desc.FileName)
		}
	}
	if id.Accounted() {
		t.Error("Accounted() = true for a set whose entries cannot be told apart")
	}
}

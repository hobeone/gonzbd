package par2

import (
	"log/slog"
	"testing"
)

// TestVerifyIdentified_AmbiguousBasename pins that two delivered files sharing
// a basename are refused rather than confused.
//
// "CD1/track01.flac" and "CD2/track01.flac" is an ordinary music release, and
// the join needs a basename fallback for the already-relocated case — so
// without an ambiguity check both files resolve to whichever CRC was inserted
// last, and an intact release reports a mismatch and is sent to par2 repair.
//
// Refusing is the rule Identify already applies to a shared Hash16k and a
// shared CRC32, and it lands on the conservative side: Unverified fetches the
// recovery volumes and lets par2 answer.
func TestVerifyIdentified_AmbiguousBasename(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)

	id := Identification{Files: []Identified{
		{OnDisk: "CD1/track01.flac", Desc: FileDesc{FileName: "CD1/track01.flac", FileCRC32: 0x11111111}},
		{OnDisk: "CD2/track01.flac", Desc: FileDesc{FileName: "CD2/track01.flac", FileCRC32: 0x22222222}},
	}}
	// The caller knows both only by their basename, which is the state a
	// filename field that cannot hold a path forces.
	files := []AssembledFile{
		{FileName: "track01.flac", CRC32: 0x11111111},
		{FileName: "track01.flac", CRC32: 0x22222222},
	}

	r := verifyIdentified(id, files, log)

	if r.Mismatched != 0 {
		t.Errorf("Mismatched = %d, want 0: both files are intact, and reporting corruption here sends a "+
			"healthy release to repair on the strength of a name collision", r.Mismatched)
	}
	if r.Unverified != 2 {
		t.Errorf("Unverified = %d, want 2: an ambiguous basename must be refused, not resolved by insertion "+
			"order", r.Unverified)
	}
}

// TestVerifyIdentified_ExactNameBeatsBasename pins that the exact match is
// tried first, so the ambiguity above is only reached when it has to be.
//
// Two files with the same basename in different directories, where the caller
// knows each by its full path, join exactly and both verify. Without the
// exact-first order they would collide on the basename and both be refused,
// turning a verifiable release into one that fetches its recovery set.
func TestVerifyIdentified_ExactNameBeatsBasename(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)

	id := Identification{Files: []Identified{
		{OnDisk: "CD1/track01.flac", Desc: FileDesc{FileName: "CD1/track01.flac", FileCRC32: 0x11111111}},
		{OnDisk: "CD2/track01.flac", Desc: FileDesc{FileName: "CD2/track01.flac", FileCRC32: 0x22222222}},
	}}
	files := []AssembledFile{
		{FileName: "CD1/track01.flac", CRC32: 0x11111111},
		{FileName: "CD2/track01.flac", CRC32: 0x22222222},
	}

	r := verifyIdentified(id, files, log)

	if r.Matched != 2 {
		t.Errorf("Matched = %d, want 2: each file names itself exactly, so the basename collision never "+
			"has to be consulted", r.Matched)
	}
	if r.Unverified != 0 || r.NotInPar2 != 0 {
		t.Errorf("Unverified=%d NotInPar2=%d, want 0 and 0", r.Unverified, r.NotInPar2)
	}
}

// TestApplyRenames_HonoursAFilteredRenameList pins that ApplyRenames acts on
// Assessment.Renames rather than re-deriving the moves from ID.Files.
//
// An earlier version iterated the identifications directly, which made
// Renames a second, parallel statement of the same thing: a caller that
// trimmed it was silently ignored, and the two could drift apart with nothing
// to notice. Filtering the list is the cheapest way to tell which one is
// actually consulted.
func TestApplyRenames_HonoursAFilteredRenameList(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	a := payload(21, 8*1024)
	b := payload(22, 8*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"Sub/a.bin": a, "Sub/b.bin": b})
	writeFile(t, dir, "a.bin", a)
	writeFile(t, dir, "b.bin", b)

	as, err := Assess(dir, sets, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if len(as.Renames) != 2 {
		t.Fatalf("fixture guard: %d renames planned, want 2", len(as.Renames))
	}

	// Keep exactly one.
	kept := as.Renames[:1]
	as.Renames = kept

	applied := ApplyRenames(dir, as, slog.New(slog.DiscardHandler))
	if len(applied) != 1 {
		t.Fatalf("applied %d renames, want 1: ApplyRenames must act on Renames, not re-derive them from "+
			"ID.Files", len(applied))
	}
	if applied[0] != kept[0] {
		t.Errorf("applied %+v, want %+v", applied[0], kept[0])
	}
}

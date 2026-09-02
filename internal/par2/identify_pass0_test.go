package par2

import (
	"crypto/md5" //nolint:gosec // par2 records MD5; matching it is the point
	"os"
	"path/filepath"
	"testing"
)

// TestIdentify_FindsAnEntryAlreadyAtItsPar2Path pins pass 0, and with it the
// idempotency of identification as a whole.
//
// scanFlatFiles reads one directory level and skips directories outright, so
// every name- and content-based pass is blind to anything inside a
// subdirectory. Without a pass that checks the par2 path itself, Identify
// answers differently the second time it is asked about the same directory:
// QuickCheck relocates "shot.jpg" to "Screens/shot.jpg" on the strength of the
// first answer, and the second reports that entry unaccounted.
//
// internal/app runs exactly that sequence — recordPar2Names then
// par2Verdict — so the stale answer made a healthy job with any
// subdirectory in its par2 set fetch its entire recovery volume set.
func TestIdentify_FindsAnEntryAlreadyAtItsPar2Path(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	body := payload(9, 40*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"Screens/shot.jpg": body})

	// The file where par2 says it should be — the post-relocation state.
	if err := os.MkdirAll(filepath.Join(dir, "Screens"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "Screens"), "shot.jpg", body)

	id := identifyIn(t, dir, sets)

	if !id.Accounted() {
		t.Fatalf("%d entr(y/ies) unaccounted though the file is at the exact path par2 names: %+v",
			len(id.Unaccounted), id.Unaccounted)
	}
	if len(id.Files) != 1 {
		t.Fatalf("identified %d files, want 1: %+v", len(id.Files), id.Files)
	}
	if id.Files[0].NeedsRename() {
		t.Errorf("NeedsRename() = true for a file already at %q; relocating it would move it onto itself",
			id.Files[0].Desc.FileName)
	}
}

// TestIdentify_RejectsAnEntryAtItsPar2PathWithTheWrongLength pins the length
// check on pass 0.
//
// Being at the right path is not evidence the file is whole. Claiming a
// truncated one would report the entry accounted while relocateFile — which
// does compare lengths — declines to touch it, so the two would disagree about
// the same file, and the job would skip a repair it needs.
func TestIdentify_RejectsAnEntryAtItsPar2PathWithTheWrongLength(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	body := payload(9, 40*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"Screens/shot.jpg": body})

	if err := os.MkdirAll(filepath.Join(dir, "Screens"), 0o750); err != nil {
		t.Fatal(err)
	}
	// The same first 16 KB — so this is not rejected for its content — but
	// truncated, which is exactly the shape a partial download leaves.
	writeFile(t, filepath.Join(dir, "Screens"), "shot.jpg", body[:20*1024])

	id := identifyIn(t, dir, sets)

	if id.Accounted() {
		t.Error("a truncated file at the par2 path was reported accounted; par2 repair is what fixes it, " +
			"and reporting it whole is what skips the fetch that enables the repair")
	}
	if len(id.Files) != 0 {
		t.Errorf("identified %+v, want none", id.Files)
	}
}

// TestComputeHash16k_EmptyFile pins that a 0-byte file hashes rather than
// erroring.
//
// io.ReadFull returns io.ErrUnexpectedEOF when it read SOME of the buffer and
// io.EOF when it read NONE, so treating only the former as success made an
// empty file fail to hash at all. Identify then logged "could not hash
// candidate" and skipped it, leaving a par2 entry for an empty file
// permanently unaccounted — and the job fetching recovery volumes over it.
func TestComputeHash16k_EmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "empty.bin", nil)

	got, err := ComputeHash16k(filepath.Join(dir, "empty.bin"))
	if err != nil {
		t.Fatalf("ComputeHash16k on a 0-byte file: %v", err)
	}
	if want := md5.Sum(nil); got != want { //nolint:gosec // par2 compatibility, not security
		t.Errorf("hash = %x, want %x (the MD5 of no bytes, which is what par2 records for an empty file)", got, want)
	}
}

// TestRelocateFile_AcceptsALeadingDoubleDotName pins that the traversal guard
// rejects escapes rather than names.
//
// The guard it replaced was filepath.Rel plus strings.HasPrefix(rel, ".."),
// which rejects any name merely BEGINNING with two dots — Rel(dir,
// dir/"..config.txt") is "..config.txt". fsutil.PathWithin tests ".." as a
// whole path element, which is the actual escape.
func TestRelocateFile_AcceptsALeadingDoubleDotName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	body := payload(3, 4*1024)
	writeFile(t, dir, "flat.bin", body)

	fd := FileDesc{FileName: "..config.txt", FileSize: uint64(len(body))}
	if !relocateFile(dir, "flat.bin", fd, nil) {
		t.Fatal("relocateFile refused a legitimate filename beginning with two dots")
	}
	if _, err := os.Stat(filepath.Join(dir, "..config.txt")); err != nil {
		t.Errorf("the file was not relocated: %v", err)
	}
}

// TestRelocateFile_RefusesWhatItCannotVerify pins the two ways relocateFile
// declines to move a file, both of which matter to Identify's agreement with
// it: a file Identify reports accounted but relocateFile refuses to touch
// leaves the two disagreeing about the same file.
func TestRelocateFile_RefusesWhatItCannotVerify(t *testing.T) {
	t.Parallel()

	t.Run("a source that is not there", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		fd := FileDesc{FileName: "target.bin", FileSize: 100}
		if relocateFile(dir, "absent.bin", fd, nil) {
			t.Error("relocateFile reported success for a source file that does not exist")
		}
	})

	t.Run("a length par2 disagrees with", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		body := payload(8, 4*1024)
		writeFile(t, dir, "flat.bin", body)

		// The same content, a different declared length: the shape a
		// truncated download leaves behind.
		fd := FileDesc{FileName: "target.bin", FileSize: uint64(len(body)) + 1}
		if relocateFile(dir, "flat.bin", fd, nil) {
			t.Error("relocateFile moved a file whose length disagrees with par2; par2 repair is what fixes that, " +
				"and renaming it first hides which file is short")
		}
		if _, err := os.Stat(filepath.Join(dir, "target.bin")); err == nil {
			t.Error("the file was moved despite the length mismatch")
		}
	})
}

// TestRelocateFile_RejectsTraversal is the counterpart: the guard must still
// refuse a par2 name that escapes the download directory. par2 filenames are
// poster-controlled, so this is an injection boundary rather than a formatting
// preference — Standing Design Rule 1's carve-out keeps it regardless of what
// any earlier build wrote.
func TestRelocateFile_RejectsTraversal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	body := payload(4, 4*1024)
	writeFile(t, dir, "flat.bin", body)

	fd := FileDesc{FileName: "../escaped.txt", FileSize: uint64(len(body))}
	if relocateFile(dir, "flat.bin", fd, nil) {
		t.Fatal("relocateFile accepted a par2 name escaping the download directory")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.txt")); err == nil {
		t.Error("a file was written outside the download directory")
	}
}

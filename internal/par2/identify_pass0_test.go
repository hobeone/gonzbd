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
// Both callers depend on it. postproc's quickcheck stage relocates from one
// assessment and is re-run on a retry; internal/app assesses at every file
// completion, and a job that fetches recovery volumes completes more than
// once. Without this pass the second look at an already-relocated directory
// reports the entry unaccounted, and a healthy job with any subdirectory in
// its par2 set fetches its entire recovery volume set.
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
	// Truncated, which is exactly the shape a partial download leaves, but
	// with the first 16 KB intact. Pass 0 never reads content — it compares
	// the recorded length — so the intact prefix is not for pass 0's benefit:
	// it is what stops pass 2 from claiming the file by Hash16k and reporting
	// the entry accounted for a reason this test is not about. The assertion
	// below is on Identify as a whole, so every pass has to decline.
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
// The guard two revisions back was filepath.Rel plus
// strings.HasPrefix(rel, ".."), which rejects any name merely BEGINNING with
// two dots — Rel(dir, dir/"..config.txt") is "..config.txt". os.Root, which
// replaced the lexical fsutil.PathWithin check that replaced that, refuses ".."
// as a path COMPONENT, which is the actual escape.
func TestRelocateFile_AcceptsALeadingDoubleDotName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	body := payload(3, 4*1024)
	writeFile(t, dir, "flat.bin", body)

	fd := FileDesc{FileName: "..config.txt", FileSize: uint64(len(body))}
	if !relocateIn(t, dir, "flat.bin", fd, nil) {
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
		if relocateIn(t, dir, "absent.bin", fd, nil) {
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
		if relocateIn(t, dir, "flat.bin", fd, nil) {
			t.Error("relocateFile moved a file whose length disagrees with par2; par2 repair is what fixes that, " +
				"and renaming it first hides which file is short")
		}
		if _, err := os.Stat(filepath.Join(dir, "target.bin")); err == nil {
			t.Error("the file was moved despite the length mismatch")
		}
	})
}

// TestRelocateFile_RefusesASymlinkedComponentLeavingTheRoot is the case the
// lexical check could not see, and the reason os.Root replaced it.
//
// Every component of "link/evil.txt" is an ordinary name, so a check that
// inspects the STRING accepts it. What matters is where it resolves: "link"
// points outside the job directory, so the write lands there. os.Root refuses
// it because Root methods reject a name whose components reference a location
// outside the root, which is a property of the resolved path rather than of
// the text.
func TestRelocateFile_RefusesASymlinkedComponentLeavingTheRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := t.TempDir()

	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	body := payload(5, 4*1024)
	writeFile(t, dir, "flat.bin", body)

	fd := FileDesc{FileName: "link/evil.txt", FileSize: uint64(len(body))}
	if relocateIn(t, dir, "flat.bin", fd, nil) {
		t.Error("relocateFile moved a file through a symlinked component pointing out of the job directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("a file was written outside the download directory through a symlinked component")
	}
}

// TestIdentify_DoesNotAccountAnEntryFromASymlink pins pass 0's Lstat and its
// regular-file requirement.
//
// Stat follows a symlink at the final component, so an entry would be reported
// accounted from a link pointing at some other file rather than from the
// delivered file itself. Accounted() is what the download path consults before
// deciding whether to fetch recovery volumes, so a wrong answer there is a
// missing file reported present.
//
// The assembler writes only regular files — its single content write is the
// os.OpenFile at internal/assembler/assembler.go:1630, and the package calls
// os.Symlink nowhere — but the external unpackers can extract a symlink into
// the job directory, so this is reachable rather than theoretical.
func TestIdentify_DoesNotAccountAnEntryFromASymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	body := payload(6, 40*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"Screens/shot.jpg": body})

	// The link target is RELATIVE and INSIDE the job directory. An absolute
	// target outside it would be refused by os.Root even under Stat, so the
	// test would pass without Lstat doing any of the work — it would be
	// exercising the containment guard rather than the follow-the-link
	// question this test is about.
	//
	// The decoy matches the recorded LENGTH but not the content, so pass 0's
	// size check passes on the followed link while the content passes cannot
	// claim the decoy on its own merits. Identical content would let pass 2
	// match it by Hash16k and the entry would be accounted for a reason that
	// has nothing to do with pass 0.
	writeFile(t, dir, "decoy.jpg", payload(7, len(body)))
	if err := os.MkdirAll(filepath.Join(dir, "Screens"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "decoy.jpg"), filepath.Join(dir, "Screens", "shot.jpg")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	id := identifyIn(t, dir, sets)

	if id.Accounted() {
		t.Error("an entry was reported accounted from a symlink rather than a delivered file")
	}
	if len(id.Files) != 0 {
		t.Errorf("identified %+v, want none", id.Files)
	}
}

// TestRelocateFile_RefusesToMoveASymlink pins relocateFile's source-side Lstat
// and regular-file requirement, which is the same pair pass 0 carries.
//
// Without it the two disagree about a symlink, and the disagreement is not
// symmetric: relocateFile would stat THROUGH the link, find the target's size
// matches, and move the link itself to the path par2 names — after which pass 0
// refuses it as non-regular on every later assessment. internal/app assesses at
// each file completion, so the entry reads unaccounted from then on and the job
// re-fetches its whole recovery volume set each time. That is the
// non-idempotency pass 0 was written to prevent, reached through relocation
// rather than through scanning.
//
// The link is RELATIVE and points INSIDE the root, for the reason spelled out
// on TestIdentify_DoesNotAccountAnEntryFromASymlink: an absolute target outside
// the root is refused by os.Root under Stat as well, so the test would pass
// without the Lstat doing any work.
// The two subtests isolate the two guards, which otherwise mask each other: a
// symlink's own Lstat size is the length of its target STRING, so with the
// regular-file check removed the size comparison refuses it anyway — and with a
// recorded size of 0 there is no size comparison to fall back on.
func TestRelocateFile_RefusesToMoveASymlink(t *testing.T) {
	t.Parallel()

	// setup builds a link whose target is RELATIVE and INSIDE the root, for the
	// reason spelled out on TestIdentify_DoesNotAccountAnEntryFromASymlink: an
	// absolute target outside the root is refused by os.Root under Stat as
	// well, so the test would pass without the Lstat doing any work.
	setup := func(t *testing.T) (dir string, body []byte) {
		t.Helper()
		dir = t.TempDir()
		body = payload(11, 4*1024)
		writeFile(t, dir, "decoy.bin", body)
		if err := os.Symlink("decoy.bin", filepath.Join(dir, "flat.bin")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		return dir, body
	}

	refused := func(t *testing.T, dir string, fd FileDesc) {
		t.Helper()
		if relocateIn(t, dir, "flat.bin", fd, nil) {
			t.Error("relocateFile moved a symlink to the path par2 names; pass 0 then refuses it as " +
				"non-regular, so the entry reads unaccounted on every later assessment")
		}
		if _, err := os.Lstat(filepath.Join(dir, "Screens", "shot.jpg")); err == nil {
			t.Error("the symlink was relocated into the par2 path")
		}
	}

	// Pins the Lstat. The recorded size is the TARGET's, so a Stat-based check
	// is satisfied and the move proceeds.
	t.Run("a recorded length the target satisfies", func(t *testing.T) {
		t.Parallel()
		dir, body := setup(t)
		refused(t, dir, FileDesc{FileName: "Screens/shot.jpg", FileSize: uint64(len(body))})
	})

	// Pins the regular-file requirement. par2 records no length for this entry,
	// so nothing else stands between the link and the rename.
	t.Run("no recorded length", func(t *testing.T) {
		t.Parallel()
		dir, _ := setup(t)
		refused(t, dir, FileDesc{FileName: "Screens/shot.jpg"})
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
	if relocateIn(t, dir, "flat.bin", fd, nil) {
		t.Fatal("relocateFile accepted a par2 name escaping the download directory")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.txt")); err == nil {
		t.Error("a file was written outside the download directory")
	}
}

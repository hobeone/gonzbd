package par2

import (
	"log/slog"
	"testing"
)

// TestVerifyIdentified covers the join and its four outcomes directly, without
// a filesystem.
//
// Assess's other tests go through a real directory, which is right for them —
// identification is the part that must actually look. This one is about what
// happens AFTER identification has answered, so it constructs the answer and
// checks only the classification. That separation is why the function is
// worth testing on its own: it is the piece with the branching, and its inputs
// are cheap to state exactly.
func TestVerifyIdentified(t *testing.T) {
	t.Parallel()

	entry := func(par2Path string, crc uint32) FileDesc {
		return FileDesc{FileName: par2Path, FileCRC32: crc}
	}
	log := slog.New(slog.DiscardHandler)

	t.Run("a matching CRC is checked and matched", func(t *testing.T) {
		t.Parallel()
		id := Identification{Files: []Identified{
			{OnDisk: "movie.mkv", Desc: entry("movie.mkv", 0xAABBCCDD)},
		}}
		r := verifyIdentified(id, []AssembledFile{{FileName: "movie.mkv", CRC32: 0xAABBCCDD}}, log)
		if r.Checked != 1 || r.Matched != 1 || r.Mismatched != 0 {
			t.Errorf("Checked=%d Matched=%d Mismatched=%d, want 1, 1, 0", r.Checked, r.Matched, r.Mismatched)
		}
		if len(r.Files) != 1 || r.Files[0].Par2FileName != "movie.mkv" {
			t.Errorf("Files = %+v", r.Files)
		}
	})

	t.Run("a differing CRC is corruption", func(t *testing.T) {
		t.Parallel()
		id := Identification{Files: []Identified{
			{OnDisk: "movie.mkv", Desc: entry("movie.mkv", 0xAABBCCDD)},
		}}
		r := verifyIdentified(id, []AssembledFile{{FileName: "movie.mkv", CRC32: 0x11223344}}, log)
		if r.Mismatched != 1 || r.Matched != 0 {
			t.Errorf("Mismatched=%d Matched=%d, want 1 and 0", r.Mismatched, r.Matched)
		}
		if len(r.Files) != 1 || r.Files[0].Match {
			t.Errorf("Files = %+v, want one non-matching result", r.Files)
		}
	})

	t.Run("the two unavailable-CRC cases are both NoCRC, not damage", func(t *testing.T) {
		t.Parallel()
		// One where the caller has no assembled CRC (a failed or resumed
		// download), one where par2 recorded none (a set with no IFSC data).
		// Neither says the file is wrong, so neither may be counted as such.
		id := Identification{Files: []Identified{
			{OnDisk: "no-ours.mkv", Desc: entry("no-ours.mkv", 0xAABBCCDD)},
			{OnDisk: "no-theirs.mkv", Desc: entry("no-theirs.mkv", 0)},
		}}
		r := verifyIdentified(id, []AssembledFile{
			{FileName: "no-ours.mkv", CRC32: 0},
			{FileName: "no-theirs.mkv", CRC32: 0x11223344},
		}, log)
		if r.NoCRC != 2 {
			t.Errorf("NoCRC = %d, want 2", r.NoCRC)
		}
		if r.Mismatched != 0 || r.Checked != 0 {
			t.Errorf("Mismatched=%d Checked=%d, want 0 and 0", r.Mismatched, r.Checked)
		}
		want := []string{"no-ours.mkv", "no-theirs.mkv"}
		for i, n := range want {
			if r.NoCRCFiles[i] != n {
				t.Errorf("NoCRCFiles[%d] = %q, want %q", i, r.NoCRCFiles[i], n)
			}
		}
	})

	t.Run("an unaccounted entry is unverified", func(t *testing.T) {
		t.Parallel()
		id := Identification{Unaccounted: []FileDesc{entry("missing.mkv", 0xAABBCCDD)}}
		r := verifyIdentified(id, nil, log)
		if r.Unverified != 1 {
			t.Errorf("Unverified = %d, want 1", r.Unverified)
		}
		if len(r.UnverifiedFiles) != 1 || r.UnverifiedFiles[0] != "missing.mkv" {
			t.Errorf("UnverifiedFiles = %v, want [missing.mkv]", r.UnverifiedFiles)
		}
	})

	t.Run("an identified file the caller does not list is unverified", func(t *testing.T) {
		t.Parallel()
		// par2 protects something present in the directory that this job did
		// not download — an extracted file, or another job's leftovers. There
		// is no assembled CRC that could speak to it.
		id := Identification{Files: []Identified{
			{OnDisk: "extracted.mkv", Desc: entry("extracted.mkv", 0xAABBCCDD)},
		}}
		r := verifyIdentified(id, nil, log)
		if r.Unverified != 1 || r.Checked != 0 {
			t.Errorf("Unverified=%d Checked=%d, want 1 and 0", r.Unverified, r.Checked)
		}
	})

	t.Run("a delivered file no entry claimed is NotInPar2", func(t *testing.T) {
		t.Parallel()
		id := Identification{Files: []Identified{
			{OnDisk: "movie.mkv", Desc: entry("movie.mkv", 0xAABBCCDD)},
		}}
		r := verifyIdentified(id, []AssembledFile{
			{FileName: "movie.mkv", CRC32: 0xAABBCCDD},
			{FileName: "readme.txt", CRC32: 0x99999999},
		}, log)
		if r.NotInPar2 != 1 {
			t.Errorf("NotInPar2 = %d, want 1", r.NotInPar2)
		}
		if r.Matched != 1 {
			t.Errorf("Matched = %d, want 1", r.Matched)
		}
	})

	t.Run("the join reduces both sides to a basename", func(t *testing.T) {
		t.Parallel()
		// The relocated-by-a-previous-run case: identification answers with a
		// path, the caller knows the file by its basename alone.
		id := Identification{Files: []Identified{
			{OnDisk: "Screens/shot.jpg", Desc: entry("Screens/shot.jpg", 0xAABBCCDD)},
		}}
		r := verifyIdentified(id, []AssembledFile{{FileName: "shot.jpg", CRC32: 0xAABBCCDD}}, log)
		if r.Matched != 1 {
			t.Errorf("Matched = %d, want 1: an intact file reported unverified reads as damage", r.Matched)
		}
		if r.NotInPar2 != 0 {
			t.Errorf("NotInPar2 = %d, want 0", r.NotInPar2)
		}
	})
}

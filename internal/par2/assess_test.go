package par2

import (
	"crypto/md5" //nolint:gosec // md5 used for PAR2 spec packet integrity
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// These tests supply a real directory, which the verification they replace did
// not need.
//
// That is the change, not an inconvenience of the fixtures. Verification used
// to compare two in-memory lists and match par2 entries by NAME, so it could
// answer without ever looking at the filesystem — and answering without
// looking is precisely what let it report a healthy obfuscated release as
// having matched nothing (#492). Assess identifies against the directory
// first, so a test that supplies no directory is testing nothing.

const fixtureDir = "../../test/fixtures/par2"

// assessFixture copies data.par2 and its payload into a private directory and
// assesses it. data.bin has CRC32 0x1068AFA6.
//
// It copies rather than assessing test/fixtures/par2 in place, and that is
// load-bearing rather than hygiene. That directory holds TWO sets — data.par2
// protecting "data.bin" and subdir.par2 protecting "Screens/data.bin" — so
// assessing it whole yields two manifest entries and only one payload, which
// is a legitimately unaccounted entry rather than the single-file case these
// subtests mean to describe.
//
// The verification this replaces could not see that: it indexed par2 by
// BASENAME, so both entries collapsed onto the key "data.bin" and one silently
// shadowed the other. That is the collision documented at verifyIdentified's
// join key, showing up in the fixtures themselves.
func assessFixture(t *testing.T, files []AssembledFile) Assessment {
	t.Helper()

	dir := t.TempDir()
	for _, name := range []string{"data.par2", "data.bin"} {
		b, err := os.ReadFile(filepath.Join(fixtureDir, name))
		if err != nil {
			t.Skipf("par2 fixture not available: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	sets, err := FindPar2Files(dir)
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}
	if len(sets) == 0 {
		t.Skip("no par2 sets found in fixture")
	}

	a, err := Assess(dir, sets, files, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	return a
}

func TestAssess_NoSets(t *testing.T) {
	t.Parallel()

	a, err := Assess(t.TempDir(), nil, []AssembledFile{
		{FileName: "movie.mkv", CRC32: 0x12345678},
		{FileName: "sample.avi", CRC32: 0xDEADBEEF},
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.CRC.Checked != 0 {
		t.Errorf("Checked = %d, want 0 (no par2 sets)", a.CRC.Checked)
	}
	if len(a.Renames) != 0 {
		t.Errorf("Renames = %v, want none", a.Renames)
	}
}

func TestAssess_EmptyInput(t *testing.T) {
	t.Parallel()

	a, err := Assess(t.TempDir(), nil, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.CRC.Checked != 0 || a.CRC.Matched != 0 || a.CRC.Mismatched != 0 {
		t.Errorf("unexpected result for empty input: %+v", a.CRC)
	}
}

func TestAssess_AgainstTheRealFixture(t *testing.T) {
	t.Parallel()

	t.Run("matching CRC verifies", func(t *testing.T) {
		t.Parallel()
		r := assessFixture(t, []AssembledFile{{FileName: "data.bin", CRC32: 0x1068AFA6}}).CRC
		if r.Checked != 1 || r.Matched != 1 {
			t.Errorf("Checked=%d Matched=%d, want 1 and 1", r.Checked, r.Matched)
		}
		if r.Mismatched != 0 || r.NoCRC != 0 || r.Unverified != 0 {
			t.Errorf("Mismatched=%d NoCRC=%d Unverified=%d, want all 0", r.Mismatched, r.NoCRC, r.Unverified)
		}
	})

	t.Run("mismatched CRC is corruption", func(t *testing.T) {
		t.Parallel()
		r := assessFixture(t, []AssembledFile{{FileName: "data.bin", CRC32: 0xDEADBEEF}}).CRC
		if r.Checked != 1 || r.Mismatched != 1 || r.Matched != 0 {
			t.Errorf("Checked=%d Mismatched=%d Matched=%d, want 1, 1, 0", r.Checked, r.Mismatched, r.Matched)
		}
	})

	t.Run("a par2-tracked file with no CRC is unavailable, not damaged", func(t *testing.T) {
		t.Parallel()
		r := assessFixture(t, []AssembledFile{{FileName: "data.bin", CRC32: 0}}).CRC
		if r.Checked != 0 || r.NoCRC != 1 {
			t.Errorf("Checked=%d NoCRC=%d, want 0 and 1", r.Checked, r.NoCRC)
		}
		// Identified, so it must NOT also be counted as unaccounted.
		if r.Unverified != 0 {
			t.Errorf("Unverified = %d, want 0: the entry was identified, only its CRC was missing", r.Unverified)
		}
		if len(r.NoCRCFiles) != 1 || r.NoCRCFiles[0] != "data.bin" {
			t.Errorf("NoCRCFiles = %v, want [data.bin]", r.NoCRCFiles)
		}
	})

	t.Run("a delivered file par2 does not protect is benign", func(t *testing.T) {
		t.Parallel()
		// data.bin IS on disk in the fixture, so it identifies; the caller
		// simply lists a different file as well.
		r := assessFixture(t, []AssembledFile{
			{FileName: "data.bin", CRC32: 0x1068AFA6},
			{FileName: "unknown.mkv", CRC32: 0x12345678},
		}).CRC
		if r.Matched != 1 {
			t.Errorf("Matched = %d, want 1", r.Matched)
		}
		if r.NotInPar2 != 1 {
			t.Errorf("NotInPar2 = %d, want 1 (unknown.mkv)", r.NotInPar2)
		}
		if r.Unverified != 0 {
			t.Errorf("Unverified = %d, want 0", r.Unverified)
		}
	})
}

// TestAssess_ObfuscatedFileVerifies is the case the whole design exists for.
//
// The delivered file carries a name with no relationship to the par2 entry, so
// no name-based pass can match it — which is what made a healthy release look
// like one that matched nothing, and got its recovery volumes discarded
// (#492). Identification finds it by content, and verification then compares
// the CRC the caller recorded under the OBFUSCATED name, because that is the
// name the caller knows it by. Both halves have to work for this to pass.
func TestAssess_ObfuscatedFileVerifies(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	body := payload(11, 40*1024)
	sets := par2SetForWithCRC(t, dir, map[string][]byte{"movie.mkv": body})

	const obfuscated = "7xq6N6P340dCh9Lnih5hY3jsArfSN1"
	writeFile(t, dir, obfuscated, body)

	a, err := Assess(dir, sets, []AssembledFile{
		{FileName: obfuscated, CRC32: crc32.ChecksumIEEE(body)},
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}

	if !a.ID.Accounted() {
		t.Fatalf("obfuscated file left unaccounted: %+v", a.ID.Unaccounted)
	}
	if a.CRC.Matched != 1 {
		t.Errorf("Matched = %d, want 1: the file is intact and was identified, so it must verify", a.CRC.Matched)
	}
	if a.CRC.Unverified != 0 {
		t.Errorf("Unverified = %d, want 0", a.CRC.Unverified)
	}

	// And the rename is reported but NOT performed.
	if len(a.Renames) != 1 || a.Renames[0].To != "movie.mkv" {
		t.Fatalf("Renames = %+v, want one rename to movie.mkv", a.Renames)
	}
	if _, err := os.Stat(filepath.Join(dir, "movie.mkv")); err == nil {
		t.Error("Assess moved a file; it must report renames without applying them, or the verdict it " +
			"returns would describe a directory that no longer exists")
	}
}

// TestAssess_VerifiesAFileAPreviousRunRelocated pins the basename join.
//
// A file already at its par2 path is identified as "Screens/shot.jpg" while
// the caller still knows it as "shot.jpg" — JobProgress.Filename cannot hold a
// path. Joining on the full path would find no CRC and report an intact file
// unverified, which reads as damage.
func TestAssess_VerifiesAFileAPreviousRunRelocated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	body := payload(12, 40*1024)
	sets := par2SetForWithCRC(t, dir, map[string][]byte{"Screens/shot.jpg": body})

	if err := os.MkdirAll(filepath.Join(dir, "Screens"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "Screens"), "shot.jpg", body)

	a, err := Assess(dir, sets, []AssembledFile{
		{FileName: "shot.jpg", CRC32: crc32.ChecksumIEEE(body)},
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}

	if a.CRC.Matched != 1 {
		t.Errorf("Matched = %d, want 1: the file is intact and at the path par2 names, but the caller "+
			"knows it by its basename", a.CRC.Matched)
	}
	if len(a.Renames) != 0 {
		t.Errorf("Renames = %+v, want none: the file is already where par2 wants it", a.Renames)
	}
}

// TestAssess_UnverifiedFilesAreOrdered pins that the user-facing list does not
// vary between runs. It is interpolated into the reason a job gives for
// fetching recovery volumes.
func TestAssess_UnverifiedFilesAreOrdered(t *testing.T) {
	t.Parallel()

	setID := [16]byte{9, 9, 9}
	names := []string{"zebra.txt", "apple.txt", "mango.txt"}
	pkts := make([][]byte, 0, 2*len(names))
	fileIDs := make([][16]byte, 0, len(names))
	for i, name := range names {
		fileID := [16]byte{byte(i + 1)}
		fileIDs = append(fileIDs, fileID)
		content := []byte("content for " + name)
		hash16k := md5.Sum(content) //nolint:gosec // test fixture only
		pkts = append(pkts,
			buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, hash16k, hash16k, uint64(len(content)), name)),
			buildPacket(setID, typeIFSC, buildIFSCBody(fileID, []ifscSlice{{md5Hash: hash16k, crc32: crc32.ChecksumIEEE(content)}})),
		)
	}

	parContent := buildPacket(setID, typeMain, buildMainBody(16, fileIDs...))
	for _, p := range pkts {
		parContent = append(parContent, p...)
	}

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "multi.par2"), parContent, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sets, err := FindPar2Files(tmpDir)
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}

	// No payload files on disk, so all three entries are unaccounted.
	a, err := Assess(tmpDir, sets, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.CRC.Unverified != 3 {
		t.Fatalf("Unverified = %d, want 3", a.CRC.Unverified)
	}
	want := []string{"apple.txt", "mango.txt", "zebra.txt"}
	for i, name := range want {
		if a.CRC.UnverifiedFiles[i] != name {
			t.Errorf("UnverifiedFiles[%d] = %q, want %q", i, a.CRC.UnverifiedFiles[i], name)
		}
	}
}

// TestApplyRenames_ReportsOnlyWhatItAchieved pins that the returned list is
// what happened, not what was planned.
//
// Callers track file ownership from this list (postproc's markRenamed), so a
// rename reported but not performed would move ownership to a path holding no
// file, and the real file would be left unowned — which the cleanup stages
// then skip.
func TestApplyRenames_ReportsOnlyWhatItAchieved(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	good := payload(13, 8*1024)
	bad := payload(14, 8*1024)
	sets := par2SetFor(t, dir, map[string][]byte{
		"Sub/good.bin": good,
		"Sub/bad.bin":  bad,
	})

	writeFile(t, dir, "good.bin", good)
	// Same first 16 KB as par2 recorded, so it identifies — but truncated, so
	// relocateFile's length check refuses to move it.
	writeFile(t, dir, "bad.bin", bad[:4*1024])

	a, err := Assess(dir, sets, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if len(a.Renames) == 0 {
		t.Fatal("fixture guard: no renames planned, so this proves nothing")
	}

	applied := ApplyRenames(dir, a, slog.New(slog.DiscardHandler))
	for _, r := range applied {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(r.To))); err != nil {
			t.Errorf("ApplyRenames reported %q -> %q but the destination does not exist: %v", r.From, r.To, err)
		}
	}
}

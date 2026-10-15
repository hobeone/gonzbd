package deobfuscate_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/deobfuscate"
	"github.com/hobeone/gonzbd/internal/fsutil"
)

func TestIsProbablyObfuscated(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		filename string
		want     bool
	}{
		// Certainly obfuscated
		{"32 hex digits", "b082fa0beaa644d3aa01045d5b8d0b36.mkv", true},
		{"40+ hex+dots", "0675e29e9abfd2.f7d069dab0b853283cc1b069a25f82.6547.avi", true},
		{"square-bracket + 30-hex", "[BlaBla] 5937bc5e32146ebef89a622e4a23f07b0d3757 [Brrr].mkv", true},
		{"abc.xyz prefix", "abc.xyz.a4c567edbcbf27.BLA.mkv", true},
		// Not obfuscated
		{"mixed case with separator", "Great Distro.mkv", false},
		{"three separators", "this is a download.mkv", false},
		{"letters+digits+sep", "Beast 2020.mkv", false},
		{"capital start mostly lowercase", "Catullus.mkv", false},
		{"tv show name", "The.Good.Place.S01E01.1080p.mkv", false},
		// Default obfuscated (pure lowercase, no separators, long)
		{"all lowercase no sep long", "abcdefghijk.mkv", true},
		// Path is handled correctly (only basename checked)
		{"full path not obfuscated", "/some/dir/My.Great.Show.S02E05.mkv", false},
		{"full path obfuscated", "/some/dir/b082fa0beaa644d3aa01045d5b8d0b36.mkv", true},
		// Boundaries to kill mutants
		{"boundary capital start A", "Aaaaa.mkv", false},
		{"boundary capital start Z", "Zaaaa.mkv", false},
		{"boundary capital start ratio 0.25", "AAaaaaaaaa.mkv", false},
		{"boundary short simple length 3", "cat.mkv", false},
		{"boundary short simple length 10", "catapultss.mkv", false},
		{"boundary short simple seps 1", "cat_dog.mkv", false},
		{"boundary mixed case separator", "AA_aa.mkv", false},
		{"boundary letters+digits+sep", "abcd 2020.mkv", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := deobfuscate.IsProbablyObfuscated(slog.Default(), tc.filename)
			if got != tc.want {
				t.Errorf("IsProbablyObfuscated(%q) = %v, want %v", tc.filename, got, tc.want)
			}
		})
	}

	// Test nil logger doesn't panic
	gotNil := deobfuscate.IsProbablyObfuscated(nil, "b082fa0beaa644d3aa01045d5b8d0b36.mkv")
	if !gotNil {
		t.Error("expected true for nil logger obfuscated check")
	}
}

func TestBiggestFile(t *testing.T) {
	t.Parallel()

	t.Run("empty list", func(t *testing.T) {
		t.Parallel()
		path, ok, err := deobfuscate.BiggestFile(nil)
		if err != nil || ok || path != "" {
			t.Errorf("expected empty result, got path=%q ok=%v err=%v", path, ok, err)
		}
	})

	t.Run("single file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		f := createFile(t, dir, "only.mkv", 1000)
		path, ok, err := deobfuscate.BiggestFile([]string{f})
		if err != nil {
			t.Fatal(err)
		}
		if !ok || path != f {
			t.Errorf("single file: got path=%q ok=%v, want path=%q ok=true", path, ok, f)
		}
	})

	t.Run("biggest is 3x larger", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		big := createFile(t, dir, "big.mkv", 9001)
		small := createFile(t, dir, "small.mkv", 3000)
		path, ok, err := deobfuscate.BiggestFile([]string{big, small})
		if err != nil {
			t.Fatal(err)
		}
		if !ok || path != big {
			t.Errorf("3x case: got path=%q ok=%v, want path=%q ok=true", path, ok, big)
		}

		// Exact 3x boundary
		bigExact := createFile(t, dir, "big_exact.mkv", 9000)
		path, ok, err = deobfuscate.BiggestFile([]string{bigExact, small})
		if err != nil {
			t.Fatal(err)
		}
		if !ok || path != bigExact {
			t.Errorf("exact 3x case: got path=%q ok=%v, want path=%q ok=true", path, ok, bigExact)
		}
	})

	t.Run("biggest not 3x larger", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		a := createFile(t, dir, "a.mkv", 5000)
		b := createFile(t, dir, "b.mkv", 4000)
		_, ok, err := deobfuscate.BiggestFile([]string{a, b})
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("expected ok=false when biggest is not 3x larger")
		}
	})

	t.Run("multiple files none qualify", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		a := createFile(t, dir, "a.mkv", 1000)
		b := createFile(t, dir, "b.mkv", 900)
		c := createFile(t, dir, "c.mkv", 800)
		_, ok, err := deobfuscate.BiggestFile([]string{a, b, c})
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("expected ok=false for three similar-sized files")
		}
	})
}

func TestDeobfuscate(t *testing.T) {
	t.Parallel()

	t.Run("renames obfuscated biggest file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Obfuscated name: 32 hex chars. Must be >10 MiB for heuristic deobfuscation.
		big := createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv", 11*1024*1024)
		small := createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.nfo", 100)

		renames, err := deobfuscate.Deobfuscate(context.Background(), slog.Default(), dir, "Cool.Show.S01E01", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) == 0 {
			t.Fatal("expected at least one rename")
		}

		// big should be gone, new name should exist
		if _, err := os.Stat(big); !os.IsNotExist(err) {
			t.Errorf("original big file still exists: %s", big)
		}

		expectedBig := filepath.Join(dir, "Cool.Show.S01E01.mkv")
		if _, err := os.Stat(expectedBig); err != nil {
			t.Errorf("expected renamed file not found: %s", expectedBig)
		}

		// Sibling file must also be renamed and original should be gone.
		expectedSmall := filepath.Join(dir, "Cool.Show.S01E01.nfo")
		if _, err := os.Stat(expectedSmall); err != nil {
			t.Errorf("expected renamed sibling file not found: %s", expectedSmall)
		}
		if _, err := os.Stat(small); !os.IsNotExist(err) {
			t.Errorf("original sibling file still exists: %s", small)
		}
	})

	t.Run("no rename when not obfuscated", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// >10 MiB so heuristic threshold is passed, but name is not obfuscated.
		createFile(t, dir, "Great.Show.S01E01.1080p.mkv", 11*1024*1024)
		createFile(t, dir, "other.nfo", 100)

		renames, err := deobfuscate.Deobfuscate(context.Background(), slog.Default(), dir, "SomeName", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames, got %d", len(renames))
		}
	})

	t.Run("no rename when biggest not 3x", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv", 5000)
		createFile(t, dir, "abcdefghijklmnopqrstuvwxyz012345.mkv", 4500)

		renames, err := deobfuscate.Deobfuscate(context.Background(), slog.Default(), dir, "SomeName", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames for equally-sized files, got %d", len(renames))
		}
	})

	t.Run("excluded extension not renamed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.rar", 9001)
		createFile(t, dir, "tiny.nfo", 10)

		renames, err := deobfuscate.Deobfuscate(context.Background(), slog.Default(), dir, "SomeName", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames for excluded extension, got %d", len(renames))
		}
	})

	t.Run("skip deobfuscate when biggest under 10MiB", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// 5 MiB = well under the 10 MiB threshold
		createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv", 5*1024*1024)
		createFile(t, dir, "tiny.nfo", 10)

		renames, err := deobfuscate.Deobfuscate(context.Background(), slog.Default(), dir, "SomeName", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected no renames for <10 MiB file, got %d", len(renames))
		}
		// Verify the original file still exists (not renamed).
		if _, err := os.Stat(filepath.Join(dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv")); err != nil {
			t.Errorf("original file should still exist: %v", err)
		}
	})

	t.Run("deobfuscate runs when biggest over 10MiB", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// 11 MiB = above the 10 MiB threshold
		createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv", 11*1024*1024)
		createFile(t, dir, "tiny.nfo", 10)

		renames, err := deobfuscate.Deobfuscate(context.Background(), slog.Default(), dir, "SomeName", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) == 0 {
			t.Error("expected rename for >10 MiB obfuscated file")
		}
	})

	t.Run("deobfuscate runs when biggest is exactly 10MiB", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// 10 MiB = exactly the threshold (boundary)
		createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv", 10*1024*1024)
		createFile(t, dir, "tiny.nfo", 10)

		renames, err := deobfuscate.Deobfuscate(context.Background(), slog.Default(), dir, "SomeName", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) == 0 {
			t.Error("expected rename for exactly 10 MiB obfuscated file")
		}
	})

	t.Run("nil logger doesn't panic", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		createFile(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv", 11*1024*1024)
		createFile(t, dir, "tiny.nfo", 10)

		renames, err := deobfuscate.Deobfuscate(context.Background(), nil, dir, "SomeName", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) == 0 {
			t.Error("expected rename with nil logger")
		}

		// Test subtitles with nil logger
		subRenames, err := deobfuscate.Subtitles(nil, dir)
		if err != nil {
			t.Fatal(err)
		}
		_ = subRenames
	})

	t.Run("deobfuscate with extension fix first", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		ebmlHeader := []byte{
			0x1A, 0x45, 0xDF, 0xA3, // EBML magic
			0x93,                   // size
			0x42, 0x86, 0x81, 0x01, // EBMLVersion: 1
			0x42, 0xF7, 0x81, 0x01, // EBMLReadVersion: 1
			0x42, 0xF2, 0x81, 0x04, // EBMLMaxIDLength: 4
			0x42, 0xF3, 0x81, 0x08, // EBMLMaxSizeLength: 8
			0x42, 0x82, 0x88, // DocType (length 8)
			'm', 'a', 't', 'r', 'o', 's', 'k', 'a', // "matroska"
		}
		data := make([]byte, 11*1024*1024)
		copy(data, ebmlHeader)
		path := filepath.Join(dir, "12345678901234567890.xyz")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}

		renames, err := deobfuscate.Deobfuscate(context.Background(), slog.Default(), dir, "SomeName", fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 2 {
			t.Errorf("expected 2 renames (extension fix + deobfuscate), got %d: %+v", len(renames), renames)
		}
	})
}

func TestDeobfuscate_SiblingMatchesPreFixStem(t *testing.T) {
	dir := t.TempDir()

	// "abc.xyz" is the abc.xyz-obfuscation pattern (matched regardless of
	// what follows). The original name has no popular extension, so it
	// goes through FixExtension, which content-sniffs PNG magic bytes and
	// appends ".png" -- producing "abc.xyz.somejunk.png". A naive
	// last-extension trim of the POST-fix name leaves "abc.xyz.somejunk"
	// as the sibling-matching stem, which misses true siblings that only
	// share the pre-fix stem "abc.xyz".
	pngSig := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A} //nolint:prealloc // fixed-size literal, not built via loop append
	big := append(pngSig, make([]byte, 11*1024*1024)...)             // 11 MiB: clears the 10 MiB heuristic floor and the 3x BiggestFile ratio
	bigName := "abc.xyz.somejunk"
	if err := os.WriteFile(filepath.Join(dir, bigName), big, 0o644); err != nil {
		t.Fatal(err)
	}

	siblingName := "abc.xyz.eng.srt"
	sibling := []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n")
	if err := os.WriteFile(filepath.Join(dir, siblingName), sibling, 0o644); err != nil {
		t.Fatal(err)
	}

	renames, err := deobfuscate.Deobfuscate(context.Background(), nil, dir, "Useful_Name", fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("Deobfuscate: %v", err)
	}

	var sawSibling bool
	for _, r := range renames {
		if filepath.Base(r.From) == siblingName {
			sawSibling = true
			if filepath.Base(r.To) != "Useful_Name.eng.srt" {
				t.Errorf("sibling renamed to %q, want %q", filepath.Base(r.To), "Useful_Name.eng.srt")
			}
		}
	}
	if !sawSibling {
		t.Errorf("sibling %q was not renamed (stem-matching bug); renames: %+v", siblingName, renames)
	}
}

func TestSubtitles(t *testing.T) {
	t.Parallel()

	// bigVideo is ~30 MiB; srt files are tiny — satisfies 3× dominance ratio.
	const bigSize = 30 * 1024 * 1024
	const tinySize = 100

	t.Run("numeric-suffix srt is not a sibling and must be renamed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Dominant video file.
		createFile(t, dir, "Some_Big_Movie.mkv", bigSize)
		// Genuine unrelated subtitle — different base entirely, should be renamed.
		createFile(t, dir, "14_English.srt", tinySize)
		// Unrelated subtitle that shares the video's base as a bare prefix but
		// has a digit immediately after — NOT a sibling, so it must also be
		// renamed (not skipped). The bare-prefix bug wrongly skips this one.
		createFile(t, dir, "Some_Big_Movie2.srt", tinySize)

		renames, err := deobfuscate.Subtitles(nil, dir)
		if err != nil {
			t.Fatal(err)
		}

		// Both srt files are unrelated to the video and must be renamed.
		// The buggy code skips Some_Big_Movie2.srt (bare HasPrefix match),
		// so it returns only 1 rename. The correct code returns 2.
		if len(renames) != 2 {
			t.Fatalf("expected 2 renames (both srts are unrelated), got %d: %+v", len(renames), renames)
		}

		// Build a set of (From → To) pairs for order-independent checking.
		renamed := make(map[string]string, 2)
		for _, r := range renames {
			renamed[filepath.Base(r.From)] = filepath.Base(r.To)
		}

		wantPairs := map[string]string{
			"14_English.srt":      "Some_Big_Movie.14_English.srt",
			"Some_Big_Movie2.srt": "Some_Big_Movie.Some_Big_Movie2.srt",
		}
		for from, to := range wantPairs {
			if got := renamed[from]; got != to {
				t.Errorf("rename of %q: got %q, want %q", from, got, to)
			}
		}
	})

	t.Run("exact-base match srt is skipped (already correct)", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		createFile(t, dir, "Some_Big_Movie.mkv", bigSize)
		// Exact base match (no extension difference): already a sibling.
		createFile(t, dir, "Some_Big_Movie.srt", tinySize)

		renames, err := deobfuscate.Subtitles(nil, dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected 0 renames (srt already matches base), got %d: %+v", len(renames), renames)
		}
	})

	t.Run("separator suffix srt is skipped (already a sibling)", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		createFile(t, dir, "Some_Big_Movie.mkv", bigSize)
		// "Some_Big_Movie.en.srt" — separator '.' after the base prefix.
		createFile(t, dir, "Some_Big_Movie.en.srt", tinySize)

		renames, err := deobfuscate.Subtitles(nil, dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected 0 renames (srt is already a separator-sibling), got %d: %+v", len(renames), renames)
		}
	})
}

// createFile writes size bytes of zeros to dir/name and returns the full path.
func createFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data := make([]byte, size)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("createFile %s: %v", path, err)
	}
	return path
}

func writeTestPar2File(t *testing.T, dir, fileName string, fileSize uint64, hash16k [16]byte) {
	t.Helper()
	fileNameBytes := []byte(fileName)
	if pad := (4 - len(fileNameBytes)%4) % 4; pad > 0 {
		fileNameBytes = append(fileNameBytes, make([]byte, pad)...)
	}
	bodyLen := uint64(16 + 16 + 16 + 8 + len(fileNameBytes))
	packetLen := 64 + bodyLen
	buf := make([]byte, packetLen)
	copy(buf[0:8], []byte("PAR2\x00PKT"))
	binary.LittleEndian.PutUint64(buf[8:16], packetLen)
	copy(buf[48:64], []byte{'P', 'A', 'R', ' ', '2', '.', '0', '\x00', 'F', 'i', 'l', 'e', 'D', 'e', 's', 'c'})
	copy(buf[64+16+16:64+32+16], hash16k[:])
	binary.LittleEndian.PutUint64(buf[64+48:64+56], fileSize)
	copy(buf[64+56:], fileNameBytes)
	ph := md5.New() //nolint:gosec // MD5 is PAR2 spec; not used for security
	ph.Write(buf[32:64])
	ph.Write(buf[64:])
	copy(buf[16:32], ph.Sum(nil))

	parPath := filepath.Join(dir, fileName+".par2")
	if err := os.WriteFile(parPath, buf, 0o644); err != nil {
		t.Fatalf("WriteFile PAR2: %v", err)
	}
}

func TestPar2Rename_SkipsSizeMismatchOnShared16kPrefix(t *testing.T) {
	t.Parallel()

	prefix := bytes.Repeat([]byte("A"), 16384)
	hash16k := md5.Sum(prefix) //nolint:gosec // MD5 is PAR2 spec

	const (
		oneMiB   = 1024 * 1024
		threeMiB = 3 * 1024 * 1024
	)

	t.Run("joined file par2 does not rename split 001 part", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// 1. Create a 1 MiB split .001 part sharing the 16k prefix.
		partData := append(append([]byte(nil), prefix...), make([]byte, oneMiB-len(prefix))...)
		part001 := filepath.Join(dir, "Movie.Split.bin.001")
		if err := os.WriteFile(part001, partData, 0o644); err != nil {
			t.Fatalf("WriteFile part001: %v", err)
		}

		// 2. Create an obfuscated 3 MiB file that matches the par2 size and 16k hash.
		joinedData := append(append([]byte(nil), prefix...), make([]byte, threeMiB-len(prefix))...)
		obfPath := filepath.Join(dir, "abcdef1234567890abcdef1234567890.bin")
		if err := os.WriteFile(obfPath, joinedData, 0o644); err != nil {
			t.Fatalf("WriteFile obfPath: %v", err)
		}

		// 3. Create PAR2 protecting the joined file (3 MiB).
		writeTestPar2File(t, dir, "Movie.Split.bin", threeMiB, hash16k)

		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		defer root.Close()

		renames, err := deobfuscate.Par2Rename(t.Context(), slog.Default(), root, dir, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatalf("Par2Rename: %v", err)
		}

		// Movie.Split.bin.001 must not be renamed because its size (1 MiB) != par2 size (3 MiB).
		if _, err := os.Stat(part001); err != nil {
			t.Errorf("Movie.Split.bin.001 missing or renamed: %v", err)
		}

		// The obfuscated 3 MiB file must be renamed to Movie.Split.bin.
		wantTarget := filepath.Join(dir, "Movie.Split.bin")
		if _, err := os.Stat(wantTarget); err != nil {
			t.Errorf("expected %s to exist: %v", wantTarget, err)
		}

		if len(renames) != 1 {
			t.Fatalf("len(renames) = %d, want 1; renames = %+v", len(renames), renames)
		}
		if renames[0].From != obfPath || renames[0].To != wantTarget {
			t.Errorf("renames[0] = %+v, want From=%q To=%q", renames[0], obfPath, wantTarget)
		}
	})

	t.Run("split part par2 does not rename joined file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// 1. Create a 3 MiB joined file sharing the 16k prefix.
		joinedData := append(append([]byte(nil), prefix...), make([]byte, threeMiB-len(prefix))...)
		joinedPath := filepath.Join(dir, "Movie.Split.bin")
		if err := os.WriteFile(joinedPath, joinedData, 0o644); err != nil {
			t.Fatalf("WriteFile joinedPath: %v", err)
		}

		// 2. Create an obfuscated 1 MiB file that matches the par2 size and 16k hash.
		partData := append(append([]byte(nil), prefix...), make([]byte, oneMiB-len(prefix))...)
		obfPath := filepath.Join(dir, "9876543210abcdef9876543210abcdef.bin")
		if err := os.WriteFile(obfPath, partData, 0o644); err != nil {
			t.Fatalf("WriteFile obfPath: %v", err)
		}

		// 3. Create PAR2 protecting the split part (1 MiB).
		writeTestPar2File(t, dir, "Movie.Split.bin.001", oneMiB, hash16k)

		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		defer root.Close()

		renames, err := deobfuscate.Par2Rename(t.Context(), slog.Default(), root, dir, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatalf("Par2Rename: %v", err)
		}

		// Movie.Split.bin must not be renamed because its size (3 MiB) != par2 size (1 MiB).
		if _, err := os.Stat(joinedPath); err != nil {
			t.Errorf("Movie.Split.bin missing or renamed: %v", err)
		}

		// The obfuscated 1 MiB file must be renamed to Movie.Split.bin.001.
		wantTarget := filepath.Join(dir, "Movie.Split.bin.001")
		if _, err := os.Stat(wantTarget); err != nil {
			t.Errorf("expected %s to exist: %v", wantTarget, err)
		}

		if len(renames) != 1 {
			t.Fatalf("len(renames) = %d, want 1; renames = %+v", len(renames), renames)
		}
		if renames[0].From != obfPath || renames[0].To != wantTarget {
			t.Errorf("renames[0] = %+v, want From=%q To=%q", renames[0], obfPath, wantTarget)
		}
	})
}

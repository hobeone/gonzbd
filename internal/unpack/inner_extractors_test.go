package unpack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fileJoin refuses what it cannot join and treats an existing output as
// joined. It names no engine: FileJoin does.
func TestFileJoinInner_Direct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	outDir := t.TempDir()

	if _, err := fileJoin(ctx, testLogger(), Archive{Type: RarArchive, Name: "x"}, outDir, Options{}); err == nil {
		t.Error("fileJoin(RarArchive) err = nil")
	}
	if _, err := fileJoin(ctx, testLogger(), Archive{Type: SplitArchive, Name: "x"}, outDir, Options{}); err == nil {
		t.Error("fileJoin(no parts) err = nil")
	}

	if err := os.WriteFile(filepath.Join(outDir, "joined.bin"), []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := Archive{Type: SplitArchive, Name: "joined.bin", Parts: []string{filepath.Join(outDir, "gone.001")}}
	res, err := fileJoin(ctx, testLogger(), a, outDir, Options{})
	if err != nil {
		t.Fatalf("fileJoin(existing output) err = %v", err)
	}
	if len(res.ExtractedFiles) != 1 || res.ExtractedFiles[0] != "joined.bin" || res.Engine != "" {
		t.Errorf("fileJoin(existing output) = %+v, want ExtractedFiles [joined.bin] and no Engine", res)
	}
}

// unRAR refuses a password the archive's check value proves wrong before it
// looks for the binary, and names no engine: UnRAR does.
func TestUnRARInner_PreVerifiedWrongPassword(t *testing.T) {
	t.Parallel()
	a := Archive{Type: RarArchive, Name: "password_rar5", MainFile: filepath.Join("testdata", "password_rar5.rar")}
	opts := Options{UnrarCommand: filepath.Join(t.TempDir(), "no-such-unrar")}
	res, err := unRAR(context.Background(), testLogger(), a, t.TempDir(), "wrong", opts)
	if !errors.Is(err, errPreVerifiedWrong) {
		t.Fatalf("unRAR err = %v, want errPreVerifiedWrong", err)
	}
	if res.Reason != FailWrongPassword || res.Engine != "" {
		t.Errorf("unRAR = %+v, want Reason FailWrongPassword and no Engine", res)
	}
}

// Each in-process wrapper tries the candidates in turn: a wrong password is
// FailWrongPassword from the engine itself, and the right one extracts.
func TestInProcessWithPasswords_FindsThePassword(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rar := Archive{Type: RarArchive, Name: "password_rar5", MainFile: filepath.Join("testdata", "password_rar5.rar")}
	outDir := t.TempDir()
	res, err := GoUnRARWithPasswords(ctx, testLogger(), rar, outDir, Options{Passwords: []string{"wrong", "testpass"}})
	if err != nil || res.Engine != "go_unrar" || len(res.ExtractedFiles) == 0 {
		t.Errorf("GoUnRARWithPasswords = %+v, %v; want go_unrar extracting with testpass", res, err)
	}

	// A failure that is not a wrong password ends the search: an archive
	// refused for want of space is FailDiskFull on the first candidate.
	noRoom := Options{Passwords: []string{"a", "b"}, MinFreeSpace: int64(1) << 62}
	res, err = GoUnRARWithPasswords(ctx, testLogger(), rar, t.TempDir(), noRoom)
	if !errors.Is(err, ErrInsufficientSpace) || res.Reason != FailDiskFull || res.Engine != "go_unrar" {
		t.Errorf("GoUnRARWithPasswords(no room) = %+v, %v; want go_unrar FailDiskFull", res, err)
	}
	sevenZip := Archive{Type: SevenZipArchive, Name: "flat_dir_file", MainFile: filepath.Join("testdata", "flat_dir_file.7z")}
	res, err = GoSevenZipWithPasswords(ctx, testLogger(), sevenZip, t.TempDir(), noRoom)
	if !errors.Is(err, ErrInsufficientSpace) || res.Reason != FailDiskFull || res.Engine != "go_7z" {
		t.Errorf("GoSevenZipWithPasswords(no room) = %+v, %v; want go_7z FailDiskFull", res, err)
	}
	gone := Archive{Type: SevenZipArchive, Name: "gone", MainFile: filepath.Join(t.TempDir(), "gone.7z")}
	missing := filepath.Join(t.TempDir(), "no-such-7z")
	res, err = SevenZipWithPasswords(ctx, testLogger(), gone, t.TempDir(), Options{SevenZipCommand: missing})
	if err == nil || res.Engine != "7z" {
		t.Errorf("SevenZipWithPasswords(missing binary) = %+v, %v; want a 7z failure", res, err)
	}
}

// runUnrar reports a binary that cannot start as an error of its own, with
// no exit code, and a non-zero exit as a classified failure.
func TestRunUnrar_Direct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := Archive{Type: RarArchive, Name: "x", MainFile: "x.rar"}

	res, err := runUnrar(ctx, testLogger(), a, t.TempDir(), filepath.Join(t.TempDir(), "no-such-unrar"), nil, Options{})
	if err == nil || res.ExitCode != 0 {
		t.Errorf("runUnrar(missing binary) = %+v, %v; want an error and no exit code", res, err)
	}

	bin := filepath.Join(t.TempDir(), "unrar")
	script := "#!/bin/sh\necho 'No space left on device'\nexit 9\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // executable test double under t.TempDir()
		t.Fatal(err)
	}
	res, err = runUnrar(ctx, testLogger(), a, t.TempDir(), bin, nil, Options{})
	if err == nil || res.ExitCode != 9 || res.Reason != FailDiskFull {
		t.Errorf("runUnrar(exit 9, disk full) = %+v, %v; want ExitCode 9, FailDiskFull", res, err)
	}
}

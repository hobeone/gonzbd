package unpack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// treeBytes sums the sizes of the regular files under dir.
func treeBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		n += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return n
}

// declareTarEntrySize rewrites the size field of the ustar header named name
// in the tar at path, and its checksum. tar.Writer writes a header-only
// entry (a FIFO, a link) with size 0, which a crafted archive need not.
func declareTarEntrySize(path, name string, size int64) error {
	b, err := os.ReadFile(path) //nolint:gosec // test fixture under t.TempDir()
	if err != nil {
		return err
	}
	for off := 0; off+512 <= len(b); off += 512 {
		h := b[off : off+512]
		if string(bytes.TrimRight(h[:100], "\x00")) != name {
			continue
		}
		copy(h[124:136], fmt.Sprintf("%011o\x00", size))
		copy(h[148:156], "        ")
		var sum int64
		for _, c := range h {
			sum += int64(c)
		}
		copy(h[148:156], fmt.Sprintf("%06o\x00 ", sum))
		return os.WriteFile(path, b, 0o600) //nolint:gosec // test fixture under t.TempDir()
	}
	return fmt.Errorf("no header %q in %s", name, path)
}

// What the headers declare is what the extraction writes, for each format
// declaredSize reads.
func TestDeclaredSize_MatchesWhatExtractionWrites(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tarDir := t.TempDir()
	tarPath := filepath.Join(tarDir, "set.tar")
	if err := os.WriteFile(tarPath, buildTarBytes(t, []tarEntry{
		{name: "d/", typeflag: '5'},
		{name: "d/a.txt", content: []byte("aaaa")},
		{name: "b.txt", content: []byte("bbbbbbb")},
		{name: "link", typeflag: '2', linkname: "b.txt"},
		{name: "fifo", typeflag: '6'},
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	// The FIFO's header declares a size archive/tar reads past and go_tar
	// does not write, so it is not counted.
	if err := declareTarEntrySize(tarPath, "fifo", 1<<40); err != nil {
		t.Fatal(err)
	}
	splitDir := t.TempDir()
	parts := make([]string, 0, 2)
	for i, s := range []string{"one", "three"} {
		p := filepath.Join(splitDir, "joined.bin."+partSuffix(i+1))
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p)
	}

	cases := []struct {
		name    string
		archive Archive
		extract func(a Archive, outDir string) (Result, error)
	}{
		{"rar5", Archive{Type: RarArchive, Name: "with_dirs", MainFile: filepath.Join("testdata", "with_dirs.rar")},
			func(a Archive, outDir string) (Result, error) {
				return GoUnRAR(ctx, testLogger(), a, outDir, "", Options{})
			}},
		{"rar5 multi-volume", Archive{Type: RarArchive, Name: "multi_new", MainFile: filepath.Join("testdata", "multi_new.part01.rar")},
			func(a Archive, outDir string) (Result, error) {
				return GoUnRAR(ctx, testLogger(), a, outDir, "", Options{})
			}},
		{"7z", Archive{Type: SevenZipArchive, Name: "flat_dir_file", MainFile: filepath.Join("testdata", "flat_dir_file.7z")},
			func(a Archive, outDir string) (Result, error) {
				return GoSevenZip(ctx, testLogger(), a, outDir, "", Options{})
			}},
		{"tar", Archive{Type: TarArchive, Name: "set", MainFile: tarPath, Parts: []string{tarPath}},
			func(a Archive, outDir string) (Result, error) { return GoTar(ctx, testLogger(), a, outDir, Options{}) }},
		{"split", Archive{Type: SplitArchive, Name: "joined.bin", MainFile: parts[0], Parts: parts},
			func(a Archive, outDir string) (Result, error) {
				return FileJoin(ctx, testLogger(), a, outDir, Options{})
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := declaredSize(tc.archive, "")
			if err != nil {
				t.Fatalf("declaredSize: %v", err)
			}
			outDir := t.TempDir()
			if _, err := tc.extract(tc.archive, outDir); err != nil {
				t.Fatalf("extract: %v", err)
			}
			if want := treeBytes(t, outDir); got != want || got == 0 {
				t.Errorf("declaredSize = %d, extraction wrote %d bytes", got, want)
			}
		})
	}
}

func TestDeclaredSize_Unknown(t *testing.T) {
	t.Parallel()
	enc := Archive{Type: RarArchive, Name: "encrypted_header", MainFile: filepath.Join("testdata", "encrypted_header.rar")}
	if _, err := declaredSize(enc, ""); !errors.Is(err, errSizeUnknown) {
		t.Errorf("declaredSize(encrypted RAR headers) err = %v, want errSizeUnknown", err)
	}
	if _, err := declaredSize(Archive{Type: ArchiveType(99), MainFile: "x"}, ""); !errors.Is(err, errSizeUnknown) {
		t.Errorf("declaredSize(unknown type) err = %v, want errSizeUnknown", err)
	}
}

func TestFits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		declared, free, minFree int64
		want                    bool
	}{
		{declared: 60, free: 100, minFree: 40, want: true}, // leaves exactly minFree
		{declared: 61, free: 100, minFree: 40, want: false},
		{declared: 100, free: 100, minFree: 0, want: true},
		{declared: 101, free: 100, minFree: 0, want: false},
	}
	for _, c := range cases {
		if got := fits(c.declared, c.free, c.minFree); got != c.want {
			t.Errorf("fits(%d, %d, %d) = %v, want %v", c.declared, c.free, c.minFree, got, c.want)
		}
	}
}

// Every extractor refuses, before writing anything, an archive that does not
// fit above MinFreeSpace, and reports it as FailDiskFull (#901). A
// MinFreeSpace no filesystem has free makes every archive not fit.
func TestExtractors_RefuseWhatDoesNotFit(t *testing.T) {
	t.Parallel()

	const noRoom = int64(1) << 62
	ctx := context.Background()
	rar := Archive{Type: RarArchive, Name: "single_rar5", MainFile: filepath.Join("testdata", "single_rar5.rar")}
	sevenZip := Archive{Type: SevenZipArchive, Name: "flat_dir_file", MainFile: filepath.Join("testdata", "flat_dir_file.7z")}
	tarPath := filepath.Join(t.TempDir(), "set.tar")
	if err := os.WriteFile(tarPath, buildTarBytes(t, []tarEntry{{name: "a.txt", content: []byte("a")}}), 0o600); err != nil {
		t.Fatal(err)
	}
	tarArchive := Archive{Type: TarArchive, Name: "set", MainFile: tarPath, Parts: []string{tarPath}}
	partPath := filepath.Join(t.TempDir(), "joined.bin.001")
	if err := os.WriteFile(partPath, []byte("part"), 0o600); err != nil {
		t.Fatal(err)
	}
	split := Archive{Type: SplitArchive, Name: "joined.bin", MainFile: partPath, Parts: []string{partPath}}

	// The external tools never run: the refusal comes first, so a binary
	// that does not exist is never reached.
	missing := filepath.Join(t.TempDir(), "no-such-binary")
	cases := []struct {
		name string
		run  func(outDir string, opts Options) (Result, error)
	}{
		{"go_unrar", func(outDir string, opts Options) (Result, error) {
			return GoUnRAR(ctx, testLogger(), rar, outDir, "", opts)
		}},
		{"unrar", func(outDir string, opts Options) (Result, error) {
			opts.UnrarCommand = missing
			return UnRAR(ctx, testLogger(), rar, outDir, "", opts)
		}},
		{"go_7z", func(outDir string, opts Options) (Result, error) {
			return GoSevenZip(ctx, testLogger(), sevenZip, outDir, "", opts)
		}},
		{"7z", func(outDir string, opts Options) (Result, error) {
			opts.SevenZipCommand = missing
			return SevenZip(ctx, testLogger(), sevenZip, outDir, "", opts)
		}},
		{"go_tar", func(outDir string, opts Options) (Result, error) {
			return GoTar(ctx, testLogger(), tarArchive, outDir, opts)
		}},
		{"filejoin", func(outDir string, opts Options) (Result, error) {
			return FileJoin(ctx, testLogger(), split, outDir, opts)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outDir := t.TempDir()
			var lines []string
			res, err := tc.run(outDir, Options{
				MinFreeSpace: noRoom,
				OnLine:       func(l string) { lines = append(lines, l) },
			})
			if !errors.Is(err, ErrInsufficientSpace) {
				t.Fatalf("err = %v, want ErrInsufficientSpace", err)
			}
			if res.Reason != FailDiskFull {
				t.Errorf("Reason = %v, want FailDiskFull", res.Reason)
			}
			if got := dirTree(t, outDir); len(got) != 0 {
				t.Errorf("refused extraction left %v in outDir", got)
			}
			// FileJoin reports nothing through OnLine; the archive
			// extractors show the refusal in the unpack log.
			if tc.name != "filejoin" && !slices.ContainsFunc(lines, func(l string) bool {
				return strings.HasPrefix(l, "ERROR: ") && strings.Contains(l, ErrInsufficientSpace.Error())
			}) {
				t.Errorf("OnLine lines %q carry no refusal", lines)
			}
		})
	}
}

// An archive whose size its headers do not declare is let through: the
// check refuses nothing it cannot measure.
func TestCheckFreeSpace_UnknownSizeProceeds(t *testing.T) {
	t.Parallel()
	enc := Archive{Type: RarArchive, Name: "encrypted_header", MainFile: filepath.Join("testdata", "encrypted_header.rar")}
	if err := checkFreeSpace(context.Background(), testLogger(), enc, t.TempDir(), "", int64(1)<<62); err != nil {
		t.Errorf("checkFreeSpace(unknown size) = %v, want nil", err)
	}
}

// An attempt that fails because the disk filled is FailDiskFull, whatever
// the engine classified it as; any other failure keeps its reason.
func TestExtractInStage_DiskFullIsFailDiskFull(t *testing.T) {
	t.Parallel()
	a := Archive{Type: ArchiveType(99), Name: "x", MainFile: "x"} // size unknown: no pre-check
	cases := []struct {
		err  error
		want FailReason
	}{
		{fmt.Errorf("go_tar: write f: %w", &os.PathError{Op: "write", Path: "f", Err: syscall.ENOSPC}), FailDiskFull},
		{errors.New("go_tar: something else"), FailCorrupt},
	}
	for _, c := range cases {
		res, err := extractInStage(context.Background(), testLogger(), a, t.TempDir(), "", Options{},
			func(string) (Result, error) { return Result{Reason: FailCorrupt}, c.err })
		if !errors.Is(err, c.err) {
			t.Errorf("err = %v, want %v", err, c.err)
		}
		if res.Reason != c.want {
			t.Errorf("Reason for %v = %v, want %v", c.err, res.Reason, c.want)
		}
	}
}

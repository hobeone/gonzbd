package unpack_test

import (
	"bytes"
	"cmp"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// ---- helpers -----------------------------------------------------------------

// touch creates an empty regular file at path, making parent dirs as needed.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// write creates a file at path with the given content.
func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ---- Classify tests ----------------------------------------------------------

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want unpack.ArchiveType
	}{
		{"movie.rar", unpack.RarArchive},
		{"movie.RAR", unpack.RarArchive},
		{"movie.part01.rar", unpack.RarArchive},
		{"movie.part1.rar", unpack.RarArchive},
		{"movie.r00", unpack.RarArchive},
		{"movie.r99", unpack.RarArchive},
		{"archive.7z", unpack.SevenZipArchive},
		{"archive.7Z", unpack.SevenZipArchive},
		{"archive.7z.001", unpack.SevenZipArchive},
		{"archive.7z.002", unpack.SevenZipArchive},
		{"data.001", unpack.SplitArchive},
		{"data.002", unpack.SplitArchive},
		// P21: .ts.NNN files are generic splits — joined by FileJoin
		{"show.ts.001", unpack.SplitArchive},
		{"show.ts.002", unpack.SplitArchive},
		{"backup.tar", unpack.TarArchive},
		{"backup.TAR", unpack.TarArchive},
		// Compressed tar variants are out of scope (upstream TAR_RE is
		// `\.(tar$)`, plain tar only) — they must NOT be classified as tar.
		{"backup.tar.gz", unpack.UnknownArchive},
		{"backup.tgz", unpack.UnknownArchive},
		{"readme.txt", unpack.UnknownArchive},
		{"movie.nfo", unpack.UnknownArchive},
		{"noext", unpack.UnknownArchive},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			got := unpack.Classify(tc.path)
			if got != tc.want {
				t.Errorf("Classify(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// ---- Scan tests --------------------------------------------------------------

func TestScan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		layout  []string
		wantLen int
		// wantNames is a sorted list of archive set names expected.
		wantNames []string
		// wantTypes maps set name → expected ArchiveType.
		wantTypes map[string]unpack.ArchiveType
	}{
		{
			name: "new-style multipart RAR",
			layout: []string{
				"movie.part01.rar",
				"movie.part02.rar",
				"movie.part03.rar",
				"readme.nfo",
			},
			wantLen:   1,
			wantNames: []string{"movie"},
			wantTypes: map[string]unpack.ArchiveType{"movie": unpack.RarArchive},
		},
		{
			name: "legacy RAR set",
			layout: []string{
				"show.rar",
				"show.r00",
				"show.r01",
			},
			wantLen:   1,
			wantNames: []string{"show"},
			wantTypes: map[string]unpack.ArchiveType{"show": unpack.RarArchive},
		},
		{
			name: "single 7z archive",
			layout: []string{
				"backup.7z",
			},
			wantLen:   1,
			wantNames: []string{"backup"},
			wantTypes: map[string]unpack.ArchiveType{"backup": unpack.SevenZipArchive},
		},
		{
			name: "split 7z volumes",
			layout: []string{
				"big.7z.001",
				"big.7z.002",
				"big.7z.003",
			},
			wantLen:   1,
			wantNames: []string{"big"},
			wantTypes: map[string]unpack.ArchiveType{"big": unpack.SevenZipArchive},
		},
		{
			name: "generic split files",
			layout: []string{
				"data.001",
				"data.002",
				"data.003",
			},
			wantLen:   1,
			wantNames: []string{"data"},
			wantTypes: map[string]unpack.ArchiveType{"data": unpack.SplitArchive},
		},
		{
			name: "mixed archive types",
			layout: []string{
				"alpha.part01.rar",
				"alpha.part02.rar",
				"beta.7z",
				"gamma.001",
				"gamma.002",
				"ignore.txt",
			},
			wantLen:   3,
			wantNames: []string{"alpha", "beta", "gamma"},
			wantTypes: map[string]unpack.ArchiveType{
				"alpha": unpack.RarArchive,
				"beta":  unpack.SevenZipArchive,
				"gamma": unpack.SplitArchive,
			},
		},
		{
			name:    "empty directory",
			layout:  []string{},
			wantLen: 0,
		},
		{
			name: "non-archive files only",
			layout: []string{
				"readme.txt",
				"image.jpg",
			},
			wantLen: 0,
		},
		{
			// P21: .ts.NNN files are generic splits with name "show.ts"
			name: "ts split files",
			layout: []string{
				"show.ts.001",
				"show.ts.002",
				"show.ts.003",
			},
			wantLen:   1,
			wantNames: []string{"show.ts"},
			wantTypes: map[string]unpack.ArchiveType{"show.ts": unpack.SplitArchive},
		},
		{
			name: "single tar archive",
			layout: []string{
				"backup.tar",
			},
			wantLen:   1,
			wantNames: []string{"backup"},
			wantTypes: map[string]unpack.ArchiveType{"backup": unpack.TarArchive},
		},
		{
			// Compressed tar variants are out of scope for TarArchive
			// detection and remain unrecognised (no other archive type
			// matches them either).
			name: "compressed tar variants are not classified as tar",
			layout: []string{
				"backup.tar.gz",
				"backup.tgz",
			},
			wantLen: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for _, f := range tc.layout {
				touch(t, filepath.Join(dir, f))
			}

			archives, err := unpack.Scan(dir)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}

			if len(archives) != tc.wantLen {
				t.Fatalf("Scan returned %d archives, want %d: %+v", len(archives), tc.wantLen, archives)
			}

			slices.SortFunc(archives, func(a, b unpack.Archive) int { return cmp.Compare(a.Name, b.Name) })

			for i, name := range tc.wantNames {
				if archives[i].Name != name {
					t.Errorf("archives[%d].Name = %q, want %q", i, archives[i].Name, name)
				}
			}
			for _, a := range archives {
				wantType, ok := tc.wantTypes[a.Name]
				if !ok {
					continue
				}
				if a.Type != wantType {
					t.Errorf("archive %q: Type = %v, want %v", a.Name, a.Type, wantType)
				}
			}
		})
	}
}

// TestScanNewStyleRARMainFile verifies that the first part (part01) is selected
// as the MainFile when scanning a new-style multi-part RAR set.
func TestScanNewStyleRARMainFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, f := range []string{"movie.part01.rar", "movie.part02.rar", "movie.part03.rar"} {
		touch(t, filepath.Join(dir, f))
	}

	archives, err := unpack.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(archives) != 1 {
		t.Fatalf("want 1 archive, got %d", len(archives))
	}
	a := archives[0]
	if got := filepath.Base(a.MainFile); got != "movie.part01.rar" {
		t.Errorf("MainFile = %q, want movie.part01.rar", got)
	}
	if len(a.Parts) != 3 {
		t.Errorf("len(Parts) = %d, want 3", len(a.Parts))
	}
}

// ---- FileJoin tests ----------------------------------------------------------

// P21: FileJoin with .ts.NNN files produces show.ts as output.
func TestFileJoin_TSFiles(t *testing.T) {
	t.Parallel()

	part1 := []byte("TS segment 1 ")
	part2 := []byte("TS segment 2 ")
	part3 := []byte("TS segment 3")
	want := append(append(part1, part2...), part3...)

	dir := t.TempDir()
	outDir := t.TempDir()

	write(t, filepath.Join(dir, "show.ts.001"), part1)
	write(t, filepath.Join(dir, "show.ts.002"), part2)
	write(t, filepath.Join(dir, "show.ts.003"), part3)

	archive := unpack.Archive{
		Type:     unpack.SplitArchive,
		Name:     "show.ts",
		MainFile: filepath.Join(dir, "show.ts.001"),
		Parts: []string{
			filepath.Join(dir, "show.ts.001"),
			filepath.Join(dir, "show.ts.002"),
			filepath.Join(dir, "show.ts.003"),
		},
	}

	res, err := unpack.FileJoin(t.Context(), slog.Default(), archive, outDir, unpack.Options{})
	if err != nil {
		t.Fatalf("FileJoin: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("Result.Err: %v", res.Err)
	}

	outPath := filepath.Join(outDir, "show.ts")
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("joined output = %q, want %q", got, want)
	}
}

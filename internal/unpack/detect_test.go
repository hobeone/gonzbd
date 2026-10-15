package unpack

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

func TestGroupArchives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files []string
		want  []Archive
	}{
		{
			name:  "empty list",
			files: nil,
			want:  nil,
		},
		{
			name: "single zip file (not classified as RAR/7z/Split)",
			files: []string{
				"/dir/somefile.zip",
			},
			want: nil,
		},
		{
			name: "new-style multi-part rar",
			files: []string{
				"/dir/movie.part02.rar",
				"/dir/movie.part01.rar",
				"/dir/movie.part03.rar",
			},
			want: []Archive{
				{
					Type:     RarArchive,
					Name:     "movie",
					MainFile: "/dir/movie.part01.rar",
					Parts: []string{
						"/dir/movie.part01.rar",
						"/dir/movie.part02.rar",
						"/dir/movie.part03.rar",
					},
				},
			},
		},
		{
			name: "legacy rar volumes",
			files: []string{
				"/dir/show.r01",
				"/dir/show.rar",
				"/dir/show.r00",
			},
			want: []Archive{
				{
					Type:     RarArchive,
					Name:     "show",
					MainFile: "/dir/show.rar",
					Parts: []string{
						"/dir/show.r00",
						"/dir/show.r01",
						"/dir/show.rar",
					},
				},
			},
		},
		{
			name: "legacy rar volumes without .rar file",
			files: []string{
				"/dir/show.r01",
				"/dir/show.r00",
			},
			want: []Archive{
				{
					Type:     RarArchive,
					Name:     "show",
					MainFile: "/dir/show.r00",
					Parts: []string{
						"/dir/show.r00",
						"/dir/show.r01",
					},
				},
			},
		},
		{
			name: "split 7z volumes",
			files: []string{
				"/dir/data.7z.002",
				"/dir/data.7z.001",
			},
			want: []Archive{
				{
					Type:     SevenZipArchive,
					Name:     "data",
					MainFile: "/dir/data.7z.001",
					Parts: []string{
						"/dir/data.7z.001",
						"/dir/data.7z.002",
					},
				},
			},
		},
		{
			name: "single 7z archive",
			files: []string{
				"/dir/single.7z",
			},
			want: []Archive{
				{
					Type:     SevenZipArchive,
					Name:     "single",
					MainFile: "/dir/single.7z",
					Parts:    []string{"/dir/single.7z"},
				},
			},
		},
		{
			name: "single 7z overridden by split 7z",
			files: []string{
				"/dir/data.7z",
				"/dir/data.7z.001",
				"/dir/data.7z.002",
			},
			want: []Archive{
				{
					Type:     SevenZipArchive,
					Name:     "data",
					MainFile: "/dir/data.7z.001",
					Parts: []string{
						"/dir/data.7z.001",
						"/dir/data.7z.002",
					},
				},
			},
		},
		{
			name: "generic split files",
			files: []string{
				"/dir/split.002",
				"/dir/split.001",
				"/dir/split.003",
			},
			want: []Archive{
				{
					Type:     SplitArchive,
					Name:     "split",
					MainFile: "/dir/split.001",
					Parts: []string{
						"/dir/split.001",
						"/dir/split.002",
						"/dir/split.003",
					},
				},
			},
		},
		{
			name: "generic split files non-contiguous fallback",
			files: []string{
				"/dir/split.001",
				"/dir/split.003",
			},
			want: []Archive{
				{
					Type:     SplitArchive,
					Name:     "split",
					MainFile: "/dir/split.001",
					Parts: []string{
						"/dir/split.001",
						"/dir/split.003",
					},
				},
			},
		},
		{
			name: "single tar archive",
			files: []string{
				"/dir/backup.tar",
			},
			want: []Archive{
				{
					Type:     TarArchive,
					Name:     "backup",
					MainFile: "/dir/backup.tar",
					Parts:    []string{"/dir/backup.tar"},
				},
			},
		},
		{
			name: "generic split files overridden by rar",
			files: []string{
				"/dir/movie.part02.rar",
				"/dir/movie.part01.rar",
				"/dir/movie.001",
				"/dir/movie.002",
			},
			want: []Archive{
				{
					Type:     RarArchive,
					Name:     "movie",
					MainFile: "/dir/movie.part01.rar",
					Parts: []string{
						"/dir/movie.part01.rar",
						"/dir/movie.part02.rar",
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := groupArchives(tc.files)
			if len(got) != len(tc.want) {
				t.Fatalf("len(got) = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i].Type != tc.want[i].Type {
					t.Errorf("[%d] Type = %d, want %d", i, got[i].Type, tc.want[i].Type)
				}
				if got[i].Name != tc.want[i].Name {
					t.Errorf("[%d] Name = %q, want %q", i, got[i].Name, tc.want[i].Name)
				}
				if got[i].MainFile != tc.want[i].MainFile {
					t.Errorf("[%d] MainFile = %q, want %q", i, got[i].MainFile, tc.want[i].MainFile)
				}
				if !slices.Equal(got[i].Parts, tc.want[i].Parts) {
					t.Errorf("[%d] Parts = %v, want %v", i, got[i].Parts, tc.want[i].Parts)
				}
			}
		})
	}
}

func TestEnsureRarSet(t *testing.T) {
	t.Parallel()

	m := make(map[string]*rarSet)

	s1 := ensureRarSet(m, "key1")
	if s1 == nil {
		t.Fatal("expected s1 to be non-nil")
	}
	if m["key1"] != s1 {
		t.Error("expected s1 to be stored in map")
	}

	s2 := ensureRarSet(m, "key1")
	if s2 != s1 {
		t.Error("expected retrieve existing s1 on duplicate call")
	}
}

func TestSortedNumericParts(t *testing.T) {
	t.Run("valid contiguous sequence", func(t *testing.T) {
		input := []string{"file.003", "file.001", "file.002"}
		got, err := sortedNumericParts(input)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		want := []string{"file.001", "file.002", "file.003"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("missing part", func(t *testing.T) {
		input := []string{"file.001", "file.003"}
		_, err := sortedNumericParts(input)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		expected := "missing part 2 in split sequence"
		if err.Error() != expected {
			t.Errorf("got error %q, want %q", err.Error(), expected)
		}
	})

	t.Run("invalid suffix", func(t *testing.T) {
		input := []string{"file.abc"}
		_, err := sortedNumericParts(input)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

// TestGroupArchives_Single7zNotShadowedByPar2Backup pins that numbered backup
// files created by external par2 repair (e.g. "archive.7z.1", "archive.7z.2")
// are not classified as split 7z volumes (.7z.001) and do not shadow the
// repaired single "archive.7z" in groupArchives or Classify.
func TestGroupArchives_Single7zNotShadowedByPar2Backup(t *testing.T) {
	t.Parallel()

	if got := Classify("/dir/archive.7z.1"); got != UnknownArchive {
		t.Errorf("Classify(archive.7z.1) = %v, want UnknownArchive", got)
	}
	if got := Classify("/dir/archive.7z.12"); got != UnknownArchive {
		t.Errorf("Classify(archive.7z.12) = %v, want UnknownArchive", got)
	}
	if got := Classify("/dir/archive.7z.001"); got != SevenZipArchive {
		t.Errorf("Classify(archive.7z.001) = %v, want SevenZipArchive", got)
	}

	got := groupArchives([]string{
		"/dir/archive.7z",
		"/dir/archive.7z.1",
		"/dir/archive.7z.2",
	})
	want := []Archive{
		{
			Type:     SevenZipArchive,
			Name:     "archive",
			MainFile: "/dir/archive.7z",
			Parts:    []string{"/dir/archive.7z"},
		},
	}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d (%+v), want %d", len(got), got, len(want))
	}
	if got[0].MainFile != want[0].MainFile || !slices.Equal(got[0].Parts, want[0].Parts) {
		t.Errorf("got[0] = %+v, want %+v", got[0], want[0])
	}
}

func TestGroupArchives_PreservesSplitArchiveCase(t *testing.T) {
	t.Parallel()

	files := []string{
		"/dir/Movie.Split.bin.002",
		"/dir/Movie.Split.bin.001",
	}

	got := groupArchives(files)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Type != SplitArchive {
		t.Errorf("Type = %v, want SplitArchive", got[0].Type)
	}
	if got[0].Name != "Movie.Split.bin" {
		t.Errorf("Name = %q, want %q", got[0].Name, "Movie.Split.bin")
	}
	if got[0].MainFile != "/dir/Movie.Split.bin.001" {
		t.Errorf("MainFile = %q, want %q", got[0].MainFile, "/dir/Movie.Split.bin.001")
	}
	wantParts := []string{"/dir/Movie.Split.bin.001", "/dir/Movie.Split.bin.002"}
	if !slices.Equal(got[0].Parts, wantParts) {
		t.Errorf("Parts = %v, want %v", got[0].Parts, wantParts)
	}
}

func TestGroupArchives_DistinctSubdirectoriesDoNotCollide(t *testing.T) {
	t.Parallel()

	t.Run("all archive families in distinct subdirectories", func(t *testing.T) {
		t.Parallel()
		files := []string{
			// new-style multi-part rar
			"/dir/sub1/movie.part01.rar",
			"/dir/sub1/movie.part02.rar",
			"/dir/sub2/movie.part01.rar",
			"/dir/sub2/movie.part02.rar",

			// legacy rar
			"/dir/sub1/show.rar",
			"/dir/sub1/show.r00",
			"/dir/sub2/show.rar",
			"/dir/sub2/show.r00",

			// seven split
			"/dir/sub1/data.7z.001",
			"/dir/sub1/data.7z.002",
			"/dir/sub2/data.7z.001",
			"/dir/sub2/data.7z.002",

			// seven single
			"/dir/sub1/single.7z",
			"/dir/sub2/single.7z",

			// split parts
			"/dir/sub1/split.001",
			"/dir/sub1/split.002",
			"/dir/sub2/split.001",
			"/dir/sub2/split.002",

			// tar single
			"/dir/sub1/backup.tar",
			"/dir/sub2/backup.tar",
		}

		got := groupArchives(files)
		// Each of the 6 types should yield 2 archives (one in sub1, one in sub2) = 12 archives total.
		if len(got) != 12 {
			t.Fatalf("len(got) = %d, want 12; got = %+v", len(got), got)
		}

		// Verify every archive's Parts are within the same directory as its MainFile.
		for _, a := range got {
			dir := filepath.Dir(a.MainFile)
			for _, p := range a.Parts {
				if filepath.Dir(p) != dir {
					t.Errorf("archive %s (%s) has part from different dir %s (want %s)", a.Name, a.MainFile, p, dir)
				}
			}
		}
	})

	t.Run("deterministic sorting across 16 subdirectories with same name", func(t *testing.T) {
		t.Parallel()
		// Go's pdqsort switches from insertion sort to quicksort partitions when
		// n > 12. Use 16 subdirectories in reverse order to ensure pdqsort partitions
		// and exercises the MainFile tie-breaker in slices.SortFunc.
		var files []string
		for i := 16; i >= 1; i-- {
			files = append(files, fmt.Sprintf("/dir/sub%02d/release.part01.rar", i))
		}

		got := groupArchives(files)
		if len(got) != 16 {
			t.Fatalf("len(got) = %d, want 16", len(got))
		}

		for i := range 16 {
			wantMain := fmt.Sprintf("/dir/sub%02d/release.part01.rar", i+1)
			if got[i].MainFile != wantMain {
				t.Errorf("[%d] MainFile = %q, want %q", i, got[i].MainFile, wantMain)
			}
		}
	})
}

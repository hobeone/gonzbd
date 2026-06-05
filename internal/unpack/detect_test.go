package unpack

import (
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

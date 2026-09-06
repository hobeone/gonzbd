package job

import (
	"slices"
	"testing"
)

func TestSortJobFiles(t *testing.T) {
	t.Parallel()
	files := []JobFile{
		{Subject: "movie.nfo", Bytes: 100},
		{Subject: "movie.part03.rar", Bytes: 100},
		{Subject: "movie.part01.rar", Bytes: 100},
		{Subject: "movie.part02.rar", Bytes: 100},
	}
	SortJobFiles(files)

	got := make([]string, len(files))
	for i := range got {
		got[i] = files[i].Subject
	}
	want := []string{
		"movie.part01.rar",
		"movie.part02.rar",
		"movie.part03.rar",
		"movie.nfo",
	}
	if !slices.Equal(got, want) {
		t.Errorf("file order:\n  got  %v\n  want %v", got, want)
	}
}

func TestSortJobFiles_LegacyRar(t *testing.T) {
	t.Parallel()
	files := []JobFile{
		{Subject: "show.sfv", Bytes: 100},
		{Subject: "show.r01", Bytes: 100},
		{Subject: "show.rar", Bytes: 100},
		{Subject: "show.r00", Bytes: 100},
	}
	SortJobFiles(files)

	got := make([]string, len(files))
	for i := range got {
		got[i] = files[i].Subject
	}
	want := []string{
		"show.rar",
		"show.r00",
		"show.r01",
		"show.sfv",
	}
	if !slices.Equal(got, want) {
		t.Errorf("legacy rar order:\n  got  %v\n  want %v", got, want)
	}
}

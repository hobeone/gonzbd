package queue

import (
	"slices"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// art returns a minimal nzb.Article for use in sort tests.
func art(id string) nzb.Article {
	return nzb.Article{ID: id, Bytes: 100, Number: 1}
}

// TestNewJob_SortsNewStyleRarVolumesFirst verifies that new-style RAR volumes
// (movie.part01.rar, movie.part02.rar, …) sort before non-RAR files and are
// ordered by volume number, regardless of their position in the NZB.
func TestNewJob_SortsNewStyleRarVolumesFirst(t *testing.T) {
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "movie.nfo", Bytes: 100, Articles: []nzb.Article{art("nfo@x")}},
			{Subject: "movie.part03.rar", Bytes: 100, Articles: []nzb.Article{art("r3@x")}},
			{Subject: "movie.part01.rar", Bytes: 100, Articles: []nzb.Article{art("r1@x")}},
			{Subject: "movie.part02.rar", Bytes: 100, Articles: []nzb.Article{art("r2@x")}},
		},
	}
	job, err := NewJob(parsed, AddOptions{Filename: "movie.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, mustManifest(t, job).NumFiles())
	for i := range got {
		got[i] = mustManifest(t, job).FileSubject(i)
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

// TestNewJob_SortsLegacyRarVolumesFirst verifies that legacy-style RAR volumes
// (movie.rar = vol1, movie.r00 = vol2, movie.r01 = vol3) are reordered correctly.
func TestNewJob_SortsLegacyRarVolumesFirst(t *testing.T) {
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "show.sfv", Bytes: 100, Articles: []nzb.Article{art("sfv@x")}},
			{Subject: "show.r01", Bytes: 100, Articles: []nzb.Article{art("r01@x")}},
			{Subject: "show.rar", Bytes: 100, Articles: []nzb.Article{art("rar@x")}},
			{Subject: "show.r00", Bytes: 100, Articles: []nzb.Article{art("r00@x")}},
		},
	}
	job, err := NewJob(parsed, AddOptions{Filename: "show.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, mustManifest(t, job).NumFiles())
	for i := range got {
		got[i] = mustManifest(t, job).FileSubject(i)
	}
	want := []string{
		"show.rar", // vol 1
		"show.r00", // vol 2
		"show.r01", // vol 3
		"show.sfv",
	}
	if !slices.Equal(got, want) {
		t.Errorf("file order:\n  got  %v\n  want %v", got, want)
	}
}

// TestNewJob_SortsMultipleRarSetsInOrder verifies that when a job contains
// two independent RAR sets, both sets are sorted by volume number and the
// sets are stable relative to each other.
func TestNewJob_SortsMultipleRarSetsInOrder(t *testing.T) {
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "extras.part02.rar", Bytes: 100, Articles: []nzb.Article{art("e2@x")}},
			{Subject: "movie.part02.rar", Bytes: 100, Articles: []nzb.Article{art("m2@x")}},
			{Subject: "movie.part01.rar", Bytes: 100, Articles: []nzb.Article{art("m1@x")}},
			{Subject: "extras.part01.rar", Bytes: 100, Articles: []nzb.Article{art("e1@x")}},
			{Subject: "movie.nfo", Bytes: 100, Articles: []nzb.Article{art("nfo@x")}},
		},
	}
	job, err := NewJob(parsed, AddOptions{Filename: "bundle.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, mustManifest(t, job).NumFiles())
	for i := range got {
		got[i] = mustManifest(t, job).FileSubject(i)
	}
	// Both sets sorted by vol; sets ordered by setname ("extras" < "movie" alphabetically).
	want := []string{
		"extras.part01.rar",
		"extras.part02.rar",
		"movie.part01.rar",
		"movie.part02.rar",
		"movie.nfo",
	}
	if !slices.Equal(got, want) {
		t.Errorf("file order:\n  got  %v\n  want %v", got, want)
	}
}

// TestNewJob_NonRarNzbUnchanged verifies that an NZB with no RAR files is
// left in document order (sort is a stable no-op).
func TestNewJob_NonRarNzbUnchanged(t *testing.T) {
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "readme.nfo", Bytes: 100, Articles: []nzb.Article{art("nfo@x")}},
			{Subject: "checksum.sfv", Bytes: 100, Articles: []nzb.Article{art("sfv@x")}},
		},
	}
	job, err := NewJob(parsed, AddOptions{Filename: "docs.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, mustManifest(t, job).NumFiles())
	for i := range got {
		got[i] = mustManifest(t, job).FileSubject(i)
	}
	want := []string{"readme.nfo", "checksum.sfv"}
	if !slices.Equal(got, want) {
		t.Errorf("file order:\n  got  %v\n  want %v", got, want)
	}
}

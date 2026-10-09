package job

import (
	"errors"
	"slices"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

func verifiedTestJob(t *testing.T) *Job {
	t.Helper()
	j := New("verified", "verified", Policy{})
	m := newManifest([]JobFile{
		{Subject: "a.bin", Bytes: 400, Articles: []JobArticle{
			{ID: "<a0@x>", Bytes: 100}, {ID: "<a1@x>", Bytes: 100},
			{ID: "<a2@x>", Bytes: 100}, {ID: "<a3@x>", Bytes: 100},
		}},
		{Subject: "b.bin", Bytes: 100, Articles: []JobArticle{{ID: "<b0@x>", Bytes: 100}}},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	return j
}

// TestInstallVerified_MarksDoneAndKeepsRowsInOffsetOrder pins the door's two
// effects: each row's article leaves the unfinished count, and the rows are
// kept for the whole-file CRC in offset order whatever order they arrived in.
func TestInstallVerified_MarksDoneAndKeepsRowsInOffsetOrder(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	before, err := j.CountUnfinishedArticles(0)
	if err != nil {
		t.Fatalf("CountUnfinishedArticles: %v", err)
	}

	rows := []durability.WrittenRow{
		{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100, CRC32: 0x2},
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1},
	}
	if err := j.InstallVerified(0, rows); err != nil {
		t.Fatalf("InstallVerified: %v", err)
	}

	after, err := j.CountUnfinishedArticles(0)
	if err != nil {
		t.Fatalf("CountUnfinishedArticles: %v", err)
	}
	if before-after != 2 {
		t.Errorf("unfinished articles went %d -> %d, want a drop of 2", before, after)
	}
	want := []durability.WrittenRow{rows[1], rows[0]}
	if got := j.FileRows(0); !slices.Equal(got, want) {
		t.Errorf("FileRows(0) = %+v, want %+v", got, want)
	}
	if got := j.FileRows(1); len(got) != 0 {
		t.Errorf("FileRows(1) = %+v, want none: rows are per file", got)
	}
	if p := j.Progress(); p.PendingArticles() != 3 {
		t.Errorf("PendingArticles = %d, want 3: the counters must follow the bits", p.PendingArticles())
	}
}

// TestInstallVerified_ReplacesAnArticlesEarlierRow pins that a second install
// of the same article replaces its row rather than adding one, so the CRC
// chain cannot see a duplicate.
func TestInstallVerified_ReplacesAnArticlesEarlierRow(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	first := durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0xA}
	second := durability.WrittenRow{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0xB}
	for _, r := range []durability.WrittenRow{first, second} {
		if err := j.InstallVerified(0, []durability.WrittenRow{r}); err != nil {
			t.Fatalf("InstallVerified: %v", err)
		}
	}
	if got := j.FileRows(0); !slices.Equal(got, []durability.WrittenRow{second}) {
		t.Errorf("FileRows(0) = %+v, want only the later row", got)
	}
}

// TestInstallVerified_RefusesARowItCannotPlace pins that a row naming another
// file, or an article outside the file, is refused and installs nothing — not
// even the valid rows beside it.
func TestInstallVerified_RefusesARowItCannotPlace(t *testing.T) {
	t.Parallel()
	good := durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100}
	for name, bad := range map[string]durability.WrittenRow{
		"another file's row":          {FileIdx: 1, ArtIdx: 4, Offset: 0, Length: 100},
		"an article outside the file": {FileIdx: 0, ArtIdx: 4, Offset: 400, Length: 100},
		"a negative article":          {FileIdx: 0, ArtIdx: -1, Offset: 0, Length: 100},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			j := verifiedTestJob(t)
			if err := j.InstallVerified(0, []durability.WrittenRow{good, bad}); err == nil {
				t.Fatal("InstallVerified accepted a row it cannot place")
			}
			if n, _ := j.CountUnfinishedArticles(0); n != 4 {
				t.Errorf("unfinished = %d after a refused install, want 4", n)
			}
			if got := j.FileRows(0); len(got) != 0 {
				t.Errorf("FileRows(0) = %+v after a refused install, want none", got)
			}
		})
	}
	j := verifiedTestJob(t)
	if err := j.InstallVerified(2, nil); err == nil {
		t.Error("InstallVerified accepted an out-of-range file index")
	}
}

// TestInstallVerified_NeedsTheManifest pins the residency gate: installing
// maintains counters, which need the manifest.
func TestInstallVerified_NeedsTheManifest(t *testing.T) {
	t.Parallel()
	j := New("cold", "cold", Policy{})
	err := j.InstallVerified(0, []durability.WrittenRow{{FileIdx: 0}})
	if !errors.Is(err, ErrNotResident) {
		t.Errorf("InstallVerified on a non-resident job = %v, want ErrNotResident", err)
	}
	if got := j.FileRows(0); got != nil {
		t.Errorf("FileRows on a job with no progress = %+v, want nil", got)
	}
}

// TestFileRows_ReturnsACopy pins that a caller cannot edit the resident rows
// through the returned slice.
func TestFileRows_ReturnsACopy(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	if err := j.InstallVerified(0, []durability.WrittenRow{{FileIdx: 0, ArtIdx: 0, Length: 100, CRC32: 7}}); err != nil {
		t.Fatalf("InstallVerified: %v", err)
	}
	got := j.FileRows(0)
	got[0].CRC32 = 99
	if again := j.FileRows(0); again[0].CRC32 != 7 {
		t.Errorf("resident row CRC = %d after editing the returned copy, want 7", again[0].CRC32)
	}
	clone := j.Progress()
	clone.written[0][0].CRC32 = 99
	if again := j.FileRows(0); again[0].CRC32 != 7 {
		t.Errorf("resident row CRC = %d after editing a Progress() clone's row, want 7: the clone shares the slice", again[0].CRC32)
	}
	if err := j.InstallVerified(0, []durability.WrittenRow{{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100}}); err != nil {
		t.Fatalf("InstallVerified: %v", err)
	}
	if n := len(clone.written[0]); n != 1 {
		t.Errorf("a Progress() clone saw %d rows after a later install, want 1: the clone shares the map", n)
	}
}

// TestInstallVerified_NeitherKeepsNorReordersTheCallersSlice pins the first
// install into a file: the resident rows are a sorted copy, so the caller can
// reuse or edit its slice without reaching the job.
func TestInstallVerified_NeitherKeepsNorReordersTheCallersSlice(t *testing.T) {
	t.Parallel()
	j := verifiedTestJob(t)
	rows := []durability.WrittenRow{
		{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100, CRC32: 0x2},
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1},
	}
	if err := j.InstallVerified(0, rows); err != nil {
		t.Fatalf("InstallVerified: %v", err)
	}
	if rows[0].ArtIdx != 2 || rows[1].ArtIdx != 0 {
		t.Errorf("the caller's slice was reordered: %+v", rows)
	}
	rows[1].CRC32 = 99
	if got := j.FileRows(0); got[0].CRC32 != 0x1 {
		t.Errorf("resident row CRC = %d after editing the caller's slice, want 1: the job kept the caller's slice", got[0].CRC32)
	}
}

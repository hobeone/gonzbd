package directunpack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The four helpers below were untested when this file was added. They are
// small, but each owns a piece of state the rest of the package reads, and
// check_test_alignment surfaced them as gaps in a file a fix had touched.

func TestBuildVolumeMap(t *testing.T) {
	tests := []struct {
		name      string
		filenames []string
		want      map[string]int
	}{
		{
			name:      "no filenames leaves the map empty",
			filenames: nil,
			want:      map[string]int{},
		},
		{
			name:      "highest volume number wins",
			filenames: []string{"movie.part01.rar", "movie.part03.rar", "movie.part02.rar"},
			want:      map[string]int{"movie": 3},
		},
		{
			name:      "a lower volume seen later does not lower the total",
			filenames: []string{"movie.part09.rar", "movie.part01.rar"},
			want:      map[string]int{"movie": 9},
		},
		{
			name:      "sets are tracked independently",
			filenames: []string{"movie.part02.rar", "show.part05.rar"},
			want:      map[string]int{"movie": 2, "show": 5},
		},
		{
			name:      "names that analyze to no set are skipped",
			filenames: []string{"readme.txt", "movie.part01.rar", "notes.nfo"},
			want:      map[string]int{"movie": 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &DirectUnpacker{
				allFilenames: tt.filenames,
				totalVolumes: make(map[string]int),
			}

			d.buildVolumeMap()

			if len(d.totalVolumes) != len(tt.want) {
				t.Fatalf("totalVolumes = %v, want %v", d.totalVolumes, tt.want)
			}
			for set, vol := range tt.want {
				if got := d.totalVolumes[set]; got != vol {
					t.Errorf("totalVolumes[%q] = %d, want %d", set, got, vol)
				}
			}
		})
	}
}

func TestSetQueued(t *testing.T) {
	tests := []struct {
		name       string
		nextSets   []string
		curSetname string
		query      string
		want       bool
	}{
		{"queued behind others", []string{"a", "b"}, "cur", "b", true},
		{"the first queued set", []string{"a", "b"}, "cur", "a", true},
		{"the set currently extracting", nil, "cur", "cur", true},
		{"neither queued nor current", []string{"a"}, "cur", "other", false},
		{"nothing queued and nothing current", nil, "", "a", false},
		{"queued while nothing is extracting", []string{"a"}, "", "a", true},
	}

	// setQueued("") is deliberately not a case here. Add returns before
	// touching any state when AnalyzeRarFilename yields "" (directunpack.go,
	// "not a RAR volume"), so "" never reaches nextSets or curSetname and the
	// question is unreachable. It answers true on an idle unpacker only
	// because `setname == d.curSetname` degenerates to `"" == ""`, which is
	// the zero-value collision Standing Design Rule 2 names -- pinning it as
	// intended behaviour would make a later guard against it look like a
	// regression.

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &DirectUnpacker{
				nextSets:   slices.Clone(tt.nextSets),
				curSetname: tt.curSetname,
			}

			if got := d.setQueued(tt.query); got != tt.want {
				t.Errorf("setQueued(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestRecordFailure(t *testing.T) {
	d := &DirectUnpacker{failedSets: make(map[string]FailedSet)}

	d.recordFailure("movie", "corrupt archive")

	if got := d.failedSets["movie"].Reason; got != "corrupt archive" {
		t.Errorf("failedSets[\"movie\"].Reason = %q, want %q", got, "corrupt archive")
	}

	// A second failure for the same set replaces the first, so the reason the
	// caller last supplied is the one Status reports.
	d.recordFailure("movie", "missing volume")

	if got := d.failedSets["movie"].Reason; got != "missing volume" {
		t.Errorf("after re-record, Reason = %q, want %q", got, "missing volume")
	}
	if len(d.failedSets) != 1 {
		t.Errorf("failedSets has %d entries, want 1", len(d.failedSets))
	}
}

func TestRecordSkipped(t *testing.T) {
	d := &DirectUnpacker{skippedSets: make(map[string]SkippedSet)}

	d.recordSkipped("movie", "not RAR5")
	d.recordSkipped("show", "not RAR5")

	if got := d.skippedSets["movie"].Reason; got != "not RAR5" {
		t.Errorf("skippedSets[\"movie\"].Reason = %q, want %q", got, "not RAR5")
	}
	if len(d.skippedSets) != 2 {
		t.Errorf("skippedSets has %d entries, want 2", len(d.skippedSets))
	}
}

// TestExtractEntries_ExitsBeforeReadingTheArchive covers the three ways
// extractEntries returns without consuming an entry. Each is reached before the
// first r.NextEntry() call, which is why a nil Reader is a sufficient argument
// here and why these cases need no archive fixture. A change that moved any of
// these checks below the read would dereference the nil and fail loudly rather
// than silently stop being an early exit.
func TestExtractEntries_ExitsBeforeReadingTheArchive(t *testing.T) {
	t.Run("an unusable extract dir is reported", func(t *testing.T) {
		notADir := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		d := &DirectUnpacker{extractDir: notADir}

		files, err := d.extractEntries(context.Background(), nil)

		if err == nil {
			t.Fatal("extractEntries() error = nil, want an open-root failure")
		}
		if !strings.Contains(err.Error(), "open root") {
			t.Errorf("error = %v, want it to mention open root", err)
		}
		if files != nil {
			t.Errorf("extractedFiles = %v, want nil", files)
		}
	})

	t.Run("a cancelled context stops the loop", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		d := &DirectUnpacker{extractDir: t.TempDir()}

		files, err := d.extractEntries(ctx, nil)

		if !errors.Is(err, context.Canceled) {
			t.Errorf("extractEntries() error = %v, want context.Canceled", err)
		}
		if files != nil {
			t.Errorf("extractedFiles = %v, want nil", files)
		}
	})

	t.Run("an aborted unpacker stops the loop", func(t *testing.T) {
		d := &DirectUnpacker{extractDir: t.TempDir(), killed: true}

		files, err := d.extractEntries(context.Background(), nil)

		if err == nil || !strings.Contains(err.Error(), "killed") {
			t.Errorf("extractEntries() error = %v, want it to mention killed", err)
		}
		if files != nil {
			t.Errorf("extractedFiles = %v, want nil", files)
		}
	})
}

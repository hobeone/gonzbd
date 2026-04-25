package sorting_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/sorting"
)

// ---- TestParse ----

func TestParse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		input      string
		wantType   sorting.MediaType
		wantTitle  string
		wantYear   int
		wantSeason int
		wantEp     int
		wantMonth  int
		wantDay    int
		wantRes    string
	}{
		{
			name:       "TV show S01E02",
			input:      "The.Good.Place.S01E02.720p",
			wantType:   sorting.TVMedia,
			wantTitle:  "The Good Place",
			wantSeason: 1,
			wantEp:     2,
			wantRes:    "720p",
		},
		{
			name:       "TV show with year",
			input:      "Battlestar.Galactica.2003.S02E05.1080p",
			wantType:   sorting.TVMedia,
			wantTitle:  "Battlestar Galactica 2003",
			wantYear:   2003,
			wantSeason: 2,
			wantEp:     5,
			wantRes:    "1080p",
		},
		{
			name:       "TV show uppercase SxxExx",
			input:      "Breaking.Bad.S03E10.BluRay",
			wantType:   sorting.TVMedia,
			wantTitle:  "Breaking Bad",
			wantSeason: 3,
			wantEp:     10,
		},
		{
			name:      "Movie with year",
			input:     "Blade.Runner.2049.BluRay.1080p",
			wantType:  sorting.MovieMedia,
			wantTitle: "Blade Runner",
			wantYear:  2049,
			wantRes:   "1080p",
		},
		{
			name:      "Movie with older year",
			input:     "The.Godfather.1972.720p",
			wantType:  sorting.MovieMedia,
			wantTitle: "The Godfather",
			wantYear:  1972,
			wantRes:   "720p",
		},
		{
			name:      "Dated show YYYY-MM-DD",
			input:     "The.Daily.Show.2024-03-15.720p",
			wantType:  sorting.DatedMedia,
			wantTitle: "The Daily Show",
			wantYear:  2024,
			wantMonth: 3,
			wantDay:   15,
			wantRes:   "720p",
		},
		{
			name:      "Dated show YYYY.MM.DD",
			input:     "Late.Night.2023.11.28.HDTV",
			wantType:  sorting.DatedMedia,
			wantTitle: "Late Night",
			wantYear:  2023,
			wantMonth: 11,
			wantDay:   28,
		},
		{
			name:      "Unknown media no year no TV",
			input:     "Some_Random_Release",
			wantType:  sorting.UnknownMedia,
			wantTitle: "Some Random Release",
		},
		{
			name:    "4K resolution normalised",
			input:   "Movie.2020.4K",
			wantRes: "2160p",
		},
		{
			name:      "2160p resolution",
			input:     "Movie.Title.2019.2160p",
			wantType:  sorting.MovieMedia,
			wantTitle: "Movie Title",
			wantYear:  2019,
			wantRes:   "2160p",
		},
		{
			name:       "TV with episode name",
			input:      "Scrubs.S01E01.Pilot.BluRay.720p",
			wantType:   sorting.TVMedia,
			wantTitle:  "Scrubs",
			wantSeason: 1,
			wantEp:     1,
			wantRes:    "720p",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sorting.Parse(tc.input)

			if tc.wantType != 0 && got.Type != tc.wantType {
				t.Errorf("Type: got %v, want %v", got.Type, tc.wantType)
			}
			if tc.wantTitle != "" && got.Title != tc.wantTitle {
				t.Errorf("Title: got %q, want %q", got.Title, tc.wantTitle)
			}
			if tc.wantYear != 0 && got.Year != tc.wantYear {
				t.Errorf("Year: got %d, want %d", got.Year, tc.wantYear)
			}
			if tc.wantSeason != 0 && got.Season != tc.wantSeason {
				t.Errorf("Season: got %d, want %d", got.Season, tc.wantSeason)
			}
			if tc.wantEp != 0 && got.Episode != tc.wantEp {
				t.Errorf("Episode: got %d, want %d", got.Episode, tc.wantEp)
			}
			if tc.wantMonth != 0 && got.Month != tc.wantMonth {
				t.Errorf("Month: got %d, want %d", got.Month, tc.wantMonth)
			}
			if tc.wantDay != 0 && got.Day != tc.wantDay {
				t.Errorf("Day: got %d, want %d", got.Day, tc.wantDay)
			}
			if tc.wantRes != "" && got.Resolution != tc.wantRes {
				t.Errorf("Resolution: got %q, want %q", got.Resolution, tc.wantRes)
			}
		})
	}
}

// ---- TestExpandTemplate ----

func TestExpandTemplate(t *testing.T) {
	t.Parallel()

	info := sorting.MediaInfo{
		Type:        sorting.TVMedia,
		Title:       "The Good Place",
		Year:        2016,
		Season:      1,
		Episode:     4,
		EpisodeName: "Jason Mendoza",
		Month:       3,
		Day:         7,
		Resolution:  "1080p",
	}

	cases := []struct {
		name    string
		tmpl    string
		ext     string
		wantOut string
	}{
		{"title", "TV/%t/%t.%ext", ".mkv", "TV/The Good Place/The Good Place.mkv"},
		{"title dot", "TV/%.t", "", "TV/The.Good.Place"},
		{"title underscore", "TV/%_t", "", "TV/The_Good_Place"},
		{"year", "%y", "", "2016"},
		{"decade", "%decade", "", "2010s"},
		{"season unpadded", "S%s", "", "S1"},
		{"season padded", "S%0s", "", "S01"},
		{"episode unpadded", "E%e", "", "E4"},
		{"episode padded", "E%0e", "", "E04"},
		{"season+episode", "S%0sE%0e", "", "S01E04"},
		{"episode name", "%en", "", "Jason Mendoza"},
		{"episode name dot", "%e.n", "", "Jason.Mendoza"},
		{"episode name underscore", "%e_n", "", "Jason_Mendoza"},
		{"resolution", "%r", "", "1080p"},
		{"month unpadded", "%m", "", "3"},
		{"month padded", "%0m", "", "03"},
		{"day unpadded", "%d", "", "7"},
		{"day padded", "%0d", "", "07"},
		{"ext token", "file.%ext", ".mkv", "file.mkv"},
		{"ext empty", "file.%ext", "", "file."},
		// Critical: %ext must match before %e
		{"ext before e in string", "%ext.%e", ".mkv", "mkv.4"},
		{"unknown token preserved", "%z remains", "", "%z remains"},
		{"full TV path", "TV/%t/Season %0s/%t S%0sE%0e %en.%ext", ".mkv",
			"TV/The Good Place/Season 01/The Good Place S01E04 Jason Mendoza.mkv"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sorting.ExpandTemplate(tc.nzfl, info, tc.ext)
			if got != tc.wantOut {
				t.Errorf("ExpandTemplate(%q, ..., %q)\n  got  %q\n  want %q",
					tc.nzfl, tc.ext, got, tc.wantOut)
			}
		})
	}
}

// ---- TestApply ----

func TestApply(t *testing.T) {
	t.Parallel()

	t.Run("moves files when rule matches", func(t *testing.T) {
		t.Parallel()

		srcDir := t.TempDir()
		destRoot := t.TempDir()

		// Create fake files in srcDir.
		writeFile(t, srcDir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv", 5000)
		writeFile(t, srcDir, "b082fa0beaa644d3aa01045d5b8d0b36.nfo", 100)

		rules := []sorting.SorterRule{
			{
				Name:       "TV rule",
				Enabled:    true,
				SortString: "TV/%t/Season %0s",
				Categories: []string{},
				Types:      []sorting.MediaType{sorting.TVMedia},
			},
		}

		result, err := sorting.Apply(
			context.Background(),
			srcDir, "tv", "The.Good.Place.S02E04.1080p",
			5100,
			rules,
			destRoot,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result.MatchedRule != "TV rule" {
			t.Errorf("MatchedRule: got %q, want %q", result.MatchedRule, "TV rule")
		}
		if len(result.Moved) != 2 {
			t.Errorf("Moved: got %d files, want 2", len(result.Moved))
		}

		// Files should exist under destRoot/TV/The Good Place/Season 02.
		expectedDir := filepath.Join(destRoot, "TV", "The Good Place", "Season 02")
		entries, err := os.ReadDir(expectedDir)
		if err != nil {
			t.Fatalf("expected destination dir not found: %s: %v", expectedDir, err)
		}
		if len(entries) != 2 {
			t.Errorf("expected 2 files in dest dir, got %d", len(entries))
		}

		// srcDir should be empty.
		srcEntries, _ := os.ReadDir(srcDir)
		if len(srcEntries) != 0 {
			t.Errorf("expected srcDir to be empty after move, got %d entries", len(srcEntries))
		}
	})

	t.Run("no rule matches returns empty result", func(t *testing.T) {
		t.Parallel()

		srcDir := t.TempDir()
		destRoot := t.TempDir()
		writeFile(t, srcDir, "some.mkv", 1000)

		rules := []sorting.SorterRule{
			{
				Name:       "Movie rule",
				Enabled:    true,
				SortString: "Movies/%t (%y)",
				Types:      []sorting.MediaType{sorting.MovieMedia},
			},
		}

		// TV job should not match movie rule.
		result, err := sorting.Apply(
			context.Background(),
			srcDir, "video", "Breaking.Bad.S01E01",
			1000,
			rules,
			destRoot,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result.MatchedRule != "" {
			t.Errorf("expected no match, got rule %q", result.MatchedRule)
		}
		if len(result.Moved) != 0 {
			t.Errorf("expected no moves, got %d", len(result.Moved))
		}
	})

	t.Run("disabled rule is skipped", func(t *testing.T) {
		t.Parallel()

		srcDir := t.TempDir()
		destRoot := t.TempDir()
		writeFile(t, srcDir, "movie.mkv", 1000)

		rules := []sorting.SorterRule{
			{
				Name:       "disabled rule",
				Enabled:    false,
				SortString: "Movies/%t (%y)",
			},
		}

		result, err := sorting.Apply(
			context.Background(),
			srcDir, "", "Blade.Runner.2049",
			1000, rules, destRoot,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result.MatchedRule != "" {
			t.Errorf("disabled rule should not match, got %q", result.MatchedRule)
		}
	})

	t.Run("size filter respected", func(t *testing.T) {
		t.Parallel()

		srcDir := t.TempDir()
		destRoot := t.TempDir()
		writeFile(t, srcDir, "movie.mkv", 1000)

		rules := []sorting.SorterRule{
			{
				Name:       "big movie rule",
				Enabled:    true,
				SortString: "Movies/%t",
				Min:        int64(10 * 1024 * 1024 * 1024), // 10 GB min
			},
		}

		result, err := sorting.Apply(
			context.Background(),
			srcDir, "", "Blade.Runner.2049",
			1000, rules, destRoot,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result.MatchedRule != "" {
			t.Errorf("expected size filter to block match, got %q", result.MatchedRule)
		}
	})

	t.Run("category filter respected", func(t *testing.T) {
		t.Parallel()

		srcDir := t.TempDir()
		destRoot := t.TempDir()
		writeFile(t, srcDir, "movie.mkv", 1000)

		rules := []sorting.SorterRule{
			{
				Name:       "movies category rule",
				Enabled:    true,
				SortString: "Movies/%t",
				Categories: []string{"movies"},
			},
		}

		result, err := sorting.Apply(
			context.Background(),
			srcDir, "tv", "Blade.Runner.2049",
			1000, rules, destRoot,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result.MatchedRule != "" {
			t.Errorf("expected category filter to block match, got %q", result.MatchedRule)
		}

		// Now with the right category — should match.
		writeFile(t, srcDir, "movie.mkv", 1000) // restore since it wasn't moved
		result2, err := sorting.Apply(
			context.Background(),
			srcDir, "movies", "Blade.Runner.2049",
			1000, rules, destRoot,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result2.MatchedRule != "movies category rule" {
			t.Errorf("expected category match, got %q", result2.MatchedRule)
		}
	})
}

// writeFile creates a file in dir with the given name and size (zero bytes).
func writeFile(t *testing.T, dir, name string, size int) {
	t.Helper()
	path := filepath.Join(dir, name)
	data := make([]byte, size)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writeFile %s: %v", path, err)
	}
}

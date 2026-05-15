package directunpack

import (
	"testing"
)

func TestAnalyzeRarFilename(t *testing.T) {
	tests := []struct {
		filename    string
		wantSetname string
		wantVolume  int
	}{
		// New-style multi-part naming.
		{"movie.part01.rar", "movie", 1},
		{"movie.part02.rar", "movie", 2},
		{"movie.part10.rar", "movie", 10},
		{"movie.part99.rar", "movie", 99},
		{"My.Movie.2024.1080p.part01.rar", "my.movie.2024.1080p", 1},
		{"My.Movie.2024.1080p.part03.rar", "my.movie.2024.1080p", 3},

		// Case insensitivity.
		{"Movie.Part01.RAR", "movie", 1},
		{"MOVIE.PART05.Rar", "movie", 5},

		// Legacy main volume.
		{"movie.rar", "movie", 1},
		{"My.Movie.2024.rar", "my.movie.2024", 1},
		{"MOVIE.RAR", "movie", 1},

		// Legacy extra volumes (r00 = volume 2).
		{"movie.r00", "movie", 2},
		{"movie.r01", "movie", 3},
		{"movie.r09", "movie", 11},
		{"movie.r99", "movie", 101},
		{"My.Movie.2024.r00", "my.movie.2024", 2},
		{"MOVIE.R05", "movie", 7},

		// With directory components (only base name matters).
		{"/path/to/downloads/movie.part01.rar", "movie", 1},
		{"/path/to/downloads/movie.r00", "movie", 2},
		{"subdir/movie.rar", "movie", 1},

		// Non-RAR files → ("", 0).
		{"readme.nfo", "", 0},
		{"movie.mkv", "", 0},
		{"movie.par2", "", 0},
		{"movie.vol00+01.par2", "", 0},
		{"movie.7z", "", 0},
		{"movie.zip", "", 0},
		{"movie.001", "", 0},
		{"", "", 0},
		{"noext", "", 0},

		// Edge cases: .rar in directory name but not in filename.
		{"/path/to/movie.rar.d/readme.txt", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			gotSetname, gotVolume := AnalyzeRarFilename(tt.filename)
			if gotSetname != tt.wantSetname || gotVolume != tt.wantVolume {
				t.Errorf("AnalyzeRarFilename(%q) = (%q, %d), want (%q, %d)",
					tt.filename, gotSetname, gotVolume, tt.wantSetname, tt.wantVolume)
			}
		})
	}
}

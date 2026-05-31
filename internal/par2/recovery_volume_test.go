package par2_test

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/par2"
)

func TestIsRecoveryVolume(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{
			name:  "index file",
			input: "movie.par2",
			want:  false,
		},
		{
			name:  "recovery volume single-digit counts",
			input: "movie.vol000+01.par2",
			want:  true,
		},
		{
			name:  "recovery volume multi-digit counts",
			input: "movie.vol123+45.par2",
			want:  true,
		},
		{
			name:  "uppercase extension",
			input: "movie.VOL12+34.PAR2",
			want:  true,
		},
		{
			name:  "NZB subject with recovery volume",
			input: `[3/12] - "movie.vol000+01.par2" yEnc (1/42)`,
			want:  true,
		},
		{
			name:  "NZB subject with index file",
			input: `[1/12] - "movie.par2" yEnc (1/3)`,
			want:  false,
		},
		{
			name:  "non-par2 file",
			input: "movie.mkv",
			want:  false,
		},
		{
			name:  "vol-looking name but no .par2 extension",
			input: "movie.vol001+02.rar",
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := par2.IsRecoveryVolume(tc.input)
			if got != tc.want {
				t.Errorf("IsRecoveryVolume(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

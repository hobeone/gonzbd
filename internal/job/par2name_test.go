package job

import "testing"

func TestIsRecoveryVolume(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"movie.vol000+01.par2", true},
		{"movie.vol12-34.par2", true},
		{"Movie.VOL07+08.PAR2", true},
		{"movie.par2", false}, // index file, not a recovery volume
		{"movie.mkv", false},
		{"movie.vol000.par2", false}, // missing the +NN/-NN block count
		{"movie.part01.rar", false},
		{"movie.vol000+01.par2.1", false},
		{`[3/12] - "movie.vol000+01.par2" yEnc (1/42)`, true},
		{`[1/12] - "movie.par2" yEnc (1/3)`, false},
		{"movie.vol03-07.par2", true},
		{"movie.vol001+02.rar", false},
		// A zero-block volume carries no recovery capacity, so it is content.
		{"movie.vol000+00.par2", false},
		{"movie.vol012+0.par2", false},
		{`[2/12] - "movie.vol000+00.par2" yEnc (1/1)`, false},
		{"movie.vol000+10.par2", true},
		{"movie.vol000+100.par2", true},
		// In the - form the second number is a volume number, not a block count.
		{"movie.vol00-00.par2", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRecoveryVolume(tt.name); got != tt.want {
				t.Errorf("IsRecoveryVolume(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

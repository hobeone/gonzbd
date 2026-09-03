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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRecoveryVolume(tt.name); got != tt.want {
				t.Errorf("IsRecoveryVolume(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

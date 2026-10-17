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
		// An out-of-range count is still a volume, with no usable count.
		{"movie.vol00+70000.par2", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRecoveryVolume(tt.name); got != tt.want {
				t.Errorf("IsRecoveryVolume(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestRecoveryVolumeBlocks(t *testing.T) {
	tests := []struct {
		name   string
		blocks int
		ok     bool
	}{
		{"movie.vol00+01.par2", 1, true},
		{"movie.vol63+13.par2", 13, true},
		{"Movie.VOL07+08.PAR2", 8, true},
		{"movie.vol000+100.par2", 100, true},
		{`[3/12] - "movie.vol31+32.par2" yEnc (1/97)`, 32, true},
		// The - form names a volume number, not a block count.
		{"movie.vol03-07.par2", 0, false},
		// PAR2 recovery exponents are 16-bit, so a larger count is untrusted.
		{"movie.vol00+65535.par2", 65535, true},
		{"movie.vol00+65536.par2", 0, false},
		{"movie.vol00+9223372036854775807.par2", 0, false},
		{"movie.vol00+99999999999999999999.par2", 0, false},
		// Not recovery volumes at all.
		{"movie.vol000+00.par2", 0, false},
		{"movie.par2", 0, false},
		{"movie.mkv", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks, ok := RecoveryVolumeBlocks(tt.name)
			if blocks != tt.blocks || ok != tt.ok {
				t.Errorf("RecoveryVolumeBlocks(%q) = (%d, %v), want (%d, %v)", tt.name, blocks, ok, tt.blocks, tt.ok)
			}
		})
	}
}

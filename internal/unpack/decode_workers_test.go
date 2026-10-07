package unpack

import (
	"log/slog"
	"testing"
)

func TestDecodeWorkers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		setting, cpus int
		want          int
	}{
		{"auto on 2 CPUs", 0, 2, 2},
		{"auto on 4 CPUs", 0, 4, 4},
		{"auto on 32 CPUs capped", 0, 32, 4},
		{"auto on 1 CPU", 0, 1, 1},
		{"auto with bogus CPU count", 0, 0, 1},
		{"serial", 1, 32, 1},
		{"explicit above auto cap", 6, 2, 6},
		{"negative is auto", -3, 32, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := DecodeWorkers(tt.setting, tt.cpus); got != tt.want {
				t.Errorf("DecodeWorkers(%d, %d) = %d, want %d", tt.setting, tt.cpus, got, tt.want)
			}
		})
	}
}

func TestGoUnRAR_DecodeWorkers(t *testing.T) {
	for _, workers := range []int{0, 1, 4} {
		outDir := t.TempDir()
		archive := Archive{Type: RarArchive, Name: "single_rar5", MainFile: "testdata/single_rar5.rar"}
		if _, err := GoUnRAR(t.Context(), slog.Default(), archive, outDir, "", Options{DecodeWorkers: workers}); err != nil {
			t.Fatalf("workers=%d: GoUnRAR: %v", workers, err)
		}
	}
}

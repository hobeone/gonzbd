package unpack

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// compressedFixtureText is the plaintext inside testdata/compressed_rar5.rar,
// which was made with
//
//	rar a -ma5 -ep -m3 -md128k compressed_rar5.rar compressed.txt
//
// from exactly these 6000 lines (330 KB, 6.8 KB packed). A compressed member
// is what the worker pipeline decodes; the other fixtures here are stored
// (-m0) and never reach it.
func compressedFixtureText() []byte {
	var b bytes.Buffer
	for i := range 6000 {
		fmt.Fprintf(&b, "line %06d the quick brown fox jumps over the lazy dog\n", i)
	}
	return b.Bytes()
}

// TestGoUnRAR_DecodeWorkers extracts a compressed member through the auto,
// serial and four-worker settings and checks the bytes each time. It pins
// that the parallel decoder delivers the same bytes as the serial one through
// GoUnRAR; it cannot observe which worker count the Reader was given, so the
// setting's resolution is pinned separately by TestDecodeWorkers.
func TestGoUnRAR_DecodeWorkers(t *testing.T) {
	want := compressedFixtureText()
	for _, workers := range []int{0, 1, 4} {
		outDir := t.TempDir()
		archive := Archive{Type: RarArchive, Name: "compressed_rar5", MainFile: "testdata/compressed_rar5.rar"}
		if _, err := GoUnRAR(t.Context(), slog.Default(), archive, outDir, "", Options{DecodeWorkers: workers}); err != nil {
			t.Fatalf("workers=%d: GoUnRAR: %v", workers, err)
		}
		got, err := os.ReadFile(filepath.Join(outDir, "compressed.txt"))
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("workers=%d: extracted %d bytes, want %d; content differs", workers, len(got), len(want))
		}
	}
}

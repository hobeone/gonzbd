package fsutil

import (
	"strings"
	"testing"
)

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty", "", "unknown"},
		{"basic", "test.bin", "test.bin"},
		{"illegal chars", "test?file*.bin", "test_file_.bin"},
		{"control chars", "test\x01file.bin", "test_file.bin"},
		{"windows device", "CON.txt", "_CON.txt"},
		{"windows device prefix", "prn", "_prn"},
		{"windows device case", "aux.bin", "_aux.bin"},
		{"mft", "$mft.bin", "Smft.bin"},
		{"long filename", strings.Repeat("a", 300) + ".bin", strings.Repeat("a", 241) + ".bin"},
		{"long with multi-byte", strings.Repeat("🚀", 100) + ".bin", strings.Repeat("🚀", 60) + ".bin"}, // 🚀 is 4 bytes, 60*4 = 240
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeFilename(tt.input)
			if got != tt.expected {
				t.Errorf("SanitizeFilename(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestTruncateFilename(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxBytes int
		expected string
	}{
		{"no truncate", "test.bin", 10, "test.bin"},
		{"truncate", "testing.bin", 8, "test.bin"}, // base "testing" -> "test", ext ".bin"
		{"multi-byte", "🚀🚀🚀.bin", 10, "🚀.bin"}, // 🚀 is 4 bytes, 4 + 4 = 8
		{"only ext", ".hugeextension", 5, ".huge"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateFilename(tt.input, tt.maxBytes)
			if got != tt.expected {
				t.Errorf("truncateFilename(%q, %d) = %q; want %q", tt.input, tt.maxBytes, got, tt.expected)
			}
			if len(got) > tt.maxBytes {
				t.Errorf("len(%q) = %d; want <= %d", got, len(got), tt.maxBytes)
			}
		})
	}
}

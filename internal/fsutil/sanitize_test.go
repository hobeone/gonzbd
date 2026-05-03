package fsutil

import (
	"strings"
	"testing"
)

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		opts     SanitizeOptions
		expected string
	}{
		{"empty", "", SanitizeOptions{}, "unknown"},
		{"basic", "test.bin", SanitizeOptions{}, "test.bin"},
		{"illegal chars", "test?file*.bin", SanitizeOptions{}, "test_file_.bin"},
		{"control chars", "test\x01file.bin", SanitizeOptions{}, "test_file.bin"},
		{"custom illegal", "test?file.bin", SanitizeOptions{ReplaceIllegalWith: "!"}, "test!file.bin"},
		{"custom spaces", "my file.bin", SanitizeOptions{ReplaceSpacesWith: "."}, "my.file.bin"},
		{"strip diacritics", "éöñ.bin", SanitizeOptions{StripDiacritics: true}, "eon.bin"},
		{"preserve emoji with diacritics", "é🚀ñ.bin", SanitizeOptions{StripDiacritics: true}, "e🚀n.bin"},
		{"windows device", "CON.txt", SanitizeOptions{}, "_CON.txt"},
		{"windows device prefix", "prn", SanitizeOptions{}, "_prn"},
		{"windows device case", "aux.bin", SanitizeOptions{}, "_aux.bin"},
		{"mft", "$mft.bin", SanitizeOptions{}, "Smft.bin"},
		{"long filename", strings.Repeat("a", 300) + ".bin", SanitizeOptions{}, strings.Repeat("a", 241) + ".bin"},
		{"long with multi-byte", strings.Repeat("🚀", 100) + ".bin", SanitizeOptions{}, strings.Repeat("🚀", 60) + ".bin"}, // 🚀 is 4 bytes, 60*4 = 240
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeFilename(tt.input, tt.opts)
			if got != tt.expected {
				t.Errorf("SanitizeFilename(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}
func TestSanitizeFolderName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		opts     SanitizeOptions
		expected string
	}{
		{"empty", "", SanitizeOptions{}, "unknown"},
		{"basic", "My Show", SanitizeOptions{}, "My Show"},
		{"trailing dots", "My Show...", SanitizeOptions{}, "My Show"},
		{"trailing spaces", "My Show   ", SanitizeOptions{}, "My Show"},
		{"illegal and trailing", "My:Show?...", SanitizeOptions{}, "My_Show_"},
		{"windows device", "CON", SanitizeOptions{}, "_CON"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeFolderName(tt.input, tt.opts)
			if got != tt.expected {
				t.Errorf("SanitizeFolderName(%q) = %q; want %q", tt.input, got, tt.expected)
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
		{"multi-byte", "🚀🚀🚀.bin", 10, "🚀.bin"},     // 🚀 is 4 bytes, 4 + 4 = 8
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

func TestCleanupName(t *testing.T) {
	patterns := []string{
		`^(?i)\[PRiVATE\]-?`,
		`^(?i)\[nzbndx\]-?`,
		`^(?i)\[DrunkenSlug\]-?`,
		`^(?i)\[Geek\]-?`,
		`^\[.*\]-?`,
		`^(?i)www\..*\.[a-z]{2,3}-?`,
		`(?i)-? ?\(Scenzbd\)$`,
		`(?i)-? ?\(Obfuscated\)$`,
		`(?i)-? ?\(NZBGeek\)$`,
	}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"no match", "My.Show.S01E01", "My.Show.S01E01"},
		{"private prefix", "[PRiVATE]-My.Show.S01E01", "My.Show.S01E01"},
		{"indexer prefix", "[www.example.com]-My.Show.S01E01", "My.Show.S01E01"},
		{"drunken slug prefix", "[DrunkenSlug] My.Show.S01E01", "My.Show.S01E01"},
		{"brackets prefix", "[Something]-My.Show.S01E01", "My.Show.S01E01"},
		{"scenzbd suffix", "My.Show.S01E01(Scenzbd)", "My.Show.S01E01"},
		{"obfuscated suffix", "My.Show.S01E01-(Obfuscated)", "My.Show.S01E01"},
		{"geek suffix", "My.Show.S01E01-(NZBGeek)", "My.Show.S01E01"},
		{"multiple cleanup", "[www.indexer.pro]-[PRiVATE]-My.Movie.2024-(Scenzbd)", "My.Movie.2024"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CleanupName(tt.input, patterns)
			if got != tt.expected {
				t.Errorf("CleanupName(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}

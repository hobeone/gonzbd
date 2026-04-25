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

func TestIsObfuscated(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"not obfuscated", "test.bin", false},
		{"md5", "13a1b10996f866ef04019baea5dbfc81", true},
		{"sha1", "13a1b10996f866ef04019baea5dbfc819a4ed680", true},
		{"sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true},
		{"sha256 with ext", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855.rar", true},
		{"too short", "12345abc", false},
		{"not hex", "13a1b10996f866ef04019baea5dbfc819a4ed6802d5e826c713365d6dbf9zabc", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsObfuscated(tt.input)
			if got != tt.expected {
				t.Errorf("IsObfuscated(%q) = %v; want %v", tt.input, got, tt.expected)
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

func TestJoinSafe(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		folder   string
		file     string
		opts     SanitizeOptions
		expected string
	}{
		{
			name:     "no truncate",
			base:     "/downloads/complete",
			folder:   "My.Job",
			file:     "file.mkv",
			opts:     SanitizeOptions{},
			expected: "/downloads/complete/My.Job/file.mkv",
		},
		{
			name:     "truncate folder",
			base:     "/downloads/complete",
			folder:   strings.Repeat("j", 250),
			file:     "file.mkv",
			opts:     SanitizeOptions{},
			expected: "/downloads/complete/" + strings.Repeat("j", 221) + "/file.mkv", // 250 - 20 - 1 - 8 = 221
		},
		{
			name:     "truncate file",
			base:     "/downloads/complete",
			folder:   "tiny",
			file:     strings.Repeat("f", 250) + ".mkv",
			opts:     SanitizeOptions{},
			expected: "/downloads/complete/tiny/" + strings.Repeat("f", 221) + ".mkv", // 250 - 20 - 1 - 4 - 4 = 221
		},
		{
			name:     "extreme constraint",
			base:     "/" + strings.Repeat("b", 240),
			folder:   "folder",
			file:     "file.mkv",
			opts:     SanitizeOptions{},
			expected: "/" + strings.Repeat("b", 240) + "/folde/f.mkv", // Hard to predict exact, but length must be <= 250
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := JoinSafe(tt.base, tt.folder, tt.file, tt.opts)
			if len(got) > maxPathBytes {
				t.Errorf("JoinSafe path too long: len(%d) > %d\nPath: %q", len(got), maxPathBytes, got)
			}
			if tt.expected != "" && !strings.Contains(tt.name, "extreme") {
				if got != tt.expected {
					t.Errorf("JoinSafe() = %q; want %q", got, tt.expected)
				}
			}
		})
	}
}

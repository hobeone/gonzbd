package fsutil

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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
		{"long filename", strings.Repeat("a", 300) + ".bin", SanitizeOptions{}, strings.Repeat("a", 251) + ".bin"},
		{"long with multi-byte", strings.Repeat("🚀", 100) + ".bin", SanitizeOptions{}, strings.Repeat("🚀", 62) + ".bin"}, // 🚀 is 4 bytes, 62*4+4 = 252 ≤ 255
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

// TestSanitizeFilename_NameMaxBoundary verifies that filenames ending up
// at exactly NAME_MAX (255 bytes) are accepted unchanged, and that the
// truncation does not split a multi-byte UTF-8 rune at the boundary.
func TestSanitizeFilename_NameMaxBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{"exactly 255", strings.Repeat("a", 251) + ".bin"},
		{"256 -> truncated", strings.Repeat("a", 252) + ".bin"},
		{"500 ascii", strings.Repeat("a", 500) + ".bin"},
		// Three-byte runes (CJK): ensure boundary doesn't slice mid-rune.
		{"3-byte runes long", strings.Repeat("漢", 100) + ".bin"},
		// Four-byte runes (emoji): every other length-budget would tempt a split.
		{"4-byte runes long", strings.Repeat("🚀", 100) + ".bin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SanitizeFilename(tt.input, SanitizeOptions{})
			if len(got) > maxFilenameBytes {
				t.Errorf("len(%q) = %d > %d", got, len(got), maxFilenameBytes)
			}
			if !utf8.ValidString(got) {
				t.Errorf("result is not valid UTF-8: %q", got)
			}
			if !strings.HasSuffix(got, ".bin") {
				t.Errorf("extension lost: %q", got)
			}
		})
	}
}

// TestSanitizeFolderName_DotHeavyName guards against the "release name has
// many dots, last segment treated as extension" trap. The folder name from
// the production bug — heavily dotted — should be returned unchanged when
// it already fits in NAME_MAX.
func TestSanitizeFolderName_DotHeavyName(t *testing.T) {
	t.Parallel()
	name := "Daemons.of.the.Shadow.Realm.S01E06.The.Kagemori.Clan.and.the.Unknown.Assailants.1080p.NF.WEB-DL.JPN.AAC2.0.H.264.MSubs-ToonsHub"
	if len(name) > maxFilenameBytes {
		t.Fatalf("test fixture too long: %d bytes", len(name))
	}
	got := SanitizeFolderName(name, SanitizeOptions{})
	if got != name {
		t.Errorf("dot-heavy folder altered:\n  got:  %q\n  want: %q", got, name)
	}
}

// TestSanitizeFolderName_OverlongTruncatesPreservingTail verifies that
// folder names exceeding NAME_MAX are truncated and that the "extension"
// (everything after the last dot) is preserved — important for keeping
// release-group tags like ".MSubs-ToonsHub" visible.
func TestSanitizeFolderName_OverlongTruncatesPreservingTail(t *testing.T) {
	t.Parallel()
	name := strings.Repeat("A", 300) + ".GROUP"
	got := SanitizeFolderName(name, SanitizeOptions{})
	if len(got) > maxFilenameBytes {
		t.Errorf("len = %d > %d", len(got), maxFilenameBytes)
	}
	if !strings.HasSuffix(got, ".GROUP") {
		t.Errorf("tail not preserved: %q", got)
	}
}

// TestJoinSafe_FolderStableAcrossFilenames is the production-shaped
// regression test for the multi-folder bug: the same job folder with two
// very different filename lengths must produce paths sharing the same
// directory.
func TestJoinSafe_FolderStableAcrossFilenames(t *testing.T) {
	t.Parallel()
	base := "/mnt/nas/public/usenet/incomplete"
	folder := "Daemons.of.the.Shadow.Realm.S01E06.The.Kagemori.Clan.and.the.Unknown.Assailants.1080p.NF.WEB-DL.JPN.AAC2.0.H.264.MSubs-ToonsHub"
	files := []string{
		"x.par2",
		folder + ".part01.rar",
		folder + ".vol000+01.par2",
		folder + ".sample.mkv",
		strings.Repeat("Q", 200) + ".rar",
	}

	dirs := make([]string, 0, len(files))
	for _, f := range files {
		path := JoinSafe(base, folder, f, SanitizeOptions{})
		dirs = append(dirs, filepath.Dir(path))
	}

	for i := 1; i < len(dirs); i++ {
		if dirs[i] != dirs[0] {
			t.Errorf("file %q produced different dir than file %q:\n  %s\n  %s",
				files[i], files[0], dirs[i], dirs[0])
		}
	}
}

// TestJoinSafe_EmptyAndEdge covers degenerate combinations.
func TestJoinSafe_EmptyAndEdge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		base   string
		folder string
		file   string
		want   string
	}{
		{"all empty", "/dl", "", "", "/dl"},
		{"only base", "/dl", "", "", "/dl"},
		{"only folder", "/dl", "f", "", "/dl/f"},
		{"only file", "/dl", "", "x.bin", "/dl/x.bin"},
		{"trailing dots stripped from folder", "/dl", "name...", "x", "/dl/name/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := JoinSafe(tt.base, tt.folder, tt.file, SanitizeOptions{})
			if got != tt.want {
				t.Errorf("JoinSafe(%q, %q, %q) = %q; want %q",
					tt.base, tt.folder, tt.file, got, tt.want)
			}
		})
	}
}

// TestJoinSafe_NoFileExtension guards filenames with no extension — a
// case where filepath.Ext returns "" and truncation must still preserve
// data without producing an empty result.
func TestJoinSafe_NoFileExtension(t *testing.T) {
	t.Parallel()
	got := JoinSafe("/dl", "folder", strings.Repeat("a", 400), SanitizeOptions{})
	fileComponent := filepath.Base(got)
	if len(fileComponent) > maxFilenameBytes {
		t.Errorf("file component %d bytes > %d", len(fileComponent), maxFilenameBytes)
	}
	if fileComponent == "" {
		t.Errorf("file component empty: %q", got)
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

	opts := SanitizeOptions{CleanupList: patterns}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CleanupName(tt.input, opts)
			if got != tt.expected {
				t.Errorf("CleanupName(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}

	// Verify pre-compiled path produces same results.
	opts.CleanupRegexps = CompileCleanupList(patterns)
	for _, tt := range tests {
		t.Run("precompiled/"+tt.name, func(t *testing.T) {
			got := CleanupName(tt.input, opts)
			if got != tt.expected {
				t.Errorf("CleanupName(%q) precompiled = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// L13: Verify null bytes in filenames are sanitized to the replacement character.
func TestSanitizeFilename_NullByte(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"null byte in middle", "test\x00file.bin", "test_file.bin"},
		{"null byte at start", "\x00file.bin", "_file.bin"},
		{"null byte at end", "file\x00.bin", "file_.bin"},
		{"multiple null bytes", "te\x00st\x00.bin", "te_st_.bin"},
		{"only null bytes", "\x00\x00\x00", "___"},
		{"null with other control", "\x00\x01\x02.txt", "___.txt"},
		{"null in extension", "file.\x00bin", "file._bin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SanitizeFilename(tt.input, SanitizeOptions{})
			if got != tt.want {
				t.Errorf("SanitizeFilename(%q) = %q; want %q", tt.input, got, tt.want)
			}
			// Extra invariant: result must never contain a null byte.
			if strings.ContainsRune(got, 0) {
				t.Errorf("result contains null byte: %q", got)
			}
		})
	}
}

// T8: Dedicated replaceWinDevices tests covering device names and $mft.
func TestReplaceWinDevices(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Standard reserved device names.
		{"CON", "CON", "_CON"},
		{"con lowercase", "con", "_con"},
		{"PRN", "PRN", "_PRN"},
		{"AUX", "AUX", "_AUX"},
		{"NUL", "NUL", "_NUL"},
		{"COM1", "COM1", "_COM1"},
		{"COM9", "COM9", "_COM9"},
		{"LPT1", "LPT1", "_LPT1"},
		{"LPT9", "LPT9", "_LPT9"},

		// Device name with extension — HasPrefix(lower, resLower+".").
		{"CON.txt", "CON.txt", "_CON.txt"},
		{"prn.dat", "prn.dat", "_prn.dat"},
		{"aux.bin", "aux.bin", "_aux.bin"},
		{"com1.log", "com1.log", "_com1.log"},

		// $mft special case — replace $ with S.
		{"$mft", "$mft", "Smft"},
		{"$MFT", "$MFT", "SMFT"},
		{"$mft.bin", "$mft.bin", "Smft.bin"},

		// Safe names — should pass through unchanged.
		{"normal file", "normal.txt", "normal.txt"},
		{"connect (not CON)", "connect", "connect"},
		{"concerto", "concerto.mp3", "concerto.mp3"},
		{"lpt10 (not reserved)", "lpt10", "lpt10"},
		{"com10", "com10", "com10"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := replaceWinDevices(tt.input)
			if got != tt.want {
				t.Errorf("replaceWinDevices(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSanitizeCoreDirect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		opts     SanitizeOptions
		expected string
	}{
		{"basic", " hello world ", SanitizeOptions{}, "hello world"},
		{"strip diacritics", "éöñ", SanitizeOptions{StripDiacritics: true}, "eon"},
		// Default ReplaceIllegalWith is "_"; this is the most common production path.
		{"default_illegal_slash", "hello/world", SanitizeOptions{}, "hello_world"},
		{"default_illegal_star", "file*name", SanitizeOptions{}, "file_name"},
		{"default_illegal_colon", "C:file", SanitizeOptions{}, "C_file"},
		{"custom illegal char replacement", "hello/world", SanitizeOptions{ReplaceIllegalWith: "!"}, "hello!world"},
		{"custom space replacement", "hello world", SanitizeOptions{ReplaceSpacesWith: "-"}, "hello-world"},
		{"win device replacement", "CON", SanitizeOptions{}, "_CON"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeCore(tt.input, tt.opts)
			if got != tt.expected {
				t.Errorf("sanitizeCore(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestStripDiacriticsDirect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"accented e", "éèêë", "eeee"},
		{"accented o", "öòóôõ", "ooooo"},
		{"accented a", "àáâãäå", "aaaaaa"},
		{"cilla/tilde", "çñ", "cn"},
		{"non-accented text", "hello 123", "hello 123"},
		{"empty string", "", ""},
		{"emoji and non-latin", "🚀漢", "🚀漢"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stripDiacritics(tt.input)
			if got != tt.expected {
				t.Errorf("stripDiacritics(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}

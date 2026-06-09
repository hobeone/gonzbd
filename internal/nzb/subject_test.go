package nzb

import (
	"strings"
	"testing"
)

func TestExtractFilenameFromSubject(t *testing.T) {
	tests := []struct {
		name     string
		subject  string
		expected string
	}{
		{"quoted", `[1/1] - "test.file.rar" yEnc`, "test.file.rar"},
		{"basic", `some.file.rar (1/10)`, "some.file.rar"},
		{"complex", `[#something] "another.file.mkv" [2/5]`, "another.file.mkv"},
		{"no match", `just some text without extension`, "just some text without extension"},
		{"brackets", `file_name [with brackets].mp4`, "file_name [with brackets].mp4"},
		{"unicode", `测试文件.rar (1/10)`, "测试文件.rar"},

		// --- Edge cases for the H.264 codec suffix ---
		// Without a real extension, the basic regex treats .264 as the
		// extension and drops the release group (-LAZY, -playWEB, etc.).
		{"h264_quoted_full",
			`[1/50] - "Show.Name.S01E01.1080p.WEB-DL.DD.2.0.H.264-LAZY.rar" yEnc (1/131)`,
			"Show.Name.S01E01.1080p.WEB-DL.DD.2.0.H.264-LAZY.rar"},
		{"h264_unquoted_no_ext",
			`Show.Name.S01E01.1080p.WEB-DL.DD.2.0.H.264-LAZY [01/50] - "" yEnc (1/711)`,
			"Show.Name.S01E01.1080p.WEB-DL.DD.2.0.H.264"},

		// --- Ampersand in filename (common from XML-decoded &amp;amp;amp;) ---
		// The quoted regex captures the full name including & and ;.
		{"ampersand_quoted",
			`[1/21] - "Kamp.Koral.S02E09.Mascot.Mayhem&amp;Putt.Up.1080p.H.264-playWEB.part01.rar" yEnc (1/131)`,
			"Kamp.Koral.S02E09.Mascot.Mayhem&amp;Putt.Up.1080p.H.264-playWEB.part01.rar"},
		// Empty quoted name with & in the release name: falls through to
		// the basic regex, which stops at & (not in the char class).
		{"ampersand_unquoted",
			`Kamp.Koral.S02E09.Mascot.Mayhem&amp;Putt.Up.1080p.H.264-playWEB [1/21] - "" yEnc (1/131)`,
			"Kamp.Koral.S02E09.Mascot.Mayh"},

		// --- Other special characters not in the basic regex char class ---
		{"semicolon_quoted",
			`[1/1] - "file;name.rar" yEnc (1/1)`,
			"file;name.rar"},
		{"hash_quoted",
			`[1/1] - "file#1.rar" yEnc (1/1)`,
			"file#1.rar"},
		{"at_sign_quoted",
			`[1/1] - "user@host.tar" yEnc (1/1)`,
			"user@host.tar"},
		{"exclamation_unquoted",
			`bang!file.zip (1/1)`,
			"file.zip"},
		{"first_obfuscated_second_real",
			`"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4.rar" "real_name.rar"`,
			"real_name.rar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractFilenameFromSubject(tt.subject)
			if got != tt.expected {
				t.Errorf("ExtractFilenameFromSubject(%q) = %q; want %q", tt.subject, got, tt.expected)
			}
		})
	}
}

func TestParsePRiVATESubject_Standard(t *testing.T) {
	tests := []struct {
		name     string
		subject  string
		expected string
	}{
		{"example 1", `[PRiVATE]-[WtFnZb]-[Some.Show.S01E05.720p]-[02/34] - "" yEnc (02/34)`, "Some.Show.S01E05.720p"},
		{"example 2", `[PRiVATE]-[WtFnZb]-[movie.name.2024.1080p.BluRay]-[01/75] - "" yEnc (01/75)`, "movie.name.2024.1080p.BluRay"},
		{"exactly 3 groups", `[PRiVATE]-[group]-[filename]`, "filename"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePRiVATESubject(tt.subject)
			if got != tt.expected {
				t.Errorf("parsePRiVATESubject(%q) = %q; want %q", tt.subject, got, tt.expected)
			}
		})
	}
}

func TestParsePRiVATESubject_CaseInsensitive(t *testing.T) {
	subject := `[private]-[group]-[Real.Name]-[01/10]`
	expected := "Real.Name"
	got := parsePRiVATESubject(subject)
	if got != expected {
		t.Errorf("parsePRiVATESubject(%q) = %q; want %q", subject, got, expected)
	}
}

func TestParsePRiVATESubject_NotPRiVATE(t *testing.T) {
	subject := `[PUBLIC]-[group]-[Real.Name]-[01/10]`
	expected := ""
	got := parsePRiVATESubject(subject)
	if got != expected {
		t.Errorf("parsePRiVATESubject(%q) = %q; want %q", subject, got, expected)
	}
}

func TestParsePRiVATESubject_HexReject(t *testing.T) {
	subject := `[PRiVATE]-[grp]-[a1b2c3d4e5f6a1b2c3d4e5f6]-[01/10]`
	expected := ""
	got := parsePRiVATESubject(subject)
	if got != expected {
		t.Errorf("parsePRiVATESubject(%q) = %q; want %q", subject, got, expected)
	}
}

func TestExtractFilename_PRiVATE_Integration(t *testing.T) {
	subject := `[PRiVATE]-[WtFnZb]-[Some.Show.S01E05.720p]-[02/34] - "" yEnc (02/34)`
	expected := "Some.Show.S01E05.720p"
	got := ExtractFilenameFromSubject(subject)
	if got != expected {
		t.Errorf("ExtractFilenameFromSubject(%q) = %q; want %q", subject, got, expected)
	}
}

func TestIsExcessivelyObfuscated(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		expected bool
	}{
		{"normal", "Some.Show.S01E05.720p.mkv", false},
		{"hex hash with ext", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4.rar", true},
		{"short hex with ext", "a1b2.rar", false},                           // Too short
		{"hex hash without ext", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4", false}, // Handled by pure hex guard in PRiVATE parser instead
		{"uppercase hex", "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4.MKV", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isExcessivelyObfuscated(tt.filename)
			if got != tt.expected {
				t.Errorf("isExcessivelyObfuscated(%q) = %v; want %v", tt.filename, got, tt.expected)
			}
		})
	}
}

// TestExtractFilenameIdempotency verifies that running extraction on an
// already-extracted filename produces the same result for common filenames.
// This is the regression test for the pipeline bug: pipeline.registerFile
// previously called ExtractFilenameFromSubject a second time on the Subject
// field (which already contains the extracted filename).
func TestExtractFilenameIdempotency(t *testing.T) {
	// These filenames should survive a second extraction unchanged.
	idempotent := []struct {
		name     string
		filename string
	}{
		{"simple_rar", "show.name.s01e01.part01.rar"},
		{"simple_par2", "show.name.s01e01.par2"},
		{"h264_with_ext", "Show.S01E01.1080p.WEB-DL.DD.2.0.H.264-LAZY.rar"},
		{"h264_no_ext", "Show.S01E01.1080p.WEB-DL.DD.2.0.H.264"},
		{"dots_only", "a.b.c.d.e.f.g.h.rar"},
		{"single_dot_ext", "filename.r"},
	}

	for _, tt := range idempotent {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractFilenameFromSubject(tt.filename)
			if got != tt.filename {
				t.Errorf("ExtractFilenameFromSubject(%q) = %q; want unchanged (idempotent)",
					tt.filename, got)
			}
		})
	}
}

// TestExtractFilename_NonIdempotentSpecialChars documents that
// ExtractFilenameFromSubject is NOT idempotent for filenames containing
// characters outside the basic-filename regex's character class (&, ;, #).
// This is why the pipeline must never call it a second time on the Subject
// field — doing so truncates or corrupts the filename.
func TestExtractFilename_NonIdempotentSpecialChars(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string // what the regex actually produces (the corrupted result)
	}{
		// & breaks the match; regex backtracks to ".Mayh" as extension
		{"ampersand_truncates",
			"Kamp.Koral.S02E09.Mascot.Mayhem&amp;Putt.Up.1080p.H.264-playWEB.part01.rar",
			"Kamp.Koral.S02E09.Mascot.Mayh"},
		// & stops the match early, losing everything after it
		{"ampersand_bare",
			"Show.Name&More.Name.S01.rar",
			"Show.Name"},
		// ; breaks the match — regex finds "semicolons.mkv" after the ;
		{"semicolons",
			"file;with;semicolons.mkv",
			"semicolons.mkv"},
		// # breaks the match — regex matches "42.zip" after the #
		{"hash_sign",
			"release#42.zip",
			"42.zip"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractFilenameFromSubject(tt.input)
			if got != tt.expected {
				t.Errorf("ExtractFilenameFromSubject(%q) = %q; want %q",
					tt.input, got, tt.expected)
			}
			// Also verify this is indeed different from the input
			if got == tt.input {
				t.Errorf("expected non-idempotent result but got input unchanged")
			}
		})
	}
}

// TestParseSubjectWithAmpersand is an end-to-end test verifying that the XML
// parser correctly decodes triple-encoded ampersands (&amp;amp;amp;) in NZB
// subject attributes, and that the resulting File.Subject preserves the full
// filename including the extension.
func TestParseSubjectWithAmpersand(t *testing.T) {
	// Simulate a real NZB file with &amp;amp;amp; in the subject attribute.
	// The XML parser decodes one level: &amp;amp;amp; → &amp;amp;
	// The ExtractFilenameFromSubject regex then extracts the quoted filename,
	// which contains the decoded &amp;amp; characters.
	nzbXML := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
<file poster="test@example.com" date="1700000000"
      subject="[1/21] - &quot;Kamp.Koral.S02E09.Mascot.Mayhem&amp;amp;amp;Putt.Up.1080p.H.264-playWEB.part01.rar&quot; yEnc (1/131)">
  <groups><group>alt.binaries.test</group></groups>
  <segments>
    <segment bytes="750000" number="1">article1@test</segment>
  </segments>
</file>
</nzb>`

	parsed, err := Parse(strings.NewReader(nzbXML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(parsed.Files))
	}

	subject := parsed.Files[0].Subject

	// The Subject should contain the full filename with the .rar extension
	// preserved. The &amp;amp; is decoded to &amp; by the XML parser, and
	// the extracted filename should include it.
	if !strings.HasSuffix(subject, ".rar") {
		t.Errorf("Subject %q does not end with .rar — extension was truncated", subject)
	}
	if !strings.Contains(subject, "Mayhem") {
		t.Errorf("Subject %q is missing 'Mayhem' — filename was truncated", subject)
	}
	if !strings.Contains(subject, "playWEB") {
		t.Errorf("Subject %q is missing 'playWEB' release group", subject)
	}

	t.Logf("Parsed Subject: %q (%d bytes)", subject, len(subject))
}

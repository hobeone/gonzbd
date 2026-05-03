package nzb

import "testing"

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

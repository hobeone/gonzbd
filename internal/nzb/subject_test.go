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

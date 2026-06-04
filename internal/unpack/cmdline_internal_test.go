package unpack

import "testing"

func TestFormatCmdLineDirect(t *testing.T) {
	tests := []struct {
		name     string
		bin      string
		args     []string
		redact   string
		expected string
	}{
		{
			"no redaction",
			"/usr/bin/unrar",
			[]string{"x", "-y", "/tmp/file.rar"},
			"",
			"/usr/bin/unrar x -y /tmp/file.rar",
		},
		{
			"redact password argument",
			"/usr/bin/unrar",
			[]string{"x", "-y", "-psecret", "/tmp/file.rar"},
			"-psecret",
			"/usr/bin/unrar x -y -p<redacted> /tmp/file.rar",
		},
		{
			"redact only when matching exact redact string",
			"/usr/bin/unrar",
			[]string{"x", "-y", "-psecret", "/tmp/file.rar"},
			"-pother",
			"/usr/bin/unrar x -y -psecret /tmp/file.rar",
		},
		{
			"do not redact non-p prefix arguments",
			"/usr/bin/unrar",
			[]string{"x", "-y", "secret", "/tmp/file.rar"},
			"secret",
			"/usr/bin/unrar x -y secret /tmp/file.rar",
		},
		{
			"do not redact exactly -p-",
			"/usr/bin/unrar",
			[]string{"x", "-y", "-p-", "/tmp/file.rar"},
			"-p-",
			"/usr/bin/unrar x -y -p- /tmp/file.rar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatCmdLine(tt.bin, tt.args, tt.redact)
			if got != tt.expected {
				t.Errorf("formatCmdLine() = %q; want %q", got, tt.expected)
			}
		})
	}
}

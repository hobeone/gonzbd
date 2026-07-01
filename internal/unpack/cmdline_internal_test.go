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

func TestFormatArgsDirect(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		redact   string
		expected []string
	}{
		{
			"nil args",
			nil,
			"-psecret",
			nil,
		},
		{
			"empty args",
			[]string{},
			"-psecret",
			[]string{},
		},
		{
			"no redaction needed",
			[]string{"x", "-y", "/tmp/file.rar"},
			"",
			[]string{"x", "-y", "/tmp/file.rar"},
		},
		{
			"redact single password flag",
			[]string{"x", "-y", "-psecret", "/tmp/file.rar"},
			"-psecret",
			[]string{"x", "-y", "-p<redacted>", "/tmp/file.rar"},
		},
		{
			"redact multiple password flags",
			[]string{"x", "-psecret", "-y", "-psecret", "/tmp/file.rar"},
			"-psecret",
			[]string{"x", "-p<redacted>", "-y", "-p<redacted>", "/tmp/file.rar"},
		},
		{
			"redact only exact matching redact flag",
			[]string{"x", "-y", "-psecret", "/tmp/file.rar"},
			"-pother",
			[]string{"x", "-y", "-psecret", "/tmp/file.rar"},
		},
		{
			"do not redact non-p prefix argument",
			[]string{"x", "-y", "secret", "/tmp/file.rar"},
			"secret",
			[]string{"x", "-y", "secret", "/tmp/file.rar"},
		},
		{
			"do not redact exactly -p-",
			[]string{"x", "-y", "-p-", "/tmp/file.rar"},
			"-p-",
			[]string{"x", "-y", "-p-", "/tmp/file.rar"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatArgs(tt.args, tt.redact)
			if tt.expected == nil {
				if got != nil {
					t.Errorf("formatArgs() = %v; want nil", got)
				}
				return
			}
			if len(got) != len(tt.expected) {
				t.Fatalf("formatArgs() len = %d; want %d (%v vs %v)", len(got), len(tt.expected), got, tt.expected)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("formatArgs()[%d] = %q; want %q", i, got[i], tt.expected[i])
				}
			}
			// Verify immutability / copy behavior
			if len(tt.args) > 0 && len(got) > 0 {
				orig := got[0]
				got[0] = "MUTATED"
				if tt.args[0] == "MUTATED" && orig != "MUTATED" {
					t.Errorf("formatArgs() modified original input slice")
				}
			}
		})
	}
}

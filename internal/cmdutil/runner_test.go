package cmdutil

import (
	"context"
	"testing"
)

func TestBuildCommand_NoWrapping(t *testing.T) {
	cmd := BuildCommand(context.Background(), CmdConfig{}, "unrar", "x", "-y", "file.rar")
	if cmd.Path == "" {
		t.Fatal("expected non-empty path")
	}
	// With no wrapping, args should be the direct command.
	args := cmd.Args
	if len(args) < 4 {
		t.Fatalf("expected at least 4 args, got %d: %v", len(args), args)
	}
	// Args[0] is the resolved path, but subsequent args should match.
	if args[1] != "x" || args[2] != "-y" || args[3] != "file.rar" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestBuildCommand_NiceOnly(t *testing.T) {
	cfg := CmdConfig{Nice: "-n 15"}
	cmd := BuildCommand(context.Background(), cfg, "unrar", "x")
	args := cmd.Args
	// Expected: nice -n 15 unrar x
	want := []string{"nice", "-n", "15", "unrar", "x"}
	if len(args) != len(want) {
		t.Fatalf("got %d args %v, want %d args %v", len(args), args, len(want), want)
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, args[i], w)
		}
	}
}

func TestBuildCommand_IonicOnly(t *testing.T) {
	cfg := CmdConfig{Ionice: "-c2 -n4"}
	cmd := BuildCommand(context.Background(), cfg, "par2", "r")
	args := cmd.Args
	// Expected: ionice -c2 -n4 par2 r
	want := []string{"ionice", "-c2", "-n4", "par2", "r"}
	if len(args) != len(want) {
		t.Fatalf("got %d args %v, want %d args %v", len(args), args, len(want), want)
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, args[i], w)
		}
	}
}

func TestBuildCommand_BothNiceAndIonice(t *testing.T) {
	cfg := CmdConfig{Nice: "-n 15", Ionice: "-c2 -n4"}
	cmd := BuildCommand(context.Background(), cfg, "7zz", "x", "-y", "archive.7z")
	args := cmd.Args
	// Expected: nice -n 15 ionice -c2 -n4 7zz x -y archive.7z
	want := []string{"nice", "-n", "15", "ionice", "-c2", "-n4", "7zz", "x", "-y", "archive.7z"}
	if len(args) != len(want) {
		t.Fatalf("got %d args %v, want %d args %v", len(args), args, len(want), want)
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, args[i], w)
		}
	}
}

func TestFormatCmdPrefix(t *testing.T) {
	tests := []struct {
		name string
		cfg  CmdConfig
		want string
	}{
		{"empty", CmdConfig{}, ""},
		{"nice only", CmdConfig{Nice: "-n 15"}, "nice -n 15"},
		{"ionice only", CmdConfig{Ionice: "-c2 -n4"}, "ionice -c2 -n4"},
		{"both", CmdConfig{Nice: "-n 15", Ionice: "-c2 -n4"}, "nice -n 15 ionice -c2 -n4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatCmdPrefix(tt.cfg)
			if got != tt.want {
				t.Errorf("FormatCmdPrefix() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidatePriorityArgs(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		args    string
		wantErr bool
	}{
		{"empty", "nice", "", false},
		{"valid nice", "nice", "-n 15", false},
		{"valid ionice", "ionice", "-c2 -n4", false},
		{"uppercase characters", "nice", "NICE -N 15", false},
		{"tab characters", "nice", "-n\t15", false},
		{"boundary characters", "nice", "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789- \t", false},
		{"semicolon injection", "nice", "-n 15; rm -rf /", true},
		{"pipe injection", "nice", "-n 15 | cat", true},
		{"backtick injection", "nice", "-n `whoami`", true},
		{"dollar injection", "nice", "-n $HOME", true},
		{"ampersand", "nice", "-n 15 &", true},
		{"parentheses", "nice", "(-n 15)", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePriorityArgs(tt.kind, tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePriorityArgs(%q, %q) error = %v, wantErr %v", tt.kind, tt.args, err, tt.wantErr)
			}
		})
	}
}

func TestParseExtraParams(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		want    []string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"only spaces", "   ", nil, false},
		{"valid single", "-v", []string{"-v"}, false},
		{"valid multiple", "-v -d --safe", []string{"-v", "-d", "--safe"}, false},
		{"invalid prefix", "v", nil, true},
		{"mixed invalid", "-v d", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseExtraParams(tt.params)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseExtraParams(%q) error = %v, wantErr %v", tt.params, err, tt.wantErr)
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Fatalf("got %d args, want %d", len(got), len(tt.want))
				}
				for i := range got {
					if got[i] != tt.want[i] {
						t.Errorf("got[%d] = %q, want %q", i, got[i], tt.want[i])
					}
				}
			}
		})
	}
}

func TestCmdConfigIsEmpty(t *testing.T) {
	if !(CmdConfig{}).IsEmpty() {
		t.Error("zero CmdConfig should be empty")
	}
	if (CmdConfig{Nice: "-n 15"}).IsEmpty() {
		t.Error("CmdConfig with Nice should not be empty")
	}
	if (CmdConfig{Ionice: "-c2"}).IsEmpty() {
		t.Error("CmdConfig with Ionice should not be empty")
	}
}

func TestValidateUnrarParams(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"nil", nil, false},
		{"empty", []string{}, false},
		{"mlp allowed", []string{"-mlp"}, false},
		{"om- allowed", []string{"-om-"}, false},
		{"ri allowed", []string{"-ri10:5"}, false},
		{"multiple allowed", []string{"-mlp", "-om-", "-ri5"}, false},
		{"df disallowed", []string{"-df"}, true},
		{"y disallowed", []string{"-y"}, true},
		{"mixed", []string{"-mlp", "-df"}, true},
		{"scf disallowed", []string{"-scf"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateUnrarParams(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateUnrarParams(%v) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
		})
	}
}

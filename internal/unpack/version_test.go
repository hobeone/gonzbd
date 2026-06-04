package unpack

import (
	"os"
	"testing"
)

func TestParseUnrarOutput(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		wantVersion int
		wantStr     string
		wantProblem bool
	}{
		{
			name:        "v7.21 freeware",
			output:      "UNRAR 7.21 freeware      Copyright (c) 1993-2026 Alexander Roshal",
			wantVersion: 721,
			wantStr:     "7.21",
			wantProblem: false,
		},
		{
			name:        "v5.50 (minimum supported)",
			output:      "UNRAR 5.50 freeware      Copyright (c) 1993-2017 Alexander L. Roshal",
			wantVersion: 550,
			wantStr:     "5.50",
			wantProblem: false,
		},
		{
			name:        "v7.10 x64",
			output:      "UNRAR 7.10 x64   Copyright (c) 1993-2024 Alexander L. Roshal",
			wantVersion: 710,
			wantStr:     "7.10",
			wantProblem: false,
		},
		{
			name:        "too old (< 5.50)",
			output:      "UNRAR 5.21 freeware      Copyright (c) 1993-2015 Alexander L. Roshal",
			wantVersion: 521,
			wantStr:     "5.21",
			wantProblem: true,
		},
		{
			name:        "fork (unar) — no version line",
			output:      "The Unarchiver 4.0.0 (Mar 23 2019)",
			wantVersion: 0,
			wantStr:     "",
			wantProblem: true,
		},
		{
			name:        "empty output",
			output:      "",
			wantVersion: 0,
			wantStr:     "",
			wantProblem: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseUnrarOutput(tc.output)
			if got.Version != tc.wantVersion {
				t.Errorf("Version = %d, want %d", got.Version, tc.wantVersion)
			}
			if got.VersionStr != tc.wantStr {
				t.Errorf("VersionStr = %q, want %q", got.VersionStr, tc.wantStr)
			}
			if got.HasProblem != tc.wantProblem {
				t.Errorf("HasProblem = %v, want %v", got.HasProblem, tc.wantProblem)
			}
			if !got.Available {
				t.Error("Available should always be true from parseUnrarOutput")
			}
		})
	}
}

func init() {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		mode := os.Getenv("HELPER_MODE")
		switch mode {
		case "unrar":
			_, _ = os.Stdout.WriteString("UNRAR 7.21 freeware\n")
			os.Exit(0)
		case "sevenzip":
			_, _ = os.Stdout.WriteString("7-Zip (z) 21.06 x64\n")
			os.Exit(0)
		default:
			os.Exit(1)
		}
	}
}

func TestDetectUnrar(t *testing.T) {
	t.Run("nonexistent binary", func(t *testing.T) {
		got := DetectUnrar(t.Context(), "/path/to/nonexistent")
		if got.Available {
			t.Error("expected Available to be false for nonexistent binary")
		}
	})

	t.Run("valid mock binary", func(t *testing.T) {
		bin := os.Args[0]
		os.Setenv("GO_WANT_HELPER_PROCESS", "1")
		os.Setenv("HELPER_MODE", "unrar")
		defer func() {
			os.Unsetenv("GO_WANT_HELPER_PROCESS")
			os.Unsetenv("HELPER_MODE")
		}()

		got := DetectUnrar(t.Context(), bin)
		if !got.Available {
			t.Error("expected Available to be true")
		}
		if got.Version != 721 || got.VersionStr != "7.21" {
			t.Errorf("expected version 721 (7.21), got version=%d (%s)", got.Version, got.VersionStr)
		}
		if got.HasProblem {
			t.Error("expected HasProblem to be false for version 7.21")
		}
	})
}

func TestDetectSevenZip(t *testing.T) {
	t.Run("nonexistent binary", func(t *testing.T) {
		got := DetectSevenZip(t.Context(), "/path/to/nonexistent")
		if got.Available {
			t.Error("expected Available to be false for nonexistent binary")
		}
	})

	t.Run("valid mock binary", func(t *testing.T) {
		bin := os.Args[0]
		os.Setenv("GO_WANT_HELPER_PROCESS", "1")
		os.Setenv("HELPER_MODE", "sevenzip")
		defer func() {
			os.Unsetenv("GO_WANT_HELPER_PROCESS")
			os.Unsetenv("HELPER_MODE")
		}()

		got := DetectSevenZip(t.Context(), bin)
		if !got.Available {
			t.Error("expected Available to be true")
		}
		if got.Version != "21.06" {
			t.Errorf("expected version 21.06, got %q", got.Version)
		}
	})
}

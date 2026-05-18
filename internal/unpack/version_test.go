package unpack

import "testing"

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

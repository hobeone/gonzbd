package config

import (
	"testing"
)

func TestFindCategory(t *testing.T) {
	cats := []CategoryConfig{
		{Name: "*", PP: 1, Script: "star.sh", Priority: -1, Dir: "star"},
		{Name: "Default", PP: 3, Script: "default.sh", Priority: 0, Dir: ""},
		{Name: "tv", PP: 7, Script: "tv.sh", Priority: 2, Dir: "TV"},
		{Name: "movies", PP: 3, Script: "movies.sh", Priority: 1, Dir: "Movies"},
	}

	tests := []struct {
		name     string
		lookup   string
		wantName string
		wantPP   int
		wantDir  string
	}{
		{"exact match", "tv", "tv", 7, "TV"},
		{"exact match movies", "movies", "movies", 3, "Movies"},
		{"fallback to Default", "unknown", "Default", 3, ""},
		{"empty name falls back to Default", "", "Default", 3, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindCategory(cats, tt.lookup)
			if got.Name != tt.wantName {
				t.Errorf("FindCategory(%q).Name = %q, want %q", tt.lookup, got.Name, tt.wantName)
			}
			if got.PP != tt.wantPP {
				t.Errorf("FindCategory(%q).PP = %d, want %d", tt.lookup, got.PP, tt.wantPP)
			}
			if got.Dir != tt.wantDir {
				t.Errorf("FindCategory(%q).Dir = %q, want %q", tt.lookup, got.Dir, tt.wantDir)
			}
		})
	}
}

func TestFindCategoryFallbackToStar(t *testing.T) {
	// No "Default" category — should fall back to "*".
	cats := []CategoryConfig{
		{Name: "*", PP: 1, Dir: "star"},
		{Name: "tv", PP: 7, Dir: "TV"},
	}
	got := FindCategory(cats, "unknown")
	if got.Name != "*" {
		t.Errorf("FindCategory(unknown) with no Default: got %q, want *", got.Name)
	}
}

func TestFindCategoryEmpty(t *testing.T) {
	// No categories at all — returns zero value.
	got := FindCategory(nil, "anything")
	if got.Name != "" || got.PP != 0 {
		t.Errorf("FindCategory(nil) = %+v, want zero", got)
	}
}

// TestFindCategoryCaseInsensitive verifies that category lookup is
// case-insensitive for both explicit names and the "Default" fallback.
// Real configs (especially those mutated through the web UI) commonly
// store the fallback as "default" rather than "Default" — a case-
// sensitive match would fall through to the zero CategoryConfig and
// silently apply PP=0, disabling post-processing.
func TestFindCategoryCaseInsensitive(t *testing.T) {
	tests := []struct {
		name     string
		cats     []CategoryConfig
		lookup   string
		wantName string
		wantPP   int
	}{
		{
			name: "lowercase default matches as fallback",
			cats: []CategoryConfig{
				{Name: "default", PP: 7},
				{Name: "tv", PP: 7},
			},
			lookup:   "",
			wantName: "default",
			wantPP:   7,
		},
		{
			name: "DEFAULT (upper) matches as fallback",
			cats: []CategoryConfig{
				{Name: "DEFAULT", PP: 5},
			},
			lookup:   "",
			wantName: "DEFAULT",
			wantPP:   5,
		},
		{
			name: "explicit-name lookup is case-insensitive",
			cats: []CategoryConfig{
				{Name: "TV", PP: 7},
				{Name: "Default", PP: 3},
			},
			lookup:   "tv",
			wantName: "TV",
			wantPP:   7,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindCategory(tt.cats, tt.lookup)
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if got.PP != tt.wantPP {
				t.Errorf("PP = %d, want %d", got.PP, tt.wantPP)
			}
		})
	}
}

package i18n

import (
	"testing"
)

// TestCatalog_Lookup tests the Lookup method behavior.
func TestCatalog_Lookup(t *testing.T) {
	tests := []struct {
		name    string
		catalog Catalog
		key     string
		want    string
	}{
		{
			name:    "Lookup with hit returns translated value",
			catalog: Catalog{"menu-queue": "Queue"},
			key:     "menu-queue",
			want:    "Queue",
		},
		{
			name:    "Lookup with miss returns key verbatim",
			catalog: Catalog{"other": "Other"},
			key:     "menu-queue",
			want:    "menu-queue",
		},
		{
			name:    "Lookup on empty catalog returns key",
			catalog: Catalog{},
			key:     "menu-queue",
			want:    "menu-queue",
		},
		{
			name:    "Lookup on nil catalog returns key",
			catalog: nil,
			key:     "menu-queue",
			want:    "menu-queue",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.catalog.Lookup(tt.key)
			if got != tt.want {
				t.Errorf("Lookup(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

// TestNew verifies New returns a non-nil empty Catalog.
func TestNew(t *testing.T) {
	cat := New()
	if cat == nil {
		t.Fatal("New() returned nil, want non-nil empty Catalog")
	}
	// Test that the empty catalog's Lookup works correctly
	got := cat.Lookup("any-key")
	if got != "any-key" {
		t.Errorf("New().Lookup(%q) = %q, want %q", "any-key", got, "any-key")
	}
}

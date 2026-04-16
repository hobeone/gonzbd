package i18n

import "testing"

// TestDefaultEnglish_HasKnownKeys verifies the default English catalog
// contains a representative sample of keys used across the Glitter templates
// (menu entries, Glitter-prefixed modal labels, post-status strings).
// The values come from upstream sabnzbd/sabnzbd/skintext.py SKIN_TEXT.
func TestDefaultEnglish_HasKnownKeys(t *testing.T) {
	cat := DefaultEnglish()
	if cat == nil {
		t.Fatal("DefaultEnglish() returned nil")
	}

	tests := []struct {
		key  string
		want string
	}{
		{"menu-queue", "Queue"},
		{"menu-history", "History"},
		{"menu-config", "Config"},
		{"Glitter-fetch", "Fetch"},
		{"Glitter-addNZB", "Add NZB"},
		{"post-Paused", "Paused"},
		{"post-Completed", "Completed"},
		{"confirm", "Are you sure?"},
	}
	for _, tt := range tests {
		if got := cat.Lookup(tt.key); got != tt.want {
			t.Errorf("DefaultEnglish().Lookup(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

// TestDefaultEnglish_SizeFloor guards against regressions that would drop
// entries during a future re-port. Upstream SKIN_TEXT has 756 entries; we
// allow a small slack but require the bulk to be present.
func TestDefaultEnglish_SizeFloor(t *testing.T) {
	cat := DefaultEnglish()
	if n := len(cat); n < 700 {
		t.Errorf("DefaultEnglish() has %d entries, want at least 700 (upstream SKIN_TEXT=756)", n)
	}
}

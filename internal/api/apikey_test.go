package api

import (
	"testing"
)

func TestGenerateAPIKey(t *testing.T) {
	t.Parallel()

	key, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	// Verify length
	if len(key) != 16 {
		t.Errorf("key length = %d; want 16", len(key))
	}

	// Verify format (lowercase hex)
	if !ValidateAPIKey(key) {
		t.Errorf("generated key failed validation: %s", key)
	}
}

func TestGenerateAPIKeyUniqueness(t *testing.T) {
	t.Parallel()

	keys := make(map[string]bool)
	for i := range 1000 {
		key, err := GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey iteration %d: %v", i, err)
		}
		if keys[key] {
			t.Errorf("duplicate key generated: %s", key)
		}
		keys[key] = true
	}

	if len(keys) != 1000 {
		t.Errorf("generated %d unique keys; want 1000", len(keys))
	}
}

func TestValidateAPIKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want bool
	}{
		{
			name: "valid lowercase hex",
			key:  "0123456789abcdef",
			want: true,
		},
		{
			name: "valid all zeros",
			key:  "0000000000000000",
			want: true,
		},
		{
			name: "valid all f",
			key:  "ffffffffffffffff",
			want: true,
		},
		{
			name: "empty string",
			key:  "",
			want: false,
		},
		{
			name: "too short",
			key:  "0123456789abcde",
			want: false,
		},
		{
			name: "too long",
			key:  "0123456789abcdef0",
			want: false,
		},
		{
			name: "uppercase hex",
			key:  "0123456789ABCDEF",
			want: false,
		},
		{
			name: "mixed case",
			key:  "0123456789AbCdEf",
			want: false,
		},
		{
			name: "invalid hex char (g)",
			key:  "0123456789abcdeg",
			want: false,
		},
		{
			name: "invalid hex char (space)",
			key:  "0123456789abcde ",
			want: false,
		},
		{
			name: "invalid hex char (dash)",
			key:  "0123456789abcd-f",
			want: false,
		},
		{
			name: "non-hex characters",
			key:  "not_hex_key_here",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateAPIKey(tt.key)
			if got != tt.want {
				t.Errorf("ValidateAPIKey(%q) = %v; want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestGenerateAndValidate(t *testing.T) {
	t.Parallel()

	// Generate a key and verify it passes validation
	key, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	if !ValidateAPIKey(key) {
		t.Errorf("generated key failed validation: %s", key)
	}
}

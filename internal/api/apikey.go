package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// GenerateAPIKey generates a 16-character lowercase hexadecimal API key.
// It uses 8 random bytes from crypto/rand, encoded as hex to produce 16 characters.
// This format matches the Python SABnzbd API key format.
func GenerateAPIKey() (string, error) {
	// Generate 8 random bytes (16 hex chars when encoded)
	randBytes := make([]byte, 8)
	if _, err := rand.Read(randBytes); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}
	return hex.EncodeToString(randBytes), nil
}

// ValidateAPIKey returns true if key is a valid API key format:
// exactly 16 lowercase hexadecimal characters.
func ValidateAPIKey(key string) bool {
	if len(key) != 16 {
		return false
	}
	// Check that all characters are lowercase hex
	for _, c := range key {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Package i18n provides translation/localization support for SABnzbd-Go.
// The catalog is a simple key-value map that returns the key itself when
// a translation is not found (English fallback).
package i18n

// Catalog holds translation key-value pairs.
type Catalog map[string]string

// Lookup returns the translated value for a key, or the key itself if not found.
// This provides English-fallback behavior: missing translations render as the key.
func (c Catalog) Lookup(key string) string {
	if c == nil {
		return key
	}
	if val, ok := c[key]; ok {
		return val
	}
	return key
}

// New returns a non-nil empty Catalog so callers can Lookup without nil-checks.
func New() Catalog {
	return Catalog{}
}

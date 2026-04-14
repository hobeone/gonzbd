// Package config defines the on-disk configuration model and load/save
// machinery for the sabnzbd-go daemon.
//
// # Format
//
// Configuration is YAML, deserialized via gopkg.in/yaml.v3. There is no
// compatibility with Python sabnzbd's INI format; this is a ground-up
// rewrite. The schema mirrors the user-facing keys documented in spec §9.
//
// # Loading and validation
//
// Load returns an error on any validation failure (missing required field,
// out-of-range numeric, duplicate name within a list, malformed type).
// Silent fallback to defaults is intentionally not provided — the project's
// "no silent error swallowing" rule applies to user-supplied data too.
//
// # Concurrency and change notification
//
// Top-level Config exposes a sync.RWMutex for callers that mutate fields at
// runtime (e.g. via the API). Per-field change notifications follow a
// callback model: subscribers register a handler against a specific field
// and the setter invokes it after releasing the lock. The callback wiring
// itself is added as soon as the first subscriber appears (e.g. when the
// downloader needs to react to bandwidth changes); shipping it before any
// subscriber exists would be dead code.
//
// # Atomic save
//
// Save writes to a sibling temp file, fsyncs, then renames over the target
// path. Callers always observe either the previous full file or the new
// full file, never a half-written one.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hobeone/sabnzbd-go/internal/constants"
)

// ByteSize is a non-negative byte count expressed in the YAML config as a
// human-readable string ("10M", "1.5G", "0", "unlimited", "") or a plain
// integer (interpreted as bytes). Internally it is stored as int64 bytes;
// 0 means "no limit" by convention for fields where that interpretation is
// meaningful (bandwidth caps, free-disk thresholds).
//
// Accepted suffixes are case-insensitive and use binary (1024-based) units:
// K, M, G, T. A bare integer is taken as bytes. Empty string and the
// literal "unlimited" both decode to 0.
type ByteSize int64

// String renders a ByteSize as a human-readable suffix form. 0 renders as
// "0" rather than "" to avoid confusion in logs.
func (b ByteSize) String() string {
	n := int64(b)
	switch {
	case n == 0:
		return "0"
	case n%constants.TiB == 0:
		return strconv.FormatInt(n/constants.TiB, 10) + "T"
	case n%constants.GiB == 0:
		return strconv.FormatInt(n/constants.GiB, 10) + "G"
	case n%constants.MiB == 0:
		return strconv.FormatInt(n/constants.MiB, 10) + "M"
	case n%constants.KiB == 0:
		return strconv.FormatInt(n/constants.KiB, 10) + "K"
	default:
		return strconv.FormatInt(n, 10)
	}
}

// MarshalYAML serializes ByteSize using its String form so YAML files stay
// human-friendly after a round-trip through the daemon.
func (b ByteSize) MarshalYAML() (any, error) {
	return b.String(), nil
}

// UnmarshalYAML parses ByteSize from a YAML scalar that is either a string
// with a binary suffix ("10M", "1.5G", "unlimited", "") or a non-negative
// integer (bytes).
func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("ByteSize: expected scalar, got kind %d at line %d", node.Kind, node.Line)
	}
	parsed, err := parseByteSize(node.Value)
	if err != nil {
		return fmt.Errorf("ByteSize at line %d: %w", node.Line, err)
	}
	*b = parsed
	return nil
}

// parseByteSize parses a single ByteSize string. Exposed (lowercase) for
// reuse by package-level helpers and tests.
func parseByteSize(raw string) (ByteSize, error) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.EqualFold(s, "unlimited") {
		return 0, nil
	}

	// Split off an optional case-insensitive suffix.
	mult := int64(1)
	last := s[len(s)-1]
	switch last {
	case 'k', 'K':
		mult = constants.KiB
		s = s[:len(s)-1]
	case 'm', 'M':
		mult = constants.MiB
		s = s[:len(s)-1]
	case 'g', 'G':
		mult = constants.GiB
		s = s[:len(s)-1]
	case 't', 'T':
		mult = constants.TiB
		s = s[:len(s)-1]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("byte size: missing numeric component in %q", raw)
	}

	// Try integer first (avoids float rounding for exact values).
	if mult == 1 {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("byte size: %q is not a valid integer", raw)
		}
		if n < 0 {
			return 0, fmt.Errorf("byte size: %q is negative", raw)
		}
		return ByteSize(n), nil
	}

	// Allow fractional values with a unit suffix ("1.5G").
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("byte size: %q is not a valid number", raw)
	}
	if f < 0 {
		return 0, fmt.Errorf("byte size: %q is negative", raw)
	}
	bytes := f * float64(mult)
	if bytes > float64(maxInt64) {
		return 0, fmt.Errorf("byte size: %q overflows int64", raw)
	}
	return ByteSize(bytes), nil
}

const maxInt64 = int64(^uint64(0) >> 1)

// Percent is an integer in the inclusive range [0, 100]. Validation happens
// at decode time so out-of-range values produce a clear load-time error
// rather than silently clamping.
type Percent int

// MarshalYAML emits the percent as a plain integer.
func (p Percent) MarshalYAML() (any, error) { return int(p), nil }

// UnmarshalYAML parses an integer percent and rejects out-of-range values.
func (p *Percent) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("Percent: expected scalar, got kind %d at line %d", node.Kind, node.Line)
	}
	n, err := strconv.Atoi(strings.TrimSpace(node.Value))
	if err != nil {
		return fmt.Errorf("Percent at line %d: %w", node.Line, err)
	}
	if n < 0 || n > 100 {
		return fmt.Errorf("Percent at line %d: %d outside [0,100]", node.Line, n)
	}
	*p = Percent(n)
	return nil
}

// SSLVerify enumerates the certificate-verification levels permitted for an
// NNTP server connection (see spec §3.3). Stored as int8 so it fits compact
// JSON/YAML representations and survives diff-friendly serialization.
type SSLVerify int8

// SSL verification levels. Values are wire-stable; do not renumber.
const (
	// SSLVerifyNone disables certificate verification entirely. Useful for
	// testing only; never recommended in production.
	SSLVerifyNone SSLVerify = 0
	// SSLVerifyMinimal accepts any valid chain regardless of hostname.
	SSLVerifyMinimal SSLVerify = 1
	// SSLVerifyHostname requires the certificate to match the configured
	// host (this is the default).
	SSLVerifyHostname SSLVerify = 2
	// SSLVerifyStrict enables hostname verification plus extra checks
	// (e.g. revocation, when supported by the runtime).
	SSLVerifyStrict SSLVerify = 3
)

// validSSLVerify returns the canonical error reported when an SSLVerify
// value is outside the documented range.
func (s SSLVerify) validate() error {
	if s < SSLVerifyNone || s > SSLVerifyStrict {
		return fmt.Errorf("ssl_verify: %d outside [0,3]", s)
	}
	return nil
}

// errEmpty is reused by validators for "field cannot be empty" messages.
var errEmpty = errors.New("must not be empty")

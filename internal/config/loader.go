package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// Load reads, parses, and validates a YAML configuration file. It returns
// an error on any decoding or validation failure; partial / fallback
// behavior is intentionally not provided.
//
// The returned *Config has its mutex initialized; callers may immediately
// hand it to subsystems.
func Load(path string) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator-supplied
	if err != nil {
		return nil, fmt.Errorf("config: open %q: %w", path, err)
	}
	//nolint:errcheck // read-only handle: if Decode succeeded the data is already consumed, and if it failed the decode error is the actionable one.
	defer f.Close()

	cfg, err := decode(f)
	if err != nil {
		return nil, fmt.Errorf("config: decode %q: %w", path, err)
	}
	cfg.ExpandPaths()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: validate %q: %w", path, err)
	}
	return cfg, nil
}

// decode is split out so tests can decode from in-memory buffers without
// touching disk.
func decode(r io.Reader) (*Config, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	// Initialize with defaults so that missing fields in YAML stay at default.
	cfg, err := Default()
	if err != nil {
		return nil, err
	}

	// NOTE: We intentionally do NOT call os.ExpandEnv on the raw YAML.
	// Doing so would silently corrupt any value containing '$' characters
	// (passwords, API keys, regex patterns). Environment variable expansion
	// for path fields is handled post-parse by cfg.ExpandPaths().
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true) // reject unknown keys to catch typos
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			// Empty file? Just return the defaults.
			return cfg, nil
		}
		return nil, err
	}

	// Sticky defaults: if these fields are empty after decoding, it means
	// they were either missing from the YAML or explicitly set to empty.
	// We restore defaults for critical naming options to ensure the UI
	// and system work correctly for existing users.
	if cfg.Downloads.ReplaceIllegalWith == "" {
		cfg.Downloads.ReplaceIllegalWith = "_"
	}
	if len(cfg.Downloads.CleanupList) == 0 {
		cfg.Downloads.CleanupList = DefaultCleanupList
	}

	// Normalize nil slices to empty so JSON serialization produces []
	// instead of null. YAML unmarshal can overwrite Default()'s empty
	// slices with nil when the key is present but has no items.
	if cfg.Servers == nil {
		cfg.Servers = []ServerConfig{}
	}
	if cfg.Categories == nil {
		cfg.Categories = []CategoryConfig{}
	}
	if cfg.Schedules == nil {
		cfg.Schedules = []ScheduleConfig{}
	}
	if cfg.RSS == nil {
		cfg.RSS = []RSSFeedConfig{}
	}

	return cfg, nil
}

// Save writes the configuration to path atomically: the YAML is rendered
// to a sibling temp file, fsynced, and renamed over the destination.
// Readers always observe either the previous file or the new file, never
// a half-written one.
func (c *Config) Save(path string) error {
	// Snapshot the YAML bytes under RLock, then release before doing I/O.
	c.mu.RLock()
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		c.mu.RUnlock()
		return fmt.Errorf("config: encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		c.mu.RUnlock()
		return fmt.Errorf("config: close encoder: %w", err)
	}
	c.mu.RUnlock()

	// --- No lock held below this line ---

	if err := fsutil.WriteAtomicBytes(path, buf.Bytes()); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

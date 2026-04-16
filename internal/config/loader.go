package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
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

	// Expand environment variables ($VAR or ${VAR}) globally.
	expanded := os.ExpandEnv(string(b))

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true) // reject unknown keys to catch typos
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("file is empty")
		}
		return nil, err
	}
	return &cfg, nil
}

// Save writes the configuration to path atomically: the YAML is rendered
// to a sibling temp file, fsynced, and renamed over the destination.
// Readers always observe either the previous file or the new file, never
// a half-written one.
func (c *Config) Save(path string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return fmt.Errorf("config: create temp file: %w", err)
	}
	tmpName := tmp.Name()

	// Best-effort cleanup if any subsequent step fails before rename. The
	// primary error has already been captured by the caller path; a
	// secondary Close/Remove failure during cleanup adds no actionable
	// information.
	cleanup := func() {
		_ = tmp.Close()        //nolint:errcheck // best-effort cleanup on error path
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on error path
	}

	enc := yaml.NewEncoder(tmp)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		cleanup()
		return fmt.Errorf("config: encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		cleanup()
		return fmt.Errorf("config: close encoder: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("config: fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on error path
		return fmt.Errorf("config: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on error path
		return fmt.Errorf("config: rename %q -> %q: %w", tmpName, path, err)
	}
	return nil
}

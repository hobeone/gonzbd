package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// generatedKeys records the api_key and nzb_key Load generated because the
// file left them empty or out, until PersistGeneratedKeys writes them back.
type generatedKeys struct {
	path string
	raw  []byte // the file as Load read it
	api  bool
	nzb  bool
}

// generateMissingKeys fills an empty api_key or nzb_key with a new key and
// reports which it filled. decode leaves a key the file omits empty, so an
// omitted key and an empty one are both filled here.
func (c *Config) generateMissingKeys() (api, nzb bool) {
	if c.General.APIKey == "" {
		c.General.APIKey = newAPIKey()
		api = true
	}
	if c.General.NZBKey == "" {
		c.General.NZBKey = newAPIKey()
		nzb = true
	}
	return api, nzb
}

// PersistGeneratedKeys writes the api_key and nzb_key Load generated back
// into the file Load read. Only those keys' lines change: every other byte of
// the file, its comments included, is kept. It does nothing when Load
// generated no key, or after it has already written them.
//
// A key that is not written back changes on every start, so a file gonzbd
// cannot write is an error, not a warning.
func (c *Config) PersistGeneratedKeys() error {
	c.mu.RLock()
	g := c.generated
	apiKey, nzbKey := c.General.APIKey, c.General.NZBKey
	c.mu.RUnlock()

	// --- No lock held below this line ---
	if g == nil {
		return nil
	}
	var edits []keyEdit
	if g.api {
		edits = append(edits, keyEdit{"api_key", apiKey})
	}
	if g.nzb {
		edits = append(edits, keyEdit{"nzb_key", nzbKey})
	}
	out, err := setGeneralKeys(g.raw, edits)
	if err != nil {
		return fmt.Errorf("config: write generated keys into %q: %w; set api_key and nzb_key yourself (`openssl rand -hex 8`)", g.path, err)
	}
	if err := writeKeysFile(g.path, out); err != nil {
		return err
	}

	c.mu.Lock()
	c.generated = nil
	c.mu.Unlock()
	slog.Default().With("component", "config").Info("config: saved generated keys",
		"path", g.path, "api_key", g.api, "nzb_key", g.nzb)
	return nil
}

// writeKeysFile replaces the config file at path with out, keeping its
// permissions, and refuses a file the process may not write. The atomic
// rename would otherwise replace a read-only file whenever its directory is
// writable.
func writeKeysFile(path string, out []byte) error {
	// Resolved so the rename replaces the file a symlink points at, not the
	// symlink.
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("config: resolve %q: %w", path, err)
	}
	f, err := os.OpenFile(target, os.O_WRONLY, 0) //nolint:gosec // G304: operator-supplied config path
	if err != nil {
		return fmt.Errorf("config: %q is not writable, so the api_key/nzb_key generated for it cannot be saved and would change on every start; make the file writable or set the keys yourself (`openssl rand -hex 8`): %w", target, err)
	}
	st, statErr := f.Stat()
	if err := errors.Join(statErr, f.Close()); err != nil {
		return fmt.Errorf("config: stat %q: %w", target, err)
	}
	if err := fsutil.WriteAtomicBytesPerm(target, out, st.Mode().Perm()); err != nil {
		return fmt.Errorf("config: write generated keys: %w", err)
	}
	return nil
}

// keyEdit is one general-section key to write and its plain-scalar value.
type keyEdit struct {
	name, value string
}

// setGeneralKeys returns raw with each edit's key in the general section set
// to its value. A key already present has its line rewritten in place, its
// trailing comment kept; a missing one is inserted on a new line just below
// "general:", and a missing general section is appended. Every other line is
// returned unchanged. The result is decoded again and checked, so a layout
// this cannot edit (a flow-style or aliased mapping) is an error rather than
// a wrong file.
func setGeneralKeys(raw []byte, edits []keyEdit) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var genKey, gen *yaml.Node
	switch doc.Kind {
	case 0: // empty, or comments only
	case yaml.DocumentNode:
		root := doc.Content[0]
		if root.Kind != yaml.MappingNode || root.Style&yaml.FlowStyle != 0 {
			return nil, errors.New("the top level is not a block mapping")
		}
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == "general" {
				genKey, gen = root.Content[i], root.Content[i+1]
			}
		}
	default:
		return nil, errors.New("the file is not a single YAML document")
	}

	lines := bytes.SplitAfter(raw, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	indent := []byte("  ")
	var insertAt int // index in lines before which missing keys go
	switch {
	case gen == nil:
		if len(lines) > 0 && !bytes.HasSuffix(lines[len(lines)-1], []byte("\n")) {
			lines[len(lines)-1] = append(lines[len(lines)-1], '\n')
		}
		lines = append(lines, []byte("general:\n"))
		insertAt = len(lines)
	case gen.Kind == yaml.ScalarNode && gen.Tag == "!!null" && gen.Value == "":
		insertAt = genKey.Line
	case gen.Kind == yaml.MappingNode && gen.Style&yaml.FlowStyle == 0:
		insertAt = genKey.Line
		if len(gen.Content) > 0 {
			indent = bytes.Repeat([]byte(" "), gen.Content[0].Column-1)
		}
	default:
		return nil, errors.New("the general section is not a block mapping")
	}

	var inserted [][]byte
	for _, e := range edits {
		k, v := mappingEntry(gen, e.name)
		if k == nil {
			inserted = append(inserted, fmt.Appendf(nil, "%s%s: %s\n", indent, e.name, e.value))
			continue
		}
		if v.Line != k.Line || v.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("%s's value is not on its key's line", e.name)
		}
		line := lines[k.Line-1]
		eol := line[len(bytes.TrimRight(line, "\r\n")):]
		var b []byte
		b = append(b, line[:k.Column-1]...)
		b = fmt.Appendf(b, "%s: %s", e.name, e.value)
		if c := v.LineComment + k.LineComment; c != "" {
			b = append(b, "  "+c...)
		}
		lines[k.Line-1] = append(b, eol...)
	}
	lines = append(lines[:insertAt], append(inserted, lines[insertAt:]...)...)
	out := bytes.Join(lines, nil)

	var check struct {
		General GeneralConfig `yaml:"general"`
	}
	if err := yaml.Unmarshal(out, &check); err != nil {
		return nil, fmt.Errorf("the edited file does not parse: %w", err)
	}
	for _, e := range edits {
		got := check.General.APIKey
		if e.name == "nzb_key" {
			got = check.General.NZBKey
		}
		if got != e.value {
			return nil, fmt.Errorf("%s did not take its new value", e.name)
		}
	}
	return out, nil
}

// mappingEntry returns the key and value nodes of name in the block mapping
// m, or nils when m is not a mapping or has no such key.
func mappingEntry(m *yaml.Node, name string) (key, value *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == name {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

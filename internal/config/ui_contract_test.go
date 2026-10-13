package config

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find go.mod in parent directories")
		}
		dir = parent
	}
}

// uiKeyword is one section=/keyword= pair on a config component in the UI.
type uiKeyword struct {
	component, section, keyword string
}

// uiConfigKeywords walks the Svelte config components and returns every
// section=/keyword= pair on a ConfigInput, ConfigSwitch, ConfigTextarea or
// ConfigSelect. It fails the test when it finds none, since that means the
// parsing broke rather than that the UI has no settings.
func uiConfigKeywords(t *testing.T) []uiKeyword {
	t.Helper()
	root, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot: %v", err)
	}

	configUIDir := filepath.Join(root, "ui", "src", "lib", "components", "config")

	compRegex := regexp.MustCompile(`(?s)<(ConfigInput|ConfigSwitch|ConfigTextarea|ConfigSelect)\b([^>]*?)>`)
	secRegex := regexp.MustCompile(`\bsection=["']([^"']*)["']`)
	keyRegex := regexp.MustCompile(`\bkeyword=["']([^"']*)["']`)

	var found []uiKeyword
	err = filepath.WalkDir(configUIDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".svelte" {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for _, match := range compRegex.FindAllStringSubmatch(string(content), -1) {
			attrs := match[2]
			secMatch := secRegex.FindStringSubmatch(attrs)
			keyMatch := keyRegex.FindStringSubmatch(attrs)
			if len(secMatch) < 2 || len(keyMatch) < 2 {
				continue
			}
			found = append(found, uiKeyword{component: filepath.Base(path), section: secMatch[1], keyword: keyMatch[1]})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	if len(found) == 0 {
		t.Fatalf("No ConfigInput/ConfigSwitch/ConfigTextarea/ConfigSelect components found, parsing logic might be broken")
	}
	return found
}

// TestUIKeywordsAreValidConfigTags verifies that every (section, keyword)
// pair defined in Svelte config components resolves to a real Go struct field
// via its json tag.
//
// This test dynamically walks the Svelte UI directory at test runtime, extracts
// section and keyword props from ConfigInput/ConfigSwitch/ConfigTextarea/ConfigSelect
// tags, and calls config.Set. Misspellings or renames will cause the test to fail.
func TestUIKeywordsAreValidConfigTags(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}

	for _, k := range uiConfigKeywords(t) {
		t.Run(k.component+":"+k.section+"/"+k.keyword, func(t *testing.T) {
			setErr := cfg.Set(k.section, k.keyword, "")
			if setErr != nil && strings.Contains(setErr.Error(), "invalid keyword") {
				t.Errorf(
					"[%s] keyword %q in section %q has no matching json tag in any Go config struct\n"+
						"  → Either the Svelte keyword= prop is misspelled, or the Go field was renamed/removed.",
					k.component, k.keyword, k.section,
				)
			}
		})
	}
}

// TestUIKeywordsAcceptTheirOwnValue sets every UI keyword to the value
// get_config reports for it, in the form the UI sends back: a JSON string
// unquoted, and any other JSON value as its text. Set must accept each one,
// so a field whose Go kind Set cannot convert fails here instead of in the
// settings page (#909: par2_max_packet_body_size was a uint64).
func TestUIKeywordsAcceptTheirOwnValue(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}

	for _, k := range uiConfigKeywords(t) {
		var fields map[string]json.RawMessage
		if json.Unmarshal(top[k.section], &fields) != nil {
			continue // a slice section (servers, categories), set as a whole array
		}
		t.Run(k.component+":"+k.section+"/"+k.keyword, func(t *testing.T) {
			v, ok := fields[k.keyword]
			if !ok {
				t.Fatalf("get_config has no %s.%s", k.section, k.keyword)
			}
			value := string(v)
			var s string
			if v[0] == '"' && json.Unmarshal(v, &s) == nil {
				value = s
			}
			if err := cfg.Set(k.section, k.keyword, value); err != nil {
				t.Errorf("Set(%q, %q, %q) = %v, want nil", k.section, k.keyword, value, err)
			}
		})
	}
}

// TestAllFlatConfigTagsAreSettable verifies the inverse direction: every json
// tag on every flat config struct (General, Downloads, PostProc) can be
// reached by config.Set(). This catches Go fields that are serialised into the
// JSON response but whose json tag does not round-trip through Set().
//
// Unlike TestUIKeywordsAreValidConfigTags (which checks Svelte→Go), this test
// checks Go→Go: it enumerates tags automatically from the struct so it never
// needs updating when new fields are added to the Go side.
func TestAllFlatConfigTagsAreSettable(t *testing.T) {
	type sectionSpec struct {
		name  string
		value any
	}

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}

	sections := []sectionSpec{
		{"general", cfg.General},
		{"downloads", cfg.Downloads},
		{"postproc", cfg.PostProc},
	}

	for _, sec := range sections {
		rv := reflect.TypeOf(sec.value)
		for field := range rv.Fields() {
			tag := field.Tag.Get("json")
			if tag == "" || tag == "-" {
				continue
			}
			keyword, _, _ := strings.Cut(tag, ",")
			if keyword == "" || keyword == "-" {
				continue
			}

			t.Run(sec.name+"/"+keyword, func(t *testing.T) {
				setErr := cfg.Set(sec.name, keyword, "")
				if setErr != nil && strings.Contains(setErr.Error(), "invalid keyword") {
					t.Errorf(
						"Go field %s.%s (json:%q) cannot be reached via Set(%q, %q, \"\").\n"+
							"  → The json tag exists on the struct but findFieldByTag() did not find it.",
						rv.Name(), field.Name, keyword, sec.name, keyword,
					)
				}
			})
		}
	}
}

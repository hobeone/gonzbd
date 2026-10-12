package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestReplaceStrings_RefusedOnSetAndLoad pins that a replacement string
// holding a path separator or NUL is refused by both doors a value enters
// through: Set (the UI's set_config) and Load (the YAML file). Either
// replacement is applied to indexer-supplied names that become a job's
// download directory, so a "/" would let a name like "a .. .. etc" become
// "a/../../etc".
func TestReplaceStrings_RefusedOnSetAndLoad(t *testing.T) {
	t.Parallel()
	for _, keyword := range []string{"replace_illegal_with", "replace_spaces_with"} {
		for _, bad := range []string{"/", `\`, "\x00", "../"} {
			cfg := mustDefault(t)
			before := cfg.Downloads
			if err := cfg.Set("downloads", keyword, bad); err == nil || !strings.Contains(err.Error(), keyword) {
				t.Errorf("Set(downloads, %s, %q) = %v, want a refusal naming %s", keyword, bad, err, keyword)
			}
			if cfg.Downloads.ReplaceIllegalWith != before.ReplaceIllegalWith || cfg.Downloads.ReplaceSpacesWith != before.ReplaceSpacesWith {
				t.Errorf("a refused Set(downloads, %s, %q) left illegal=%q spaces=%q", keyword, bad,
					cfg.Downloads.ReplaceIllegalWith, cfg.Downloads.ReplaceSpacesWith)
			}
		}
		// "." for spaces is a common naming style and cannot produce a "."
		// or ".." component, so it stays allowed.
		cfg := mustDefault(t)
		if err := cfg.Set("downloads", keyword, "."); err != nil {
			t.Errorf("Set(downloads, %s, \".\") = %v, want nil", keyword, err)
		}
	}

	line := regexp.MustCompile(`(?m)^(\s*replace_spaces_with:).*$`)
	base := minimalYAML(t)
	if !line.MatchString(base) {
		t.Fatal("the fixture YAML has no replace_spaces_with line to replace")
	}
	yaml := line.ReplaceAllString(base, "${1} /")
	path := filepath.Join(t.TempDir(), "gonzbd.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "replace_spaces_with") {
		t.Errorf("Load with replace_spaces_with: / = %v, want a refusal naming it", err)
	}
}

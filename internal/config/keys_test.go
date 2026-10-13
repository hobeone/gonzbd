package config

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// keyLine matches a general-section api_key or nzb_key line.
var keyLine = regexp.MustCompile(`^\s*(api_key|nzb_key):`)

// withoutKeyLines drops the api_key/nzb_key lines, and the "general:" line
// when dropGeneral is set, so the rest of a file can be compared exactly.
func withoutKeyLines(b []byte, dropGeneral bool) []byte {
	var out []byte
	for line := range bytes.SplitAfterSeq(b, []byte("\n")) {
		if keyLine.Match(line) || (dropGeneral && string(line) == "general:\n") {
			continue
		}
		out = append(out, line...)
	}
	return out
}

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gonzbd.yaml")
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestLoad_GeneratesAndPersistsMissingKeys pins the first-start path for a
// file that leaves the keys empty or out: Load generates both, and
// PersistGeneratedKeys writes them back so the next Load reads the same keys,
// changing no other byte of the file.
func TestLoad_GeneratesAndPersistsMissingKeys(t *testing.T) {
	const head = "# operator header comment\n"
	const tail = "downloads:\n  # keep this comment\n  max_active_jobs: 3 # inline\n"
	cases := []struct {
		name        string
		content     string
		addsGeneral bool
	}{
		{"empty quoted", head + "general:\n  # key comment\n  api_key: \"\"  # set me\n  nzb_key: ''\n  host: 127.0.0.1\n" + tail, false},
		{"null values", head + "general:\n  api_key:\n  nzb_key: ~\n  host: 127.0.0.1\n" + tail, false},
		{"keys left out", head + "general:\n    host: 127.0.0.1 # four-space indent\n" + tail, false},
		{"general null", head + "general:\n" + tail, false},
		{"general left out", head + tail, true},
		{"empty file", "", true},
		{"no trailing newline", head + "downloads:\n  max_active_jobs: 3", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigFile(t, tc.content)
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			g := cfg.GetGeneral()
			if !apiKeyPattern.MatchString(g.APIKey) || !apiKeyPattern.MatchString(g.NZBKey) || g.APIKey == g.NZBKey {
				t.Fatalf("keys not generated: api_key=%q nzb_key=%q", g.APIKey, g.NZBKey)
			}
			if err := cfg.PersistGeneratedKeys(); err != nil {
				t.Fatalf("PersistGeneratedKeys: %v", err)
			}

			again, err := Load(path)
			if err != nil {
				t.Fatalf("second Load: %v", err)
			}
			if g2 := again.GetGeneral(); g2.APIKey != g.APIKey || g2.NZBKey != g.NZBKey {
				t.Fatalf("second Load read api_key=%q nzb_key=%q, want the generated %q %q",
					g2.APIKey, g2.NZBKey, g.APIKey, g.NZBKey)
			}
			if again.generated != nil {
				t.Fatal("second Load generated keys again")
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			orig := tc.content
			if orig != "" && !strings.HasSuffix(orig, "\n") {
				orig += "\n"
			}
			if want, have := withoutKeyLines([]byte(orig), false), withoutKeyLines(got, tc.addsGeneral); !bytes.Equal(want, have) {
				t.Fatalf("lines other than the keys changed\nbefore:\n%s\nafter:\n%s", tc.content, got)
			}
			for _, c := range []string{"# operator header comment", "# keep this comment", "# inline", "# set me", "# key comment"} {
				if strings.Contains(tc.content, c) && !bytes.Contains(got, []byte(c)) {
					t.Errorf("comment %q lost:\n%s", c, got)
				}
			}
			if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o640 {
				t.Errorf("mode after write = %v (err %v), want 0640", st.Mode().Perm(), err)
			}
		})
	}
}

// TestLoad_KeepsAPresentKey pins that only the missing key is generated and
// written: a present one, and its line, are left exactly as they were.
func TestLoad_KeepsAPresentKey(t *testing.T) {
	content := "general:\n  api_key: 0011223344556677 # mine\n  nzb_key: \"\"\n"
	path := writeConfigFile(t, content)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.GetGeneral().APIKey; got != "0011223344556677" {
		t.Fatalf("APIKey = %q, want the file's", got)
	}
	if err := cfg.PersistGeneratedKeys(); err != nil {
		t.Fatalf("PersistGeneratedKeys: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "general:\n  api_key: 0011223344556677 # mine\n  nzb_key: " + cfg.GetGeneral().NZBKey + "\n"
	if string(got) != want {
		t.Fatalf("file =\n%s\nwant\n%s", got, want)
	}
}

// TestPersistGeneratedKeys_NothingGenerated pins that a file holding both
// keys is never rewritten, even when it is read-only.
func TestPersistGeneratedKeys_NothingGenerated(t *testing.T) {
	content := "general:\n  api_key: 0011223344556677\n  nzb_key: 8899aabbccddeeff\n"
	path := writeConfigFile(t, content)
	if err := os.Chmod(path, 0o440); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.PersistGeneratedKeys(); err != nil {
		t.Fatalf("PersistGeneratedKeys: %v", err)
	}
}

// TestPersistGeneratedKeys_ReadOnlyFileFails pins the choice for a file gonzbd
// cannot write: an error naming the file, and the file left as it was, rather
// than keys that would change on every start.
func TestPersistGeneratedKeys_ReadOnlyFileFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only file regardless of its mode")
	}
	content := "general:\n  host: 127.0.0.1\n"
	path := writeConfigFile(t, content)
	if err := os.Chmod(path, 0o440); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = cfg.PersistGeneratedKeys()
	if err == nil || !strings.Contains(err.Error(), "is not writable") {
		t.Fatalf("PersistGeneratedKeys = %v, want a not-writable error", err)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != content {
		t.Fatalf("a refused write changed the file:\n%s", got)
	}
}

// TestPersistGeneratedKeys_ThroughSymlink pins that a symlinked config keeps
// its symlink: the file it points at is the one rewritten.
func TestPersistGeneratedKeys_ThroughSymlink(t *testing.T) {
	target := writeConfigFile(t, "general:\n  host: 127.0.0.1\n")
	link := filepath.Join(t.TempDir(), "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(link)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.PersistGeneratedKeys(); err != nil {
		t.Fatalf("PersistGeneratedKeys: %v", err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced: %v %v", st, err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "api_key: "+cfg.GetGeneral().APIKey) {
		t.Fatalf("target not rewritten:\n%s", got)
	}
}

// TestWriteKeysFile_Errors covers the failures other than a read-only file:
// a path that no longer exists, and a directory the temp file cannot be
// created in.
func TestWriteKeysFile_Errors(t *testing.T) {
	if err := writeKeysFile(filepath.Join(t.TempDir(), "gone.yaml"), nil); err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Errorf("missing file: err = %v, want a resolve error", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root creates files in a read-only directory")
	}
	path := writeConfigFile(t, "general:\n")
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:errcheck // best-effort so TempDir cleanup can remove it
	if err := writeKeysFile(path, []byte("general:\n")); err == nil || !strings.Contains(err.Error(), "write generated keys") {
		t.Errorf("read-only directory: err = %v, want a write error", err)
	}
}

// TestSetGeneralKeys_RefusesLayoutsItCannotEdit pins that a flow-style
// general mapping is an error rather than a wrongly edited file.
func TestSetGeneralKeys_RefusesLayoutsItCannotEdit(t *testing.T) {
	for _, raw := range []string{
		"general: {host: x}\n",
		"{general: {host: x}}\n",
		"- a\n",
	} {
		out, err := setGeneralKeys([]byte(raw), []keyEdit{{"api_key", "0011223344556677"}})
		if err == nil || !strings.Contains(err.Error(), "not a block mapping") {
			t.Errorf("setGeneralKeys(%q) = %q, %v; want a not-a-block-mapping error", raw, out, err)
		}
	}
}

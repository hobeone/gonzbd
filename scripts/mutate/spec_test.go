package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write puts a spec in a temp file and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return path
}

func TestParseSpec_KeepsAnchorBytesExactly(t *testing.T) {
	t.Parallel()

	// The tab indentation is the whole reason this format is not YAML or
	// JSON: an anchor is matched against source with strings.Count, so a
	// single re-indented byte turns a real mutation into an ANCHOR failure.
	body := "pkg ./internal/queue/\nrun TestFoo\n\n" +
		"[gate]\nfile internal/queue/queue.go\n" +
		"--- anchor\n\tif err != nil {\n\t\treturn false\n\t}\n" +
		"--- replace\n\tif !ok {\n\t\treturn false\n\t}\n" +
		"--- end\n"

	sp, err := parseSpec(write(t, body))
	if err != nil {
		t.Fatalf("parseSpec: %v", err)
	}
	if len(sp.mutations) != 1 {
		t.Fatalf("got %d mutations, want 1", len(sp.mutations))
	}
	m := sp.mutations[0]
	if want := "\tif err != nil {\n\t\treturn false\n\t}"; m.anchor != want {
		t.Errorf("anchor = %q, want %q", m.anchor, want)
	}
	if want := "\tif !ok {\n\t\treturn false\n\t}"; m.replace != want {
		t.Errorf("replace = %q, want %q", m.replace, want)
	}
	if sp.pkg != "./internal/queue/" || sp.run != "TestFoo" {
		t.Errorf("pkg/run = %q/%q", sp.pkg, sp.run)
	}
}

func TestParseSpec_ContentLinesAreLiteral(t *testing.T) {
	t.Parallel()

	// A '#' inside an anchor is a Go build directive or a shell line in a
	// heredoc, never a spec comment. Blank lines matter too — they are part
	// of the matched text.
	body := "pkg ./p/\n\n[m]\nfile a.go\n" +
		"--- anchor\n# not a comment\n\nx := 1\n" +
		"--- replace\n# still not a comment\n\nx := 2\n" +
		"--- end\n"

	sp, err := parseSpec(write(t, body))
	if err != nil {
		t.Fatalf("parseSpec: %v", err)
	}
	if want := "# not a comment\n\nx := 1"; sp.mutations[0].anchor != want {
		t.Errorf("anchor = %q, want %q", sp.mutations[0].anchor, want)
	}
}

func TestParseSpec_ParsesTimeoutAndMultipleMutations(t *testing.T) {
	t.Parallel()

	body := "pkg ./p/\ntimeout 15m\n\n" +
		"[one]\nfile a.go\n--- anchor\na\n--- replace\nb\n--- end\n" +
		"[two]\nfile b.go\n--- anchor\nc\n--- replace\nd\n--- end\n"

	sp, err := parseSpec(write(t, body))
	if err != nil {
		t.Fatalf("parseSpec: %v", err)
	}
	if sp.timeout != 15*time.Minute {
		t.Errorf("timeout = %v, want 15m", sp.timeout)
	}
	if len(sp.mutations) != 2 {
		t.Fatalf("got %d mutations, want 2", len(sp.mutations))
	}
	if sp.mutations[1].name != "two" || sp.mutations[1].file != "b.go" {
		t.Errorf("second mutation = %+v", sp.mutations[1])
	}
}

func TestParseSpec_Rejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{{
		// A no-op mutation always SURVIVES, which reads as "the test does not
		// discriminate" when in truth nothing was changed. Rejecting it here
		// is the difference between a wrong answer and no answer.
		name: "identical anchor and replace",
		body: "pkg ./p/\n[m]\nfile a.go\n--- anchor\nx\n--- replace\nx\n--- end\n",
		want: "identical",
	}, {
		// A missing or misspelled `--- replace` leaves the replacement empty
		// and turns the mutation into a DELETION, which AGENTS.md warns
		// against precisely because a deletion breaks the build instead of
		// the test.
		name: "missing replace section",
		body: "pkg ./p/\n[m]\nfile a.go\n--- anchor\nx\n--- end\n",
		want: "no `--- replace` section",
	}, {
		name: "global directive inside a mutation block",
		body: "pkg ./p/\n[m]\nfile a.go\nrun TestFoo\n--- anchor\nx\n--- replace\ny\n--- end\n",
		want: "global directive",
	}, {
		// An empty anchor matches at every byte offset, so it would mutate
		// the head of the file rather than the site the author meant.
		name: "empty anchor",
		body: "pkg ./p/\n[m]\nfile a.go\n--- anchor\n--- replace\ny\n--- end\n",
		want: "empty anchor",
	}, {
		name: "missing file line",
		body: "pkg ./p/\n[m]\n--- anchor\nx\n--- replace\ny\n--- end\n",
		want: "no `file` line",
	}, {
		name: "unterminated section",
		body: "pkg ./p/\n[m]\nfile a.go\n--- anchor\nx\n--- replace\ny\n",
		want: "unterminated",
	}, {
		name: "no pkg",
		body: "[m]\nfile a.go\n--- anchor\nx\n--- replace\ny\n--- end\n",
		want: "no `pkg` line",
	}, {
		name: "no mutations",
		body: "pkg ./p/\nrun TestFoo\n",
		want: "no [mutation] blocks",
	}, {
		name: "empty mutation name",
		body: "pkg ./p/\n[]\nfile a.go\n--- anchor\nx\n--- replace\ny\n--- end\n",
		want: "name is empty",
	}, {
		name: "unknown key",
		body: "pkg ./p/\nnosuchkey value\n[m]\nfile a.go\n--- anchor\nx\n--- replace\ny\n--- end\n",
		want: "unknown key",
	}, {
		name: "file outside a block",
		body: "pkg ./p/\nfile a.go\n",
		want: "outside a [mutation] block",
	}, {
		name: "bad timeout",
		body: "pkg ./p/\ntimeout notaduration\n[m]\nfile a.go\n--- anchor\nx\n--- replace\ny\n--- end\n",
		want: "timeout",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseSpec(write(t, tc.body))
			if err == nil {
				t.Fatalf("parseSpec accepted %q", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseSpec_HandlesCRLFSpecFiles(t *testing.T) {
	t.Parallel()

	// Stripping the CR only from delimiters left anchors holding \r\n, which
	// match zero sites in an LF source file — so every mutation in a CRLF
	// spec failed as ANCHOR, blaming the anchor for the file's encoding.
	body := "pkg ./p/\r\nrun TestFoo\r\n\r\n[m]\r\nfile a.go\r\n" +
		"--- anchor\r\nif a {\r\n--- replace\r\nif b {\r\n--- end\r\n"

	sp, err := parseSpec(write(t, body))
	if err != nil {
		t.Fatalf("parseSpec: %v", err)
	}
	if got := sp.mutations[0].anchor; got != "if a {" {
		t.Errorf("anchor = %q, want %q with no carriage return", got, "if a {")
	}
	if got := sp.mutations[0].replace; got != "if b {" {
		t.Errorf("replace = %q, want %q", got, "if b {")
	}
	if sp.pkg != "./p/" {
		t.Errorf("pkg = %q", sp.pkg)
	}
}

func TestParseSpec_ParsesTags(t *testing.T) {
	t.Parallel()

	// test/integration, test/uitest and test/crash are all behind //go:build
	// tags, so no pin in any of them is red-checkable without this.
	body := "pkg ./test/integration/\ntags integration\n\n" +
		"[m]\nfile a.go\n--- anchor\nx\n--- replace\ny\n--- end\n"
	sp, err := parseSpec(write(t, body))
	if err != nil {
		t.Fatalf("parseSpec: %v", err)
	}
	if sp.tags != "integration" {
		t.Errorf("tags = %q, want %q", sp.tags, "integration")
	}
	if got := strings.Join(testArgs(sp), " "); !strings.Contains(got, "-tags=integration") {
		t.Errorf("testArgs = %q, missing -tags", got)
	}
}

func TestParseSpec_MissingFileIsAnError(t *testing.T) {
	t.Parallel()

	if _, err := parseSpec(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("parseSpec accepted a path that does not exist")
	}
}

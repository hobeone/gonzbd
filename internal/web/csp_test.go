package web

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestScriptHashesFromHTML(t *testing.T) {
	t.Parallel()

	const html = `<!doctype html>
<html>
<head>
<script src="/theme.js"></script>
<script>console.log("inline one")</script>
<link rel="stylesheet" href="/app.css">
<script type="module">console.log("inline two")</script>
</head>
<body></body>
</html>`

	got := scriptHashesFromHTML([]byte(html))

	wantBodies := []string{
		`console.log("inline one")`,
		`console.log("inline two")`,
	}
	if len(got) != len(wantBodies) {
		t.Fatalf("scriptHashesFromHTML returned %d hashes, want %d: %v", len(got), len(wantBodies), got)
	}
	for i, body := range wantBodies {
		sum := sha256.Sum256([]byte(body))
		want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if got[i] != want {
			t.Errorf("hash[%d] = %q; want %q", i, got[i], want)
		}
	}
}

func TestScriptHashesFromHTML_NoInlineScripts(t *testing.T) {
	t.Parallel()

	const html = `<html><head><script src="/a.js"></script></head><body></body></html>`
	got := scriptHashesFromHTML([]byte(html))
	if len(got) != 0 {
		t.Errorf("scriptHashesFromHTML = %v; want empty (only external scripts present)", got)
	}
}

func TestIndexScriptHashes(t *testing.T) {
	t.Parallel()

	hashes, err := IndexScriptHashes()
	if err != nil {
		t.Fatalf("IndexScriptHashes() error: %v", err)
	}
	// The SvelteKit build always injects at least one inline hydration
	// bootstrap <script>; if this ever drops to zero it likely means the
	// embedded dist/index.html wasn't built, which would also silently
	// break the CSP this function feeds.
	if len(hashes) == 0 {
		t.Error("IndexScriptHashes() returned no hashes; expected at least the SvelteKit bootstrap script")
	}
}

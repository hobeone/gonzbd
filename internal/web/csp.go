package web

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"regexp"

	"github.com/hobeone/gonzbd/ui"
)

// inlineScriptRe matches <script ...>body</script> tags, capturing the
// opening-tag attributes and the body separately so callers can skip tags
// that load an external file (src=...) rather than embedding inline code.
var inlineScriptRe = regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)

// hasSrcAttr matches a src="..." or src='...' attribute.
var hasSrcAttr = regexp.MustCompile(`(?i)\bsrc\s*=`)

// IndexScriptHashes returns CSP "'sha256-<base64>'" source expressions, one
// per inline (non-src) <script> body found in the embedded index.html. The
// SvelteKit build injects its own hydration bootstrap script inline, so its
// content (and therefore hash) can change across builds; computing the
// hash at startup from the actual served bytes means the CSP header never
// drifts out of sync with what's in the binary, unlike a hardcoded hash.
func IndexScriptHashes() ([]string, error) {
	dist, err := fs.Sub(ui.DistFS, "dist")
	if err != nil {
		return nil, fmt.Errorf("web: sub dist fs: %w", err)
	}
	b, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return nil, fmt.Errorf("web: read index.html: %w", err)
	}
	return scriptHashesFromHTML(b), nil
}

// scriptHashesFromHTML is the pure extraction logic behind IndexScriptHashes,
// split out for testing with arbitrary HTML rather than the real build output.
func scriptHashesFromHTML(html []byte) []string {
	var hashes []string
	for _, m := range inlineScriptRe.FindAllSubmatch(html, -1) {
		attrs, body := m[1], m[2]
		if hasSrcAttr.Match(attrs) {
			continue // external script, already covered by script-src 'self'
		}
		sum := sha256.Sum256(body)
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return hashes
}

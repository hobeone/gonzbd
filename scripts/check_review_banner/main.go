// Command check_review_banner verifies that every audit snapshot under
// docs/reviews/ carries a "Frozen record" banner.
//
// Those files are dated audits of a past commit, not living contracts. Large
// parts of them were falsified by the download-durability work — deleted
// identifiers, dropped columns, a changed shutdown order — and that is
// expected and fine, because a frozen record is allowed to be out of date. It
// is only safe while each file SAYS it is frozen. Without the banner a reader
// arriving from a grep has no way to tell an audit snapshot from
// docs/durability-contract.md, and the confidently-worded stale claim wins.
//
// No other gate reaches this. Build, vet, lint and the test suite do not read
// Markdown at all, and the AGENTS.md claim sweep looks for wrong sentences
// rather than missing frames — the banner's absence is not a wrong sentence,
// it is the absence of the thing that makes every other sentence in the file
// legible. The check is deliberately a presence test and not a content test:
// it does not try to judge whether a review's claims are still true, only that
// the file admits it might not be.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// bannerLabel is the phrase that marks a file as an audit snapshot.
const bannerLabel = "Frozen record"

// commitRef matches the backticked commit the snapshot was taken at.
//
// Requiring it is the second half of the check, and it is the half with teeth:
// "this is a frozen record" without a commit is unfalsifiable, because a reader
// who wants to know what changed since has nothing to diff against. With the
// SHA, `git diff <sha>..HEAD -- <scope>` answers it.
//
// An earlier version of this file required a link to docs/durability-contract.md
// instead. That was wrong and the tool caught it on its first run:
// lane5-frontend.md reviews ui/ and has no durability contract to point at. The
// rule has to hold for every lane, so it asks for what every snapshot has — a
// label and the commit it describes — not for a pointer whose target differs by
// subject.
var commitRef = regexp.MustCompile("`[0-9a-f]{7,40}`")

func main() {
	dir := flag.String("dir", filepath.Join("docs", "reviews"), "directory of review documents to check")
	flag.Parse()

	missing, err := check(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check_review_banner: %v\n", err)
		os.Exit(2)
	}
	if len(missing) == 0 {
		return
	}
	for _, m := range missing {
		fmt.Printf("%s: missing %s\n", m.path, strings.Join(m.markers, ", "))
	}
	fmt.Fprintf(os.Stderr, "check_review_banner: %d review document(s) without a complete frozen-record banner.\n"+
		"Add a blockquote stating the file is a frozen record rather than a living\n"+
		"contract, and naming the commit it was taken at.\n", len(missing))
	os.Exit(1)
}

// finding is one document and the markers it lacks.
type finding struct {
	path    string
	markers []string
}

// check returns the review documents missing any required marker.
//
// A missing directory is an error rather than a pass. "No files to check"
// and "the directory moved" are indistinguishable from the outside, and the
// second silently disables the gate — which is the failure mode a presence
// check is least able to notice about itself.
func check(dir string) ([]finding, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []finding
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path) //nolint:gosec // G304: path is built from the checked directory
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		body := string(b)
		var lacks []string
		if !strings.Contains(body, bannerLabel) {
			lacks = append(lacks, fmt.Sprintf("the %q label", bannerLabel))
		}
		if !commitRef.MatchString(body) {
			lacks = append(lacks, "a backticked commit SHA naming the reviewed state")
		}
		if len(lacks) > 0 {
			out = append(out, finding{path: path, markers: lacks})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

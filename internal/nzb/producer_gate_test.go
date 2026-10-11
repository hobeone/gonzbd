package nzb

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

const nzbImportPath = "github.com/hobeone/gonzbd/internal/nzb"

// TestNoArticleProducedOutsideNZBPackage pins the third fact behind
// the round-trip claim in docs/article-validation-contract.md: the NZB parser
// is the only producer of an Article, so every article that reaches a manifest
// has passed partitionSegments' Message-ID checks.
//
// The gate bans the identifier nzb.Article itself in non-test code outside
// this package, rather than only the composite literal. Every way of making
// an Article by hand (a literal, &, new, make([]nzb.Article, n), a var
// declaration, a conversion, a type alias) has to name the type, and no
// consumer has a reason to. Consumers reach articles through File.Articles
// and never name the element type. A new use fails until someone routes it
// through the parser or removes it.
//
// Test files are exempt. Tests hand-build NZB models to drive consumers
// (`git grep -l 'nzb\.Article' -- '*_test.go' ':!internal/nzb/'` lists 55 files), and a test-built value never reaches a manifest a production build
// wrote. The invariant concerns what production writes to disk.
//
// What this does not catch: a write to an article already sitting in a
// File.Articles slice (the fields are exported), and a zero Article obtained
// without naming the type, such as a generic zero value. Closing those needs
// the fields unexported, which the issue (#404) put out of scope.
func TestNoArticleProducedOutsideNZBPackage(t *testing.T) {
	t.Parallel()

	root := gitOutput(t, "rev-parse", "--show-toplevel")
	listing := gitOutput(t, "-C", strings.TrimSpace(root), "ls-files", "-z", "--", "*.go")

	var parsed int
	fset := token.NewFileSet()
	for rel := range strings.SplitSeq(listing, "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "internal/nzb/") {
			continue
		}
		path := strings.TrimSpace(root) + "/" + rel
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		parsed++
		for _, pos := range articleNamings(f) {
			t.Errorf("%s names nzb.Article outside internal/nzb.\n"+
				"    The parser must be the only producer of an Article: A7's uniqueness and the Message-ID fetchability checks hold for a manifest on disk only because nothing else can add an article to it.\n"+
				"    Route the data through nzb.Parse, or see docs/article-validation-contract.md before changing this gate.\n"+
				"    at %s", rel, fset.Position(pos))
		}
	}
	if parsed < 50 {
		t.Fatalf("parsed only %d non-test sources outside internal/nzb; the file listing no longer reaches the tree and this gate would pass vacuously", parsed)
	}
}

// TestArticleNamingDetector pins the detector the gate relies on, so a gate
// that passes cannot be a detector that finds nothing.
func TestArticleNamingDetector(t *testing.T) {
	t.Parallel()

	const head = "package p\nimport nzb \"" + nzbImportPath + "\"\n"
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"composite literal", head + "var _ = nzb.Article{}", 1},
		{"address of literal", head + "var _ = &nzb.Article{}", 1},
		{"slice element type", head + "var _ = make([]nzb.Article, 3)", 1},
		{"var declaration", head + "var a nzb.Article", 1},
		{"type alias", head + "type A = nzb.Article", 1},
		{"new", head + "var _ = new(nzb.Article)", 1},
		{"renamed import", "package p\nimport n \"" + nzbImportPath + "\"\nvar _ = n.Article{}", 1},
		{"second import name", head + "import zz \"" + nzbImportPath + "\"\nvar _ = nzb.Article{}", 1},
		{"dot import", "package p\nimport . \"" + nzbImportPath + "\"\nvar _ = Article{}", 1},
		{"other nzb member", head + "var _ nzb.File", 0},
		{"reading File.Articles", head + "func f(x nzb.File) int { return len(x.Articles) }", 0},
		{"unrelated package named nzb", "package p\nimport nzb \"example.com/other\"\nvar _ = nzb.Article{}", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := len(articleNamings(f)); got != tc.want {
				t.Errorf("articleNamings found %d, want %d", got, tc.want)
			}
		})
	}
}

// articleNamings returns the position of every reference to this package's
// Article type in f, resolved through f's imports so that an alias is caught
// and an unrelated package that happens to be called nzb is not. A dot import
// of the package is reported at the import, since it hides the qualifier the
// walk keys on.
func articleNamings(f *ast.File) []token.Pos {
	var out []token.Pos
	local := map[string]bool{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != nzbImportPath {
			continue
		}
		switch {
		case imp.Name == nil:
			local["nzb"] = true
		case imp.Name.Name == ".":
			out = append(out, imp.Pos())
		case imp.Name.Name != "_":
			local[imp.Name.Name] = true
		}
	}
	if len(local) == 0 {
		return out
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Article" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && local[id.Name] {
			out = append(out, sel.Pos())
		}
		return true
	})
	return out
}

// gitOutput runs git and returns stdout. The file list comes from
// git ls-files rather than a directory walk, which would descend into the
// gitignored .claude/worktrees/ and judge a sibling branch's sources.
func gitOutput(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

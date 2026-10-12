package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// goTestListNames is the reference listTests is held to: the names
// `go test -list '^Test'` prints, unfiltered, sorted.
func goTestListNames(t *testing.T, root string, sp *spec) ([]string, error) {
	t.Helper()
	cmd := exec.Command("go", listArgs(sp)...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, out)
	}
	var names []string
	for line := range strings.SplitSeq(string(out), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "ok ") || strings.HasPrefix(s, "? ") {
			continue
		}
		names = append(names, s)
	}
	slices.Sort(names)
	return names, nil
}

// fixtureModule writes a module from a file map and returns its directory.
func fixtureModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.mod"), "module m\n\ngo 1.24\n")
	for name, body := range files {
		mustWrite(t, filepath.Join(root, name), body)
	}
	return root
}

const testlistHeader = "package m\n\nimport \"testing\"\n\n"

// TestListTests_AgreesWithGoTestList holds the static reading to `go test
// -list` on fixtures built from each rule cmd/go applies: the name rule, the
// signature rule, TestMain, methods, external test packages, build tags,
// GOOS file suffixes and `//go:build ignore`.
func TestListTests_AgreesWithGoTestList(t *testing.T) {
	t.Parallel()

	rules := map[string]string{
		"m.go": "package m\n\nfunc X() {}\n",
		"a_test.go": testlistHeader +
			"func TestA(t *testing.T) {}\n" +
			"func Test(t *testing.T) {}\n" +
			"func Testable(n int) {}\n" + // lower case after Test: not a test, so not a bad signature either
			"func Test_under(t *testing.T) {}\n" +
			"func TestÜber(t *testing.T) {}\n" + // upper case non-ASCII: a test
			"func TestMain(m *testing.M) { m.Run() }\n" +
			"type r struct{}\n" +
			"func (r) TestMethod(t *testing.T) {}\n" +
			"func BenchmarkX(b *testing.B) {}\n" +
			"func ExampleX() {}\n" +
			"func FuzzX(f *testing.F) {}\n" +
			"func helper(t *testing.T) {}\n",
		"x_test.go":        "package m_test\n\nimport \"testing\"\n\nfunc TestExternal(t *testing.T) {}\n",
		"tagged_test.go":   "//go:build sometag\n\n" + testlistHeader + "func TestTagged(t *testing.T) {}\n",
		"z_linux_test.go":  testlistHeader + "func TestLinuxOnly(t *testing.T) {}\n",
		"z_darwin_test.go": testlistHeader + "func TestDarwinOnly(t *testing.T) {}\n",
		"ignored_test.go":  "//go:build ignore\n\n" + testlistHeader + "func TestIgnored(t *testing.T) {}\n",
	}

	cases := []struct {
		name  string
		files map[string]string
		tags  string
	}{
		{"every rule, no tags", rules, ""},
		{"every rule, tagged", rules, "sometag"},
		{"every rule, two tags", rules, "sometag,other"},
		{"TestMain taking *testing.T is a test", map[string]string{
			"a_test.go": testlistHeader + "func TestMain(t *testing.T) {}\n",
		}, ""},
		{"a dot-imported testing package", map[string]string{
			"a_test.go": "package m\n\nimport . \"testing\"\n\nfunc TestDot(t *T) {}\n",
		}, ""},
		{"only an external test package", map[string]string{
			"m.go":      "package m\n",
			"x_test.go": "package m_test\n\nimport \"testing\"\n\nfunc TestOnlyExternal(t *testing.T) {}\n",
		}, ""},
		{"a Test-named function with the wrong signature", map[string]string{
			"a_test.go": testlistHeader + "func TestBad(x int) {}\n",
		}, ""},
		{"a Test function with a result", map[string]string{
			"a_test.go": testlistHeader + "func TestResult(t *testing.T) int { return 0 }\n",
		}, ""},
		{"a generic Test function", map[string]string{
			"a_test.go": testlistHeader + "func TestGeneric[P any](t *testing.T) {}\n",
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := fixtureModule(t, tc.files)
			sp := &spec{pkg: ".", tags: tc.tags}

			want, wantErr := goTestListNames(t, root, sp)
			got, gotErr := listTests(root, sp)
			if (wantErr != nil) != (gotErr != nil) {
				t.Fatalf("listTests error = %v, go test -list error = %v; they must agree", gotErr, wantErr)
			}
			if !slices.Equal(got, want) {
				t.Errorf("listTests = %v, go test -list = %v", got, want)
			}
		})
	}
}

// TestListTests_AgreesWithGoTestListOnRealPackages runs the same comparison
// against packages of this repository: one with an external test package, one
// with both kinds, one behind a build tag, and this command's own.
func TestListTests_AgreesWithGoTestListOnRealPackages(t *testing.T) {
	t.Parallel()

	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range []*spec{
		{pkg: "./internal/notifier/"},
		{pkg: "./internal/config/"},
		{pkg: "./test/crash/", tags: "crash"},
		{pkg: "./scripts/mutate/"},
	} {
		t.Run(sp.pkg, func(t *testing.T) {
			t.Parallel()
			want, err := goTestListNames(t, root, sp)
			if err != nil {
				t.Fatalf("go test -list: %v", err)
			}
			got, err := listTests(root, sp)
			if err != nil {
				t.Fatalf("listTests: %v", err)
			}
			if len(want) == 0 {
				t.Fatal("go test -list named no test; the comparison would be vacuous")
			}
			if !slices.Equal(got, want) {
				t.Errorf("listTests = %v\ngo test -list = %v", got, want)
			}
		})
	}
}

// TestListTests_LeavesAPatternToGoTestList pins staticDir's refusal: a `...`
// pattern is the go command's to expand, so it goes to `go test -list` — which
// compiles, and so fails on the package TestListTests_DoesNotCompileThePackage
// lists without error.
func TestListTests_LeavesAPatternToGoTestList(t *testing.T) {
	t.Parallel()

	root := mustModule(t, "func TestDeclared(t *testing.T) {}\n")
	mustWrite(t, filepath.Join(root, "broken.go"), "package m\n\nvar x int = \"not an int\"\n")

	if _, err := listTests(root, &spec{pkg: "./..."}); err == nil {
		t.Fatal("listTests on a ./... pattern did not go through go test -list")
	}
}

// TestListTestsIn_LeavesGOFLAGSToGoTestList pins the other refusal: GOFLAGS
// can carry -tags the static reading would not see.
func TestListTestsIn_LeavesGOFLAGSToGoTestList(t *testing.T) {
	t.Parallel()

	root := mustModule(t, "func TestDeclared(t *testing.T) {}\n")
	mustWrite(t, filepath.Join(root, "broken.go"), "package m\n\nvar x int = \"not an int\"\n")
	env, err := goEnvForListing()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := listTestsIn(env, root, &spec{pkg: "."}); err != nil {
		t.Fatalf("listTestsIn without GOFLAGS: %v", err)
	}
	env.GOFLAGS = "-tags=x"
	if _, err := listTestsIn(env, root, &spec{pkg: "."}); err == nil {
		t.Fatal("listTestsIn with GOFLAGS set did not go through go test -list")
	}
}

func TestStaticDir(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		pkg  string
		want bool
	}{
		{".", true},
		{"./internal/app/", true},
		{"../sibling", true},
		{"..", true},
		{"./...", false},
		{"./internal/...", false},
		{"github.com/hobeone/gonzbd/internal/app", false},
	} {
		if _, got := staticDir("/r", tc.pkg); got != tc.want {
			t.Errorf("staticDir(%q) ok = %v, want %v", tc.pkg, got, tc.want)
		}
	}
	if dir, _ := staticDir("/r", "./internal/app/"); dir != "/r/internal/app" {
		t.Errorf("staticDir dir = %q, want /r/internal/app", dir)
	}
}

func TestListingEnvContext_SplitsTagsOnCommaAndSpace(t *testing.T) {
	t.Parallel()

	ctx := listingEnv{GOOS: "plan9", GOARCH: "arm", CGO_ENABLED: "1"}.context("a,b c")
	if !slices.Equal(ctx.BuildTags, []string{"a", "b", "c"}) {
		t.Errorf("BuildTags = %v, want [a b c]", ctx.BuildTags)
	}
	if ctx.GOOS != "plan9" || ctx.GOARCH != "arm" || !ctx.CgoEnabled {
		t.Errorf("context = %s/%s cgo=%v, want plan9/arm cgo=true", ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled)
	}
	if (listingEnv{CGO_ENABLED: "0"}).context("").CgoEnabled {
		t.Error("CGO_ENABLED=0 gave a cgo-enabled context")
	}
}

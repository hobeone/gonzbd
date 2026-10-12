package main

import (
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// affectedModule writes a module whose import graph the selection rules are
// read against, and the specs that exercise them:
//
//	a imports b, b imports c; a's tests behind the `special` tag import t
//	u and t stand alone; g's test runs git, and so does c's — which is not
//	compiled into a's test binary
//
// and returns its root and the spec paths, sorted.
func affectedModule(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":            "module m\n\ngo 1.24\n",
		"a/a.go":            "package a\n\nimport \"m/b\"\n\nfunc A() int { return b.B() }\n",
		"a/a_test.go":       "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { _ = A() }\n",
		"a/special_test.go": "//go:build special\n\npackage a\n\nimport (\n\t\"testing\"\n\n\t\"m/t\"\n)\n\nfunc TestT(x *testing.T) { _ = t.T() }\n",
		"b/b.go":            "package b\n\nimport \"m/c\"\n\nfunc B() int { return c.C() }\n",
		"c/c.go":            "package c\n\nfunc C() int { return 1 }\n",
		"c/c_test.go":       "package c\n\nimport (\n\t\"os/exec\"\n\t\"testing\"\n)\n\nfunc TestC(t *testing.T) { _ = exec.Command(\"git\", \"status\") }\n",
		"t/t.go":            "package t\n\nfunc T() int { return 1 }\n",
		"u/u.go":            "package u\n\nfunc U() int { return 1 }\n",
		"g/g.go":            "package g\n\nfunc G() int { return 1 }\n",
		"g/g_test.go":       "package g\n\nimport (\n\t\"os/exec\"\n\t\"testing\"\n)\n\nfunc TestG(t *testing.T) { _ = exec.Command(\"git\", \"status\") }\n",
	}
	spec := func(head, file string) string {
		return head + "[m]\nfile " + file + "\n--- anchor\nreturn 1\n--- replace\nreturn 2\n--- end\n"
	}
	specs := map[string]string{
		"specs/a.spec":      spec("pkg ./a/\n", "c/c.go"),
		"specs/cross.spec":  spec("pkg ./a/\n", "u/u.go"),
		"specs/g.spec":      spec("pkg ./g/\n", "g/g.go"),
		"specs/tagged.spec": spec("pkg ./a/\ntags special\n", "c/c.go"),
		"specs/u.spec":      spec("pkg ./u/\n", "u/u.go"),
	}
	for name, body := range files {
		mustWriteAll(t, filepath.Join(root, name), body)
	}
	names := make([]string, 0, len(specs))
	for name, body := range specs {
		mustWriteAll(t, filepath.Join(root, name), body)
		names = append(names, name)
	}
	slices.Sort(names)
	return root, names
}

func TestAffectedSpecs_SelectsThroughTheTestBinarysPackages(t *testing.T) {
	t.Parallel()

	root, specs := affectedModule(t)
	cases := []struct {
		name    string
		changed []string
		want    []string
	}{
		{"a transitive dependency", []string{"c/c.go"},
			[]string{"specs/a.spec", "specs/cross.spec", "specs/g.spec", "specs/tagged.spec"}},
		{"a direct dependency's new file", []string{"b/new.go"},
			[]string{"specs/a.spec", "specs/cross.spec", "specs/g.spec", "specs/tagged.spec"}},
		{"the package's own test file", []string{"a/a_test.go"},
			[]string{"specs/a.spec", "specs/cross.spec", "specs/g.spec", "specs/tagged.spec"}},
		{"a package only the tagged build imports", []string{"t/t.go"},
			[]string{"specs/g.spec", "specs/tagged.spec"}},
		{"beside a cross-package spec's mutated file", []string{"u/other.go"},
			[]string{"specs/cross.spec", "specs/g.spec", "specs/u.spec"}},
		{"a package nothing imports", []string{"z/z.go"},
			[]string{"specs/g.spec"}},
		{"the spec itself", []string{"specs/u.spec"},
			[]string{"specs/g.spec", "specs/u.spec"}},
		{"a data file", []string{"testdata/fixture.bin"}, specs},
		{"a document", []string{"docs/x.md"}, specs},
		{"go.mod", []string{"go.mod"}, specs},
		{"nothing", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := affectedSpecs(root, specs, tc.changed)
			if err != nil {
				t.Fatalf("affectedSpecs: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("affectedSpecs(%v) =\n  %v\nwant\n  %v", tc.changed, got, tc.want)
			}
		})
	}
}

// TestAffectedSpecs_WhatCannotBeResolvedIsSelectedByAnyChange pins the
// conservative branches: a package whose imports fail to load, and a pkg line
// that is a pattern rather than a directory, are each selected by a change
// the import graph would otherwise rule out.
func TestAffectedSpecs_WhatCannotBeResolvedIsSelectedByAnyChange(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWriteAll(t, filepath.Join(root, "go.mod"), "module m\n\ngo 1.24\n")
	mustWriteAll(t, filepath.Join(root, "e/e.go"), "package e\n\nimport \"m/missing\"\n\nfunc E() int { return missing.M() }\n")
	mustWriteAll(t, filepath.Join(root, "p/p.go"), "package p\n\nfunc P() int { return 1 }\n")
	body := "[m]\nfile p/p.go\n--- anchor\nreturn 1\n--- replace\nreturn 2\n--- end\n"
	mustWriteAll(t, filepath.Join(root, "specs/broken.spec"), "pkg ./e/\n"+body)
	mustWriteAll(t, filepath.Join(root, "specs/pattern.spec"), "pkg ./p/...\n"+body)
	mustWriteAll(t, filepath.Join(root, "specs/plain.spec"), "pkg ./p/\n"+body)
	specs := []string{"specs/broken.spec", "specs/pattern.spec", "specs/plain.spec"}

	got, err := affectedSpecs(root, specs, []string{"z/z.go"})
	if err != nil {
		t.Fatalf("affectedSpecs: %v", err)
	}
	if want := []string{"specs/broken.spec", "specs/pattern.spec"}; !slices.Equal(got, want) {
		t.Errorf("affectedSpecs = %v, want %v", got, want)
	}
}

func TestDepCache_ListsOnlyThisModulesPackages(t *testing.T) {
	t.Parallel()

	root, _ := affectedModule(t)
	d, err := newDepCache(root).get("./a/", "")
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(d.dirs))
	if want := []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("dirs = %v, want %v (standard library and other modules excluded)", got, want)
	}
	if d.wholeTree {
		t.Error("wholeTree set for a package nothing in which runs go or git")
	}
}

func TestCallsGoOrGit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		call string
		want bool
	}{
		{`exec.Command("go", "build")`, true},
		{`exec.Command("git", "status")`, true},
		{`exec.CommandContext(ctx, "go", "list")`, true},
		{`exec.CommandContext(ctx, "git")`, true},
		{`exec.Command("par2", "v")`, false},
		{`exec.CommandContext(ctx, name, "go")`, false},
		{`exec.Command(name, "git")`, false},
		{`exec.CommandContext("go")`, false},
		{`fmt.Println("go")`, false},
	} {
		src := "package p\n\nfunc f() { _ = " + tc.call + " }\n"
		f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		if got := callsGoOrGit(f); got != tc.want {
			t.Errorf("callsGoOrGit(%s) = %v, want %v", tc.call, got, tc.want)
		}
	}
}

// mustWriteAll is mustWrite that creates the parent directories first.
func mustWriteAll(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, body)
}

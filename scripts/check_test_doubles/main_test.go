package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckSource_NewTestDurableProofFlaggedInNormalFile(t *testing.T) {
	src := `package durability

func NewTestDurableProof(jobID string, arts []int32) DurableProof {
	return newProof(jobID, arts)
}
`
	findings, err := checkSource("internal/durability/proof.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.line != 3 {
		t.Errorf("expected finding on line 3, got %d", f.line)
	}
	if !strings.Contains(f.desc, "test double function NewTestDurableProof") {
		t.Errorf("unexpected description: %s", f.desc)
	}
}

func TestCheckSource_NewTestDurableProofWithTestBuildTagNotFlagged(t *testing.T) {
	src := `//go:build test

package durability

func NewTestDurableProof(jobID string, arts []int32) DurableProof {
	return newProof(jobID, arts)
}
`
	findings, err := checkSource("internal/durability/testing.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for file with //go:build test, got %d: %+v", len(findings), findings)
	}
}

func TestCheckSource_TestGoFilesNotFlagged(t *testing.T) {
	src := `package durability

func NewTestDurableProof(jobID string, arts []int32) DurableProof {
	return newProof(jobID, arts)
}
`
	findings, err := checkSource("internal/durability/testing_test.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for *_test.go file, got %d: %+v", len(findings), findings)
	}
}

func TestCheckSource_TestDoubleAllowSuppression(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "line comment on func line",
			src: `package testpkg

func FakeServer() {} //testdouble:allow legitimate test stub
`,
		},
		{
			name: "line comment preceding func",
			src: `package testpkg

//testdouble:allow legitimate test stub
func FakeServer() {}
`,
		},
		{
			name: "inside doc comment",
			src: `package testpkg

// FakeServer creates a fake server.
//
//testdouble:allow legitimate test stub
func FakeServer() {}
`,
		},
		{
			name: "inside func body",
			src: `package testpkg

func FakeServer() {
	//testdouble:allow legitimate test stub
}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := checkSource("internal/testpkg/server.go", []byte(tc.src))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(findings) != 0 {
				t.Errorf("expected 0 findings with suppression, got %d: %+v", len(findings), findings)
			}
		})
	}
}

func TestCheckSource_TestingGoWithoutGoBuildFlagged(t *testing.T) {
	src := `package durability

func RealHelper() {}
`
	findings, err := checkSource("internal/durability/testing.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.line != 1 {
		t.Errorf("expected finding on line 1, got %d", f.line)
	}
	if !strings.Contains(f.desc, "matches *test*.go but lacks a test //go:build tag") {
		t.Errorf("unexpected description: %s", f.desc)
	}
}

func TestCheckSource_TestingGoWithAllowSuppressed(t *testing.T) {
	src := `//testdouble:allow legitimate production helper
package durability

func RealHelper() {}
`
	findings, err := checkSource("internal/durability/testing.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings when testing.go has //testdouble:allow, got %d: %+v", len(findings), findings)
	}
}

func TestCheckSource_MethodTestDouble(t *testing.T) {
	src := `package app

type Server struct{}

func (s *Server) MockStore() {}
`
	findings, err := checkSource("internal/app/server.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.line != 5 {
		t.Errorf("expected line 5, got %d", f.line)
	}
	if !strings.Contains(f.desc, "test double method MockStore") {
		t.Errorf("unexpected description: %s", f.desc)
	}
}

func TestCheckSource_OtherTestBuildTags(t *testing.T) {
	testTags := []string{"integration", "uitest", "crash"}
	for _, tag := range testTags {
		t.Run(tag, func(t *testing.T) {
			src := "//go:build " + tag + "\n\npackage pkg\n\nfunc FakeServer() {}\n"
			findings, err := checkSource("internal/pkg/fake.go", []byte(src))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(findings) != 0 {
				t.Errorf("expected 0 findings for build tag %s, got %d: %+v", tag, len(findings), findings)
			}
		})
	}

	t.Run("non-test tag linux", func(t *testing.T) {
		src := "//go:build linux\n\npackage pkg\n\nfunc FakeServer() {}\n"
		findings, err := checkSource("internal/pkg/fake.go", []byte(src))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(findings) != 1 {
			t.Errorf("expected 1 finding for non-test tag linux, got %d: %+v", len(findings), findings)
		}
	})
}

func TestCheckSource_NormalFunctionsNotFlagged(t *testing.T) {
	src := `package app

func NewApplication() {}
func (a *Application) Added() {}
func ProcessTestResults() {}
`
	findings, err := checkSource("internal/app/app.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for normal code, got %d: %+v", len(findings), findings)
	}
}

func TestCheckFile_ErrorOnMissing(t *testing.T) {
	_, err := checkFile("nonexistent_file.go")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestRun_ExitCodes(t *testing.T) {
	tmpDir := t.TempDir()
	cleanFile := filepath.Join(tmpDir, "clean.go")
	if err := os.WriteFile(cleanFile, []byte("package main\n\nfunc RealMain() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	badFile := filepath.Join(tmpDir, "testing.go")
	if err := os.WriteFile(badFile, []byte("package main\n\nfunc NewTestHelper() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Test checkFile directly on clean and bad file
	findings, err := checkFile(cleanFile)
	if err != nil || len(findings) != 0 {
		t.Fatalf("unexpected cleanFile findings: %v, err: %v", findings, err)
	}

	findings, err = checkFile(badFile)
	if err != nil || len(findings) != 2 {
		t.Fatalf("expected 2 findings on badFile (filename + func), got %d (%v), err: %v", len(findings), findings, err)
	}
}

func TestCheckSource_SuffixMatching(t *testing.T) {
	cases := []struct {
		name string
		fn   string
	}{
		{"ForTesting", "func MakeProofForTesting() {}"},
		{"ForTests", "func NewProofForTests() {}"},
		{"ForTest", "func SetupForTest() {}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package helper\n\n" + tc.fn + "\n"
			findings, err := checkSource("internal/helper/support.go", []byte(src))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(findings) != 1 {
				t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
			}
			if !strings.Contains(findings[0].desc, "test double function") {
				t.Errorf("unexpected desc: %s", findings[0].desc)
			}
		})
	}
}

func TestCheckSource_TypeAndVarDecls(t *testing.T) {
	src := `package helper

type FakeStore struct{}

type MockClient struct{}

var MockProofMaker = func() {}

var StubClock = 123
`
	findings, err := checkSource("internal/helper/support.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 4 {
		t.Fatalf("expected 4 findings for types and vars, got %d: %+v", len(findings), findings)
	}
	expected := []string{
		"test double type FakeStore",
		"test double type MockClient",
		"test double var MockProofMaker",
		"test double var StubClock",
	}
	for i, exp := range expected {
		if !strings.Contains(findings[i].desc, exp) {
			t.Errorf("finding %d: expected %q, got %q", i, exp, findings[i].desc)
		}
	}
}

func TestCheckSource_TypeAndVarSuppression(t *testing.T) {
	src := `package helper

// FakeStore is allowed.
//
//testdouble:allow legitimate in-memory store
type FakeStore struct{}

//testdouble:allow legitimate mock variable
var MockProofMaker = func() {}
`
	findings, err := checkSource("internal/helper/support.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings when suppressed with reason, got %d: %+v", len(findings), findings)
	}
}

func TestCheckSource_BareSuppressionRequiresReason(t *testing.T) {
	src := `package helper

//testdouble:allow
func FakeServer() {}
`
	findings, err := checkSource("internal/helper/support.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expect 2 findings: bare suppression error AND unsuppressed test double
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings (bare marker error + unsuppressed func), got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].desc, "requires a reason") {
		t.Errorf("expected reason requirement error, got: %s", findings[0].desc)
	}
	if !strings.Contains(findings[1].desc, "test double function FakeServer") {
		t.Errorf("expected test double finding, got: %s", findings[1].desc)
	}
}

func TestCheckSource_NonTestBuildTagDoesNotExemptFilename(t *testing.T) {
	src := `//go:build !ignore_autogenerated

package durability

func RealHelper() {}
`
	findings, err := checkSource("internal/durability/testing.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for non-test build tag on *test*.go file, got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].desc, "lacks a test //go:build tag") {
		t.Errorf("unexpected desc: %s", findings[0].desc)
	}
}

func TestCheckSource_CompoundBuildConstraints(t *testing.T) {
	cases := []struct {
		tag      string
		expected int
	}{
		{"test || linux", 1},       // linux allows production compile, so not test-only!
		{"test && linux", 0},       // requires test, so test-only
		{"test || integration", 0}, // both are test tags
		{"!test", 1},               // compiles when NOT test!
	}

	for _, tc := range cases {
		t.Run(tc.tag, func(t *testing.T) {
			src := "//go:build " + tc.tag + "\n\npackage pkg\n\nfunc FakeServer() {}\n"
			findings, err := checkSource("internal/pkg/fake.go", []byte(src))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(findings) != tc.expected {
				t.Errorf("tag %q: expected %d findings, got %d: %+v", tc.tag, tc.expected, len(findings), findings)
			}
		})
	}
}

func TestCheckSource_FileLevelSuppressionScope(t *testing.T) {
	// Comment inside function body must NOT exempt filename check
	src := `package durability

func RealHelper() {
	//testdouble:allow inside function body
}
`
	findings, err := checkSource("internal/durability/testing.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding because suppression is not before package, got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].desc, "matches *test*.go but lacks a test //go:build tag") {
		t.Errorf("unexpected desc: %s", findings[0].desc)
	}
}

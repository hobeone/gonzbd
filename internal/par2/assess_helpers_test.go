package par2

import (
	"log/slog"
	"testing"
)

// assessAndApply is the composition production uses: assess the directory,
// then act on what the assessment reported.
//
// It exists so relocation tests exercise the real two-step shape rather than a
// single call that hides it. The pair replaced a QuickCheck function that did
// both at once — and doing both at once was the defect, because it meant the
// verdict had to be computed after the moves it depended on (#494).
//
// No assembled CRCs are supplied. These tests are about which file is which
// and where it ends up, not whether it is intact; verification against real
// CRCs is assess_test.go's subject.
func assessAndApply(t *testing.T, dir string, sets []Set, log *slog.Logger) ([]Rename, error) {
	t.Helper()
	a, err := Assess(dir, sets, nil, log)
	if err != nil {
		return nil, err
	}
	return ApplyRenames(dir, a, log), nil
}

package app

import (
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/par2"
)

// mustAssess takes the observation recordPar2Names acts on.
//
// It is a separate call in these tests because it is a separate call in
// production: par2.Assess answers what is in the directory BEFORE anything
// moves, and recordPar2Names then applies and records the moves that answer
// implies. Collapsing the two back into one helper would hide the ordering
// this change exists to make unbreakable (#494) — and a helper that assessed
// and recorded together would let a test pass against code that verified after
// renaming, which is the defect itself.
//
// No assembled CRCs are supplied. These tests are about which file ends up
// where and what the queue records, not about whether it is intact.
func mustAssess(t *testing.T, dir string, sets []par2.Set, log *slog.Logger) par2.Assessment {
	t.Helper()
	a, err := par2.AssessWithOptions(dir, sets, nil, log, par2.DefaultParseOptions())
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	return a
}

package par2

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// A par2 set posted twice under different stems parses to one SetID and slice
// size. The copies are merged by FileID, so each entry appears once and none
// is left Unaccounted as the other copy's duplicate.
func TestCollectManifests_MergesCopiesOfOneSet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, b := payload(1, 20*1024), payload(2, 20*1024)
	par2SetFor(t, dir, map[string][]byte{"a.rar": a, "b.rar": b})
	writeFile(t, dir, "a.rar", a)
	writeFile(t, dir, "b.rar", b)

	original, err := os.ReadFile(filepath.Join(dir, "set.par2"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "other.par2", original)

	sets, err := FindPar2Files(dir)
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}
	if len(sets) != 2 {
		t.Fatalf("FindPar2Files returned %d sets, want 2 (fixture guard)", len(sets))
	}
	log := slog.New(slog.DiscardHandler)

	c := collectManifestsWithOptions(sets, log, DefaultParseOptions())
	if len(c.entries) != 2 || len(c.sets) != 1 || len(c.undescribed) != 0 {
		t.Errorf("collected %d entries, %d set infos and %d undescribed, want 2, 1 and 0",
			len(c.entries), len(c.sets), len(c.undescribed))
	}

	id, err := IdentifyWithOptions(dir, sets, log, DefaultParseOptions())
	if err != nil {
		t.Fatalf("IdentifyWithOptions: %v", err)
	}
	if len(id.Unaccounted) != 0 {
		t.Errorf("Unaccounted = %v, want none", id.Unaccounted)
	}
	if _, ok := id.SliceSize(); !ok {
		t.Error("SliceSize not ok for a duplicated set")
	}

	as, err := AssessWithOptions(dir, sets, nil, log, DefaultParseOptions())
	if err != nil {
		t.Fatalf("AssessWithOptions: %v", err)
	}
	if len(as.ID.Unaccounted) != 0 {
		t.Errorf("Assess Unaccounted = %v, want none", as.ID.Unaccounted)
	}
	if _, ok := as.ID.SliceSize(); !ok {
		t.Error("Assess SliceSize not ok for a duplicated set")
	}
}

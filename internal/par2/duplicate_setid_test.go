package par2

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// A par2 set posted twice under different stems parses to one SetID. Collecting
// both would give every entry twice, and the copy of each would be Unaccounted.
func TestCollectManifests_SkipsASetIDAlreadyParsed(t *testing.T) {
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

	manifest, infos := collectManifestsWithOptions(sets, log, DefaultParseOptions())
	if len(manifest) != 2 || len(infos) != 1 {
		t.Errorf("collected %d entries and %d set infos, want 2 and 1", len(manifest), len(infos))
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

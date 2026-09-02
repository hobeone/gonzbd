package par2

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestIsIgnoredForIdentification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"movie.part1.rar", false},
		{"data.bin", false},
		{"7xq6N6P340dCh9Lnih5hY3jsArfSN1", false}, // no extension at all
		{"set.par2", true},
		{"set.vol000+01.PAR2", true}, // matching is case-insensitive
		{"movie.nfo", true},
		{"movie.NFO", true},
		{"movie.sfv", true},
		// Only the final extension counts, so a par2-looking stem with a real
		// extension is still a candidate.
		{"movie.par2.rar", false},
	} {
		if got := isIgnoredForIdentification(tc.name); got != tc.want {
			t.Errorf("isIgnoredForIdentification(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMatchMethod_String(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		m    MatchMethod
		want string
	}{
		{MatchName, "name"},
		{MatchFlattenedName, "flattened-name"},
		{MatchHash16k, "hash16k"},
		{MatchCRC32, "crc32"},
		{MatchMethod(99), "MatchMethod(99)"},
	} {
		if got := tc.m.String(); got != tc.want {
			t.Errorf("MatchMethod(%d).String() = %q, want %q", uint8(tc.m), got, tc.want)
		}
	}
}

// TestJoinKey pins the key Assess joins identification to assembled CRCs on.
//
// It is the basename, and the case that forces it is a file a PREVIOUS run
// already relocated: identification answers "Screens/shot.jpg" while the
// caller still knows the file as "shot.jpg". Joining on the full path would
// miss it and report an intact file unverified.
func TestJoinKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"shot.jpg", "shot.jpg"},
		{"Screens/shot.jpg", "shot.jpg"},
		{"a/b/c.txt", "c.txt"},
		{"Screens_shot.jpg", "Screens_shot.jpg"},
	} {
		if got := joinKey(tc.in); got != tc.want {
			t.Errorf("joinKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCollectManifestsWithOptions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	sets := par2SetFor(t, dir, map[string][]byte{
		"a.rar": payload(40, 2048),
		"b.rar": payload(41, 2048),
	})

	got := collectManifestsWithOptions(sets, slog.New(slog.DiscardHandler), DefaultParseOptions())
	if len(got) != 2 {
		t.Fatalf("collected %d descriptions, want 2", len(got))
	}
	seen := map[string]bool{}
	for _, fd := range got {
		seen[fd.FileName] = true
	}
	if !seen["a.rar"] || !seen["b.rar"] {
		t.Errorf("collected %v, want both a.rar and b.rar", seen)
	}

	// A set whose main file cannot be parsed is skipped with a warning rather
	// than aborting the others.
	bad := filepath.Join(dir, "broken.par2")
	if err := os.WriteFile(bad, []byte("not a par2 file at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	badSets, err := FindPar2Files(dir)
	if err != nil {
		t.Fatalf("FindPar2Files: %v", err)
	}
	if got := collectManifestsWithOptions(badSets, slog.New(slog.DiscardHandler), DefaultParseOptions()); len(got) != 2 {
		t.Errorf("an unparseable set changed the collected count to %d, want 2", len(got))
	}

	if got := collectManifestsWithOptions(nil, slog.New(slog.DiscardHandler), DefaultParseOptions()); len(got) != 0 {
		t.Errorf("collected %d descriptions from no sets", len(got))
	}
}

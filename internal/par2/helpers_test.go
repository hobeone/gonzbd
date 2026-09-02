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

func TestMatchBasenameAndFlattened(t *testing.T) {
	t.Parallel()

	index := map[string]*par2Entry{
		"shot.jpg":         {desc: FileDesc{FileName: "Screens/shot.jpg"}},
		"Screens_shot.jpg": {desc: FileDesc{FileName: "Screens_shot.jpg"}},
	}

	// matchBasename keys on the assembled file's own name.
	if entry, ok := matchBasename(AssembledFile{FileName: "shot.jpg"}, index); !ok {
		t.Error("matchBasename missed an exact name")
	} else if entry.desc.FileName != "Screens/shot.jpg" {
		t.Errorf("matchBasename returned %q", entry.desc.FileName)
	}
	if _, ok := matchBasename(AssembledFile{FileName: "absent.jpg"}, index); ok {
		t.Error("matchBasename matched a name that is not in the index")
	}

	// matchFlattened reduces a PATH to its basename before looking up, so an
	// assembled file already carrying subdirectory components still resolves.
	if entry, ok := matchFlattened(AssembledFile{FileName: "Screens/shot.jpg"}, index); !ok {
		t.Error("matchFlattened missed a path whose basename is indexed")
	} else if entry.desc.FileName != "Screens/shot.jpg" {
		t.Errorf("matchFlattened returned %q", entry.desc.FileName)
	}
	if _, ok := matchFlattened(AssembledFile{FileName: "Other/absent.jpg"}, index); ok {
		t.Error("matchFlattened matched a basename that is not in the index")
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

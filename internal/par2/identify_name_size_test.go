package par2

import "testing"

// TestIdentify_NameMatchDeclinesAFileOfADifferentLength pins the length check
// on pass 1, the same rule passes 0, 2 and 3 apply.
//
// A file whose name matches but which is one byte longer than the par2 packet
// records (a stale tail) must not be identified. If it were, verifyIdentified
// would compare the assembled CRC, which is settled from written-article rows
// and cannot see bytes past the last row, find a match, and quickcheck would
// pass without par2 ever running. Declined, the entry is Unaccounted, which
// counts as Unverified and takes the existing fetch-and-repair path.
func TestIdentify_NameMatchDeclinesAFileOfADifferentLength(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		par2   string
		onDisk string
	}{
		{"basename", "movie.mkv", "movie.mkv"},
		{"flattened name", "Sub/movie.mkv", "Sub_movie.mkv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()

			body := payload(5, 40*1024)
			sets := par2SetFor(t, dir, map[string][]byte{tc.par2: body})
			writeFile(t, dir, tc.onDisk, append(append([]byte{}, body...), 0xFF))

			id := identifyIn(t, dir, sets)

			if len(id.Files) != 0 {
				t.Errorf("identified %+v; a name match one byte longer than the par2 length must not be claimed", id.Files)
			}
			if id.Accounted() || len(id.Unaccounted) != 1 {
				t.Errorf("Accounted() = %v, unaccounted = %d, want the entry unaccounted so it counts as Unverified",
					id.Accounted(), len(id.Unaccounted))
			}
		})
	}
}

// TestIdentify_NameMatchStillClaimsAFileOfTheRightLength is the control: the
// same fixture at the recorded length is claimed, by name.
func TestIdentify_NameMatchStillClaimsAFileOfTheRightLength(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	body := payload(5, 40*1024)
	sets := par2SetFor(t, dir, map[string][]byte{"movie.mkv": body})
	writeFile(t, dir, "movie.mkv", body)

	id := identifyIn(t, dir, sets)

	f, ok := byOnDisk(id, "movie.mkv")
	if !ok || f.By != MatchName {
		t.Fatalf("movie.mkv not claimed by name: %+v", id.Files)
	}
}

package par2

import (
	"hash/crc32"
	"log/slog"
	"testing"
)

// nameLengthCases are the two ways a name-matched file differs from the length
// its par2 packet records: a stale tail past the true end (#837), and the more
// common short file whose last article failed. Each runs under both name forms
// pass 1 matches.
var nameLengthCases = []struct {
	name   string
	par2   string
	onDisk string
	resize func(body []byte) []byte
}{
	{"basename, longer", "movie.mkv", "movie.mkv", withStaleTail},
	{"basename, shorter", "movie.mkv", "movie.mkv", withoutLastByte},
	{"flattened name, longer", "Sub/movie.mkv", "Sub_movie.mkv", withStaleTail},
	{"flattened name, shorter", "Sub/movie.mkv", "Sub_movie.mkv", withoutLastByte},
}

func withStaleTail(body []byte) []byte   { return append(append([]byte{}, body...), 0xFF) }
func withoutLastByte(body []byte) []byte { return append([]byte{}, body[:len(body)-1]...) }

// TestIdentify_NameMatchOfADifferentLengthIsIdentified pins that a name match
// is claimed whatever its length.
//
// The file is that entry, damaged; declining it would leave the entry
// Unaccounted, and where it is the set's only entry that makes
// NothingIdentified() true, which app.par2Verdict reads as a possible Layout B
// post and holds the recovery volumes instead of fetching them.
func TestIdentify_NameMatchOfADifferentLengthIsIdentified(t *testing.T) {
	t.Parallel()

	for _, tc := range nameLengthCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()

			body := payload(5, 40*1024)
			sets := par2SetFor(t, dir, map[string][]byte{tc.par2: body})
			writeFile(t, dir, tc.onDisk, tc.resize(body))

			id := identifyIn(t, dir, sets)

			if _, ok := byOnDisk(id, tc.onDisk); !ok {
				t.Errorf("%s not identified: files %+v, unaccounted %+v", tc.onDisk, id.Files, id.Unaccounted)
			}
			if id.NothingIdentified() || !id.Accounted() {
				t.Errorf("NothingIdentified() = %v, Accounted() = %v; want false and true, so the job fetches "+
					"recovery volumes rather than holding them", id.NothingIdentified(), id.Accounted())
			}
		})
	}
}

// TestAssess_NameMatchOfADifferentLengthIsUnverified pins that such a file
// never verifies clean, even where the CRC settled from its rows equals the
// packet's: the rows cannot see a stale tail past the last article, so the CRC
// says nothing about the length.
func TestAssess_NameMatchOfADifferentLengthIsUnverified(t *testing.T) {
	t.Parallel()

	for _, tc := range nameLengthCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()

			body := payload(5, 40*1024)
			sets := par2SetForWithCRC(t, dir, map[string][]byte{tc.par2: body})
			writeFile(t, dir, tc.onDisk, tc.resize(body))

			a, err := Assess(dir, sets,
				[]AssembledFile{{FileName: tc.onDisk, CRC32: crc32.ChecksumIEEE(body)}},
				slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("Assess: %v", err)
			}
			if a.CRC.Matched != 0 || a.CRC.Unverified != 1 {
				t.Errorf("Matched = %d, Unverified = %d; want 0 and 1, a wrong length must not verify clean",
					a.CRC.Matched, a.CRC.Unverified)
			}
			if a.CRC.NotInPar2 != 0 {
				t.Errorf("NotInPar2 = %d; the delivered file was joined to its entry, want 0", a.CRC.NotInPar2)
			}
		})
	}
}

// TestAssess_NameMatchOfTheRecordedLengthVerifies is the control: the same
// fixture at the recorded length is claimed by name and its CRC compared.
func TestAssess_NameMatchOfTheRecordedLengthVerifies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	body := payload(5, 40*1024)
	sets := par2SetForWithCRC(t, dir, map[string][]byte{"movie.mkv": body})
	writeFile(t, dir, "movie.mkv", body)

	a, err := Assess(dir, sets,
		[]AssembledFile{{FileName: "movie.mkv", CRC32: crc32.ChecksumIEEE(body)}},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if f, ok := byOnDisk(a.ID, "movie.mkv"); !ok || f.By != MatchName {
		t.Fatalf("movie.mkv not claimed by name: %+v", a.ID.Files)
	}
	if a.CRC.Matched != 1 || a.CRC.Unverified != 0 {
		t.Errorf("Matched = %d, Unverified = %d; want 1 and 0", a.CRC.Matched, a.CRC.Unverified)
	}
}

// TestAssess_NameMatchOfAnEntryWithNoRecordedLength pins the FileSize > 0
// guard: a packet recording a length of 0 is not compared against, as in
// passes 0 and 2 and relocateFile, so a non-empty file under that name is not
// counted a length mismatch. With no IFSC data it is NoCRC.
func TestAssess_NameMatchOfAnEntryWithNoRecordedLength(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	sets := par2SetFor(t, dir, map[string][]byte{"empty.dat": nil})
	writeFile(t, dir, "empty.dat", []byte("not empty"))

	a, err := Assess(dir, sets,
		[]AssembledFile{{FileName: "empty.dat", CRC32: 0x12345678}},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if _, ok := byOnDisk(a.ID, "empty.dat"); !ok {
		t.Fatalf("empty.dat not identified: %+v", a.ID)
	}
	if a.CRC.Unverified != 0 || a.CRC.NoCRC != 1 {
		t.Errorf("Unverified = %d, NoCRC = %d; want 0 and 1, a zero recorded length is not compared",
			a.CRC.Unverified, a.CRC.NoCRC)
	}
}

package durability

import (
	"math"
	"strings"
	"testing"
)

// goodSeedColumns is one job_files row as the SeedRows query scans it:
// file_index, subject, bytes, article_count, filename, complete, fetch_policy,
// recorded.
func goodSeedColumns() [8]any {
	return [8]any{int64(2), "s", int64(100), int64(3), "f.bin", int64(1), int64(2), int64(3)}
}

// TestSeedRowOf_AcceptsARowOfItsTypes pins the column mapping: every field of
// the SeedRow comes from the column the query puts in that position.
func TestSeedRowOf_AcceptsARowOfItsTypes(t *testing.T) {
	t.Parallel()
	got, err := seedRowOf(goodSeedColumns())
	if err != nil {
		t.Fatalf("seedRowOf: %v", err)
	}
	want := SeedRow{FileIndex: 2, Subject: "s", Bytes: 100, ArticleCount: 3, Complete: true, FetchPolicy: 2, Filename: "f.bin", Recorded: 3}
	if got != want {
		t.Errorf("seedRowOf = %+v, want %+v", got, want)
	}
}

// TestSeedRowOf_RefusesAValueOutsideItsType pins that a column holding the
// wrong Go type, a negative count, a count past its type's range, a NULL
// filename or an unknown fetch policy makes the row unreadable, naming the
// column, rather than a zero or truncated field.
func TestSeedRowOf_RefusesAValueOutsideItsType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		col  int
		v    any
		want string
	}{
		{"non-integer file_index", 0, "2", "file_index"},
		{"non-string subject", 1, nil, "subject"},
		{"negative bytes", 2, int64(-1), "bytes"},
		{"article_count past int32", 3, int64(math.MaxInt32) + 1, "article_count"},
		{"NULL filename", 4, nil, "filename"},
		{"negative complete", 5, int64(-1), "complete"},
		{"fetch_policy 3", 6, int64(3), "fetch_policy"},
		{"recorded past int32", 7, int64(math.MaxInt32) + 1, "recorded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cols := goodSeedColumns()
			cols[c.col] = c.v
			_, err := seedRowOf(cols)
			if err == nil {
				t.Fatal("seedRowOf accepted the row")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name column %s", err, c.want)
			}
		})
	}
}

package job

import (
	"math/rand/v2"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

func wr(off, n int64) durability.WrittenRow { return durability.WrittenRow{Offset: off, Length: n} }

func TestDamagedSlices(t *testing.T) {
	tests := []struct {
		name        string
		rows        []durability.WrittenRow
		size, slice uint64
		want        int
	}{
		{"tail of the only slice missing", []durability.WrittenRow{wr(5, 5)}, 10, 10, 1},
		{"gap, then a partial tail slice", []durability.WrittenRow{wr(0, 5), wr(12, 8)}, 25, 10, 3},
		{"shared slice counted once, not from zero", []durability.WrittenRow{wr(0, 5), wr(10, 20)}, 30, 10, 1},
		{"one row spanning several slices", []durability.WrittenRow{wr(0, 30)}, 30, 10, 0},
		{"overlapping rows cover the file", []durability.WrittenRow{wr(0, 15), wr(10, 20)}, 30, 10, 0},
		{"overlap does not hide a later gap", []durability.WrittenRow{wr(0, 20), wr(5, 5), wr(25, 5)}, 30, 10, 1},
		{"exact single slice", []durability.WrittenRow{wr(0, 10)}, 10, 10, 0},
		{"no rows", nil, 25, 10, 3},
		{"row past the end after a gap", []durability.WrittenRow{wr(0, 10), wr(200, 5)}, 25, 10, 2},
		{"row straddling the end", []durability.WrittenRow{wr(0, 10), wr(10, 100)}, 25, 10, 0},
		{"zero-length row covers nothing", []durability.WrittenRow{wr(0, 0)}, 10, 10, 1},
		{"zero slice size", []durability.WrittenRow{wr(0, 5)}, 10, 0, 0},
		{"zero file size", nil, 0, 10, 0},
		{"gap inside one slice", []durability.WrittenRow{wr(0, 3), wr(6, 4), wr(10, 10)}, 20, 10, 1},
		{"two gaps in one slice count it once", []durability.WrittenRow{wr(0, 2), wr(3, 2), wr(10, 10)}, 20, 10, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DamagedSlices(tc.rows, tc.size, tc.slice); got != tc.want {
				t.Errorf("DamagedSlices = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDamagedSlices_MatchesByteSetCount(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 982))
	for iter := range 5000 {
		f := rng.IntN(60) + 1
		s := rng.IntN(12) + 1
		nrows := rng.IntN(7)
		rows := make([]durability.WrittenRow, 0, nrows)
		for range nrows {
			rows = append(rows, wr(int64(rng.IntN(f+10)), int64(rng.IntN(20))))
		}
		sortRows(rows)

		covered := make([]bool, f)
		for _, r := range rows {
			for b := r.Offset; b < r.Offset+r.Length && b < int64(f); b++ {
				covered[b] = true
			}
		}
		want := 0
		for lo := 0; lo < f; lo += s {
			for b := lo; b < min(lo+s, f); b++ {
				if !covered[b] {
					want++
					break
				}
			}
		}
		if got := DamagedSlices(rows, uint64(f), uint64(s)); got != want {
			t.Fatalf("iter %d: rows=%v F=%d S=%d: got %d, want %d", iter, rows, f, s, got, want)
		}
	}
}

func sortRows(rows []durability.WrittenRow) {
	for i := 1; i < len(rows); i++ {
		for k := i; k > 0 && durability.CompareWrittenRows(rows[k-1], rows[k]) > 0; k-- {
			rows[k-1], rows[k] = rows[k], rows[k-1]
		}
	}
}

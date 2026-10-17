package job

import (
	"math"
	"slices"
)

// cheapestCover returns the indices of a subset of the candidates whose blocks
// sum to at least need, with the fewest total bytes. Any cover of minimal cost
// may be returned. blocks and bytes are parallel; a candidate with blocks <= 0
// is never chosen.
//
// ok is false when the candidates cannot reach need together; the cover is then
// nil. need <= 0 is covered by the empty set, so it returns (nil, true).
//
// It is a 0/1 knapsack over block totals capped at need: time is
// O(len(blocks) * need), and the choice table is one bit per cell.
// ReleaseRecovery bounds need. Each count is capped at need first, which leaves
// the cover exact (one volume of need blocks already covers it) and keeps the
// sums below from overflowing for any count.
func cheapestCover(inBlocks []int, bytes []int64, need int) (cover []int, ok bool) {
	if need <= 0 {
		return nil, true
	}
	blocks := make([]int, len(inBlocks))
	var total int
	for i, b := range inBlocks {
		blocks[i] = min(max(b, 0), need)
		total += blocks[i]
	}
	if total < need {
		return nil, false
	}

	width := need + 1
	best := make([]int64, width)
	for c := 1; c < width; c++ {
		best[c] = math.MaxInt64
	}
	// Bit v*width+nc is set when volume v improved best[nc]. Below the cap the
	// total it came from is nc-blocks[v]; only the capped total need can be
	// reached from several, so intoNeed[v] records which one v used.
	took := make([]uint64, (len(blocks)*width+63)/64)
	intoNeed := make([]int32, len(blocks))
	for v, b := range blocks {
		if b <= 0 {
			continue
		}
		// Downward, so best[c] is still the value from before volume v.
		for c := need; c >= 0; c-- {
			if best[c] == math.MaxInt64 {
				continue
			}
			nc := min(need, c+b)
			if cost := best[c] + bytes[v]; cost < best[nc] {
				best[nc] = cost
				bit := v*width + nc
				took[bit/64] |= 1 << (bit % 64)
				if nc == need {
					intoNeed[v] = int32(c) //nolint:gosec // c <= need, which ReleaseRecovery bounds at MaxPar2Slices
				}
			}
		}
	}

	c := need
	for v := range slices.Backward(blocks) {
		if bit := v*width + c; took[bit/64]&(1<<(bit%64)) != 0 {
			cover = append(cover, v)
			if c == need {
				c = int(intoNeed[v])
			} else {
				c -= blocks[v]
			}
		}
	}
	return cover, true
}

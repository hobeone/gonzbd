package job

import (
	"math"
	"math/rand/v2"
	"testing"
)

func coverCost(idxs []int, blocks []int, bytes []int64) (int, int64) {
	var b int
	var c int64
	for _, i := range idxs {
		b += blocks[i]
		c += bytes[i]
	}
	return b, c
}

func TestCheapestCover(t *testing.T) {
	blocks := []int{1, 2, 4, 8, 16}
	bytes := make([]int64, len(blocks))
	for i, b := range blocks {
		bytes[i] = int64(b) * 1000
	}
	t.Run("need 9 picks 8 and 1", func(t *testing.T) {
		got, ok := cheapestCover(blocks, bytes, 9)
		if !ok {
			t.Fatal("ok = false")
		}
		if _, c := coverCost(got, blocks, bytes); c != 9000 {
			t.Errorf("cover %v costs %d, want 9000", got, c)
		}
	})
	t.Run("need 0 is empty and ok", func(t *testing.T) {
		got, ok := cheapestCover(blocks, bytes, 0)
		if !ok || len(got) != 0 {
			t.Errorf("got %v, %v", got, ok)
		}
	})
	t.Run("negative need is empty and ok", func(t *testing.T) {
		got, ok := cheapestCover(blocks, bytes, -3)
		if !ok || len(got) != 0 {
			t.Errorf("got %v, %v", got, ok)
		}
	})
	t.Run("need above total is not ok", func(t *testing.T) {
		got, ok := cheapestCover(blocks, bytes, 32)
		if ok || got != nil {
			t.Errorf("got %v, %v", got, ok)
		}
	})
	t.Run("need equal to total takes all", func(t *testing.T) {
		got, ok := cheapestCover(blocks, bytes, 31)
		if !ok || len(got) != 5 {
			t.Errorf("got %v, %v", got, ok)
		}
	})
	t.Run("blocks <= 0 never chosen", func(t *testing.T) {
		got, ok := cheapestCover([]int{0, -3, 2}, []int64{0, 0, 5}, 2)
		if !ok || len(got) != 1 || got[0] != 2 {
			t.Errorf("got %v, %v", got, ok)
		}
		if _, ok := cheapestCover([]int{0, -3}, []int64{0, 0}, 1); ok {
			t.Error("ok with no positive blocks")
		}
	})
	t.Run("a count beyond int range does not overflow", func(t *testing.T) {
		got, ok := cheapestCover([]int{math.MaxInt, math.MaxInt, 3}, []int64{300, 200, 100}, 5)
		if !ok || len(got) != 1 || got[0] != 1 {
			t.Errorf("got %v, %v, want [1], true", got, ok)
		}
	})
}

func TestCheapestCover_MatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 982))
	for iter := range 3000 {
		n := rng.IntN(8) + 1
		blocks := make([]int, n)
		bytes := make([]int64, n)
		total := 0
		for i := range n {
			blocks[i] = rng.IntN(9)
			bytes[i] = int64(rng.IntN(50))
			total += blocks[i]
		}
		need := rng.IntN(total + 3)

		bestCost := int64(-1)
		for mask := range 1 << n {
			var b int
			var c int64
			for i := range n {
				if mask&(1<<i) != 0 {
					b += blocks[i]
					c += bytes[i]
				}
			}
			if b >= need && (bestCost < 0 || c < bestCost) {
				bestCost = c
			}
		}

		got, ok := cheapestCover(blocks, bytes, need)
		if (bestCost >= 0) != ok {
			t.Fatalf("iter %d: blocks=%v need=%d ok=%v but brute-force feasible=%v", iter, blocks, need, ok, bestCost >= 0)
		}
		if !ok {
			continue
		}
		seen := map[int]bool{}
		for _, i := range got {
			if seen[i] {
				t.Fatalf("iter %d: index %d repeated in %v", iter, i, got)
			}
			seen[i] = true
		}
		b, c := coverCost(got, blocks, bytes)
		if b < need || c != bestCost {
			t.Fatalf("iter %d: blocks=%v bytes=%v need=%d: cover %v has blocks=%d cost=%d, want blocks>=%d cost=%d",
				iter, blocks, bytes, need, got, b, c, need, bestCost)
		}
	}
}

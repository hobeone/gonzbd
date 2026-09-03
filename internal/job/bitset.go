package job

// bitset is a compact fixed-size bit vector for per-article flags.
//
// It replaces []bool, which spends a byte per bit. At 20,000 articles that
// is 60 KB across done/failed/emitted where 7.5 KB suffices, and a job
// holds that for as long as it is in the queue.
//
// n is stored explicitly rather than derived from len(words) because the
// last word is padded: a 70-bit set occupies two words, and without n the
// trailing 58 bits would be indistinguishable from real articles.
type bitset struct {
	words []uint64
	n     int
}

func newBitset(n int) bitset {
	if n < 0 {
		n = 0
	}
	return bitset{words: make([]uint64, (n+63)/64), n: n}
}

// Len returns the number of bits, not the number of words.
func (b bitset) Len() int { return b.n }

func (b bitset) Get(i int) bool {
	if i < 0 || i >= b.n {
		return false
	}
	return b.words[i/64]&(1<<(uint(i)%64)) != 0
}

// Set and Clear take a value receiver but still mutate the caller's bitset:
// words is a slice, so the receiver copy shares the same backing array.
// Only a reassignment of b.words itself (never done here) would need a
// pointer receiver to be visible to the caller.
func (b bitset) Set(i int) {
	if i < 0 || i >= b.n {
		return
	}
	b.words[i/64] |= 1 << (uint(i) % 64)
}

func (b bitset) Clear(i int) {
	if i < 0 || i >= b.n {
		return
	}
	b.words[i/64] &^= 1 << (uint(i) % 64)
}

// Clone returns an independent copy. bitset is a value type holding a
// slice, so a plain assignment would alias the backing array.
func (b bitset) Clone() bitset {
	w := make([]uint64, len(b.words))
	copy(w, b.words)
	return bitset{words: w, n: b.n}
}

// ToBools renders the set as []bool for the on-disk JSON shape, which is
// deliberately unchanged.
func (b bitset) ToBools() []bool {
	out := make([]bool, b.n)
	for i := range b.n {
		out[i] = b.Get(i)
	}
	return out
}

// bitsetFromBools builds a bitset from the on-disk []bool shape.
func bitsetFromBools(in []bool) bitset {
	b := newBitset(len(in))
	for i, v := range in {
		if v {
			b.Set(i)
		}
	}
	return b
}

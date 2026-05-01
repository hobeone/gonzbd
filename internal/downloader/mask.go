package downloader

// serverMask is a highly optimized, allocation-free bitmask used to track
// which servers have been tried for a given article.
//
// Usenet setups rarely exceed a few servers, and almost never exceed 64.
// By backing the primary state with a single uint64 bitmask, we completely
// eliminate heap allocations for the try-list in 99.9% of cases.
//
// For the pathological case where a user has > 64 servers, it falls back
// to allocating a dynamic slice.
//
// Because it is used as a value type in a map (map[string]serverMask),
// mutation requires a copy-update-replace pattern:
//
//	mask := tryList[key]
//	mask.set(serverIdx)
//	tryList[key] = mask
type serverMask struct {
	fast [1]uint64 // bits 0-63
	slow []uint64  // bits 64+ (allocated only if needed)
}

// set marks the server at index idx as tried.
func (m *serverMask) set(idx int) {
	if idx < 64 {
		m.fast[0] |= 1 << idx
		return
	}
	slowIdx := (idx / 64) - 1
	if len(m.slow) <= slowIdx {
		// Reallocate slow slice if needed
		newSlow := make([]uint64, slowIdx+1)
		copy(newSlow, m.slow)
		m.slow = newSlow
	}
	m.slow[slowIdx] |= 1 << (idx % 64)
}

// has reports whether the server at index idx has been tried.
func (m *serverMask) has(idx int) bool {
	if idx < 64 {
		return (m.fast[0] & (1 << idx)) != 0
	}
	slowIdx := (idx / 64) - 1
	if len(m.slow) <= slowIdx {
		return false
	}
	return (m.slow[slowIdx] & (1 << (idx % 64))) != 0
}

// unset unmarks the server at index idx.
func (m *serverMask) unset(idx int) {
	if idx < 64 {
		m.fast[0] &^= 1 << idx
		return
	}
	slowIdx := (idx / 64) - 1
	if len(m.slow) <= slowIdx {
		return
	}
	m.slow[slowIdx] &^= 1 << (idx % 64)
}

// isEmpty reports whether no servers have been marked as tried.
func (m *serverMask) isEmpty() bool {
	if m.fast[0] != 0 {
		return false
	}
	for _, v := range m.slow {
		if v != 0 {
			return false
		}
	}
	return true
}

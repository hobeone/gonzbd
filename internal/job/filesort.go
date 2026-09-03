package job

import (
	"slices"
	"strings"

	"github.com/hobeone/gonzbd/internal/rarheader"
)

// rarSortKey returns the sort key for a JobFile.
// RAR volumes sort first (tier 0), ordered by (setname, volume).
// All other files keep their relative document order (tier 1).
type rarSortKey struct {
	tier    int
	setname string
	vol     int
}

func fileRarSortKey(f JobFile) rarSortKey {
	setname, vol := rarheader.AnalyzeRarFilename(f.Subject)
	if setname != "" {
		return rarSortKey{tier: 0, setname: setname, vol: vol}
	}
	return rarSortKey{tier: 1}
}

// SortJobFiles reorders files so that RAR volumes appear first, sorted by
// (set name, volume number). Non-RAR files retain their relative document
// order. The sort is stable.
func SortJobFiles(files []JobFile) {
	slices.SortStableFunc(files, func(a, b JobFile) int {
		ka, kb := fileRarSortKey(a), fileRarSortKey(b)
		if ka.tier != kb.tier {
			return ka.tier - kb.tier
		}
		if ka.tier == 0 {
			if ka.setname != kb.setname {
				return strings.Compare(ka.setname, kb.setname)
			}
			return ka.vol - kb.vol
		}
		return 0 // both non-RAR: stable preserves document order
	})
}

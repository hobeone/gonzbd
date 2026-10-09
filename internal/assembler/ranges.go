package assembler

import "sort"

// Range is a half-open byte range [Off, Off+Len) in a target file.
type Range struct{ Off, Len int64 }

func (r Range) end() int64 { return r.Off + r.Len }

func (r Range) intersects(s Range) bool {
	return r.Len > 0 && s.Len > 0 && r.Off < s.end() && s.Off < r.end()
}

type ownedRange struct {
	r  Range
	id articleID
}

// ownedRanges records which article owns each written byte range of one file.
// Ranges never intersect: claim is called only after ownerOf found no owner.
type ownedRanges struct{ s []ownedRange }

// ownerOf returns the owner of a range intersecting r, unless that owner is
// arriving itself.
func (o *ownedRanges) ownerOf(r Range, arriving articleID) (articleID, bool) {
	i := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.end() > r.Off })
	for ; i < len(o.s) && o.s[i].r.Off < r.end(); i++ {
		if o.s[i].r.intersects(r) && !o.s[i].id.sameArticle(arriving) {
			return o.s[i].id, true
		}
	}
	return articleID{}, false
}

// claim records id as the owner of r, keeping the slice sorted by Off.
func (o *ownedRanges) claim(r Range, id articleID) {
	if r.Len <= 0 {
		return
	}
	i := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.Off >= r.Off })
	if i < len(o.s) && o.s[i].r == r {
		o.s[i].id = id
		return
	}
	o.s = append(o.s, ownedRange{})
	copy(o.s[i+1:], o.s[i:])
	o.s[i] = ownedRange{r: r, id: id}
}

// seededOwner owns ranges verified before this process. Its index matches no
// manifest article, so sameArticle never waves an arrival through.
var seededOwner = articleID{artIdx: -1}

// seed marks rs as owned by seededOwner.
func (o *ownedRanges) seed(rs []Range) {
	for _, r := range rs {
		o.claim(r, seededOwner)
	}
}

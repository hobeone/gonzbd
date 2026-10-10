package assembler

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sort"
)

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

// claim records id as the owner of r, keeping the slice sorted by Off and free
// of intersections: ownerOf's binary search is correct only while end order
// equals offset order.
//
// Entries of the same article that intersect r are replaced by r (a retry may
// write a different length). An intersecting entry of a DIFFERENT article
// panics rather than being refused: the caller must have found no owner via
// ownerOf first, and the one production path, acceptArticle, does so
// immediately before Accept on the file's single goroutine —
// `git grep -n '\.claim(' -- 'internal/assembler/*.go' ':!*_test.go' ':!internal/assembler/diskprobe.go'`
// finds 2 lines, this comment and the one call, in writeOne, reached from
// Accept, whose one non-test caller is acceptArticle.
// A refusal here could not return the article's accounting, since its bytes
// are already written.
func (o *ownedRanges) claim(r Range, id articleID) {
	if r.Len <= 0 {
		return
	}
	lo := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.end() > r.Off })
	hi := lo
	for hi < len(o.s) && o.s[hi].r.Off < r.end() {
		if !o.s[hi].id.sameArticle(id) {
			panic(fmt.Sprintf("assembler: claim of %+v by article %d intersects %+v owned by article %d",
				r, id.artIdx, o.s[hi].r, o.s[hi].id.artIdx))
		}
		hi++
	}
	o.s = slices.Delete(o.s, lo, hi)
	i := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.Off >= r.Off })
	o.s = append(o.s, ownedRange{})
	copy(o.s[i+1:], o.s[i:])
	o.s[i] = ownedRange{r: r, id: id}
}

// seededOwner owns ranges verified before this process. Its index matches no
// manifest article, so sameArticle never waves an arrival through.
var seededOwner = articleID{artIdx: -1}

// errSeedNotEmpty is returned by seed on a set that already holds ranges.
var errSeedNotEmpty = errors.New("assembler: seed on a non-empty range set")

// seed marks rs as owned by seededOwner. It runs once, at file open, before any
// Accept: on a non-empty set it returns errSeedNotEmpty and changes nothing.
//
// seed drops ranges with Len <= 0 and checks nothing else: a negative Off or an
// Off+Len that overflows int64 must be rejected by the caller before seed.
//
// seededOwner stands for many independent verified articles, so seed does not
// go through claim, whose same-article replace would drop earlier coverage.
// Instead it sorts a copy of rs and merges intersecting or abutting ranges.
func (o *ownedRanges) seed(rs []Range) error {
	if len(o.s) != 0 {
		return errSeedNotEmpty
	}
	sorted := slices.DeleteFunc(slices.Clone(rs), func(r Range) bool { return r.Len <= 0 })
	slices.SortFunc(sorted, func(a, b Range) int { return cmp.Compare(a.Off, b.Off) })
	for _, r := range sorted {
		if n := len(o.s); n > 0 && r.Off <= o.s[n-1].r.end() {
			if e := r.end(); e > o.s[n-1].r.end() {
				o.s[n-1].r.Len = e - o.s[n-1].r.Off
			}
			continue
		}
		o.s = append(o.s, ownedRange{r: r, id: seededOwner})
	}
	return nil
}

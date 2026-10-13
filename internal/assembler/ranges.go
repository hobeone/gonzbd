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
// Ranges never intersect, and each of the two writers holds that itself:
// claim panics on any intersection, and seed merges intersecting or abutting
// ranges.
// `git grep -n 'o\.s.* = ' -- 'internal/assembler/*.go' ':!*_test.go'`
// returns 3 lines, one in claim and two in seed.
//
// A range is owned against every arrival, its own owner included, because
// its owner never arrives again: claiming a range resolves the article
// Written, which keeps it in its writer's seenDone, and admit refuses an
// article in seenDone before acceptArticle consults this set.
// TestFileWriter_RangeOwnersStayAdmitted pins that.
type ownedRanges struct{ s []ownedRange }

// ownerOf returns the owner of a range intersecting r.
func (o *ownedRanges) ownerOf(r Range) (articleID, bool) {
	i := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.end() > r.Off })
	for ; i < len(o.s) && o.s[i].r.Off < r.end(); i++ {
		if o.s[i].r.intersects(r) {
			return o.s[i].id, true
		}
	}
	return articleID{}, false
}

// covered returns how many bytes of r lie inside owned ranges. The ranges are
// disjoint, so their intersections with r sum without counting a byte twice.
func (o *ownedRanges) covered(r Range) int64 {
	var n int64
	i := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.end() > r.Off })
	for ; i < len(o.s) && o.s[i].r.Off < r.end(); i++ {
		if lo, hi := max(o.s[i].r.Off, r.Off), min(o.s[i].r.end(), r.end()); hi > lo {
			n += hi - lo
		}
	}
	return n
}

// claim records id as the owner of r, keeping the slice sorted by Off and free
// of intersections: ownerOf's binary search is correct only while end order
// equals offset order.
//
// An intersecting entry panics rather than being refused: the caller must have
// found no owner via ownerOf first, and the one production path,
// acceptArticle, does so immediately before accept on the file's single
// goroutine —
// `git grep -n '\.claim(' -- 'internal/assembler/*.go' ':!*_test.go' ':!internal/assembler/diskprobe.go'`
// finds 2 lines, this comment and the one call, in writeOne, reached from
// accept, whose one non-test caller is acceptArticle.
// A refusal here could not return the article's accounting, since its bytes
// are already written.
func (o *ownedRanges) claim(r Range, id articleID) {
	if r.Len <= 0 {
		return
	}
	lo := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.end() > r.Off })
	if lo < len(o.s) && o.s[lo].r.Off < r.end() {
		panic(fmt.Sprintf("assembler: claim of %+v by article %d intersects %+v owned by article %d",
			r, id.artIdx, o.s[lo].r, o.s[lo].id.artIdx))
	}
	o.s = slices.Insert(o.s, lo, ownedRange{r: r, id: id})
}

// maxEnd returns the largest end over all ranges, accepted and seeded alike, or
// 0 for an empty set. The slice is sorted by Off and free of intersections, so
// its last entry ends last.
func (o *ownedRanges) maxEnd() int64 {
	if len(o.s) == 0 {
		return 0
	}
	return o.s[len(o.s)-1].r.end()
}

// seededOwner owns ranges verified before this process. Its index matches no
// manifest article, so a claim panic names a seeded range as one.
var seededOwner = articleID{artIdx: -1}

// errSeedNotEmpty is returned by seed on a set that already holds ranges.
var errSeedNotEmpty = errors.New("assembler: seed on a non-empty range set")

// seed marks rs as owned by seededOwner. It runs once, at file open, before any
// accept: on a non-empty set it returns errSeedNotEmpty and changes nothing.
//
// seed drops ranges with Len <= 0 and checks nothing else: a negative Off or an
// Off+Len that overflows int64 must be rejected by the caller before seed.
//
// seed does not go through claim, which panics on an intersection. It sorts a
// copy of rs and merges intersecting or abutting ranges.
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

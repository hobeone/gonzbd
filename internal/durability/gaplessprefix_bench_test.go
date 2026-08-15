package durability

import (
	"fmt"
	"testing"
)

// benchFacts builds n offset-ordered, abutting facts for one file, and a
// bitmap marking every one of them durable except the article at hole. Pass a
// hole of -1 for no hole -- Bitmap has no Clear (Set is the package's only bit
// mutation), so the gap has to be built in rather than punched out.
//
// artBytes is a 750 KB article: the yEnc-decoded size of a typical Usenet
// part, so the offsets a real job walks are the ones being walked here.
func benchFacts(n int, hole int) ([]ArticleFact, Bitmap) {
	const artBytes = 750 * 1024
	facts := make([]ArticleFact, n)
	bm := NewBitmap(n)
	for i := range n {
		facts[i] = ArticleFact{
			FileIdx: 0,
			ArtIdx:  int32(i), //nolint:gosec // G115: bounded by the benchmark's n
			Offset:  int64(i) * artBytes,
			Length:  artBytes,
			CRC32:   uint32(i * 2654435761), //nolint:gosec // G115: an arbitrary spread of values
		}
		if i != hole {
			bm.Set(i)
		}
	}
	return facts, bm
}

// BenchmarkGaplessPrefix_WholeFile measures the WORST case for #371's re-walk:
// every article durable and every offset abutting, so the loop never breaks
// and the walk is genuinely O(articles).
//
// This is the shape a file downloading in strict order reaches, and it is the
// only shape where the O(N)-per-checkpoint claim holds. The interesting
// comparison is against the fsyncs the same barrier performs — a checkpoint
// that costs a millisecond of arithmetic beside a syncing device is not a
// finding, and the incremental form is not free: the prefix must restart from
// zero whenever a durable bit is cleared, which Resumer does.
//
// Read crc32util.Combine's doc before optimising anything here. ~89% of the
// figure below is that one call, so the loop is not the lever — and the lever
// that looks obvious there was rejected for depending on article lengths this
// process does not control.
func BenchmarkGaplessPrefix_WholeFile(b *testing.B) {
	for _, n := range []int{100, 1_000, 10_000, 20_000} {
		facts, bm := benchFacts(n, -1)
		tgt := &fakeTarget{artCount: n}
		b.Run(fmt.Sprintf("articles=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				verifiedTo, _ := gaplessPrefix(facts, 0, bm, tgt)
				if verifiedTo == 0 {
					b.Fatal("fixture walked nothing, so this measures the break and not the walk")
				}
			}
		})
	}
}

// BenchmarkGaplessPrefix_StallsAtFirstHole measures the case a real job spends
// almost all of its life in.
//
// Articles do not arrive in order — the dispatcher hands them to whichever
// connection is free — so the gapless prefix stalls at the first article that
// has not landed, and the loop BREAKS there. The walk is O(prefix), not
// O(articles), and #371's O(N × checkpoints) framing only describes the
// benchmark above.
//
// The hole is at article 10 of n, so this is the ratio the cost actually has.
func BenchmarkGaplessPrefix_StallsAtFirstHole(b *testing.B) {
	for _, n := range []int{1_000, 20_000} {
		facts, bm := benchFacts(n, 10)
		tgt := &fakeTarget{artCount: n}
		b.Run(fmt.Sprintf("articles=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				gaplessPrefix(facts, 0, bm, tgt)
			}
		})
	}
}

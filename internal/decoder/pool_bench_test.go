package decoder

import (
	"fmt"
	"testing"
)

// BenchmarkDecodeRecycled decodes an article and returns its buffer to the
// pool, the cycle production runs for every article. The pinned-B/op metric
// is the capacity of the buffer the decode handed out, which is what a
// queued, not-yet-written article holds on the heap.
func BenchmarkDecodeRecycled(b *testing.B) {
	for _, size := range []int{64 * 1024, 512 * 1024, 768000} {
		article := yencEncode("bench.bin", worstCaseRaw(size))
		b.Run(fmt.Sprintf("payload=%d", size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			var pinned int
			for b.Loop() {
				art, err := DecodeArticle(article)
				if err != nil {
					b.Fatal(err)
				}
				pinned = cap(art.Data)
				PutBuffer(art.Data)
			}
			b.ReportMetric(float64(pinned), "pinned-B/op")
		})
	}
}
